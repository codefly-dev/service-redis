package main

// redisprobe.go — the readiness handshake shared by both runtimes.
//
// Readiness means "redis answers PING as the credentials we projected into the
// server", not "something on this port sent bytes back". A passworded server
// answers an unauthenticated PING with "-NOAUTH …", a starting one with
// "-LOADING …", and an unrelated service with whatever it speaks; none of those
// prove the agent can run commands, so none of them count as ready.
//
// A read replica is held to more: it answers PING as soon as it starts, long
// before it has the primary's data, so it is ready only once it also reports
// its link to the primary up (INFO replication, master_link_status:up).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	redisProbeDialTimeout = 2 * time.Second
	redisProbeIOTimeout   = 2 * time.Second

	// bufio never grows past this, surfacing anything longer as ErrBufferFull
	// instead, so it is what bounds a hostile or broken peer: a readiness reply
	// is one short frame and the handshake reads at most two.
	redisProbeMaxFrameBytes = 512

	// Liveness costs a forked `ps`, so it runs on a multiple of the probe
	// interval rather than on every attempt.
	redisLivenessCheckEvery = 4

	// Docker publishes the host port as soon as the container is created, well
	// before redis inside it serves, and a cold container start is slower than a
	// local process launch.
	redisDockerReadinessBudget = 60 * time.Second

	// redisProbeMaxBulkBytes bounds the one bulk reply the probe reads, INFO
	// replication, which is a few hundred bytes from a real server.
	redisProbeMaxBulkBytes = 64 << 10
)

// errRedisProtocol marks a reply that is not RESP at all. Unlike a refused dial
// or a reset connection, waiting does not turn it into a redis server.
var errRedisProtocol = errors.New("malformed RESP reply")

// redisProbeError reports a failed handshake. retryable separates a server that
// is still coming up (dial refused, connection reset, "-LOADING") from a verdict
// no amount of waiting changes (bad credentials, a non-redis peer).
type redisProbeError struct {
	phase     string
	address   string
	retryable bool
	err       error
}

func (e *redisProbeError) Error() string {
	return fmt.Sprintf("redis %s at %s: %v", e.phase, e.address, e.err)
}

func (e *redisProbeError) Unwrap() error { return e.err }

type redisWaitOptions struct {
	address  string
	password string
	budget   time.Duration
	// replica requires the server to be a replica whose link to its primary
	// is up, on top of the authenticated PING.
	replica bool

	// onAttemptFailed, when set, receives every failed probe. Without it a wait
	// is silent for its whole budget, which is the failure users actually watch.
	onAttemptFailed func(error)
}

// waitForRedisPong probes until redis completes an authenticated PING, the
// budget expires, or the caller cancels. Errors carry the phase and address but
// never the credentials.
func waitForRedisPong(ctx context.Context, opts redisWaitOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.budget)
	defer cancel()

	var last error
	for {
		probe := probeRedis
		if opts.replica {
			probe = probeRedisReplica
		}
		err := probe(ctx, opts.address, opts.password)
		if err == nil {
			return nil
		}
		last = err
		if opts.onAttemptFailed != nil {
			opts.onAttemptFailed(err)
		}

		var probeErr *redisProbeError
		if errors.As(err, &probeErr) && !probeErr.retryable {
			return err
		}

		timer := time.NewTimer(redisProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("redis at %s did not become ready (%v): %w", opts.address, ctx.Err(), last)
		case <-timer.C:
		}
	}
}

// probeRedis runs one handshake: authenticate when a password is configured,
// then require a parsed "+PONG".
func probeRedis(ctx context.Context, address string, password string) error {
	return withRedisHandshake(ctx, address, password, nil)
}

// probeRedisReplica runs the handshake, then requires the server to be a
// replica with its link to the primary up. A replica still syncing is
// retryable; a server that is not a replica at all is not.
func probeRedisReplica(ctx context.Context, address string, password string) error {
	return withRedisHandshake(ctx, address, password, func(conn net.Conn, reader *bufio.Reader) error {
		info, err := redisInfo(ctx, conn, reader, address, "replication")
		if err != nil {
			return err
		}
		if role := info["role"]; role != "slave" {
			return &redisProbeError{phase: "replication", address: address, err: fmt.Errorf("server role is %q, want a replica", role)}
		}
		if link := info["master_link_status"]; link != "up" {
			return &redisProbeError{phase: "replication", address: address, retryable: true, err: fmt.Errorf("link to the primary is %q", link)}
		}
		return nil
	})
}

// withRedisHandshake authenticates and PINGs, then runs then on the same
// connection when it is set.
func withRedisHandshake(ctx context.Context, address string, password string, then func(net.Conn, *bufio.Reader) error) error {
	dialer := &net.Dialer{Timeout: redisProbeDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return &redisProbeError{phase: "dial", address: address, retryable: true, err: err}
	}
	defer conn.Close()

	// Closing the socket on cancellation is what makes an in-flight read
	// interruptible: a deadline alone would hold the caller until it expires.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	reader := bufio.NewReaderSize(conn, redisProbeMaxFrameBytes)

	if password != "" {
		if err = redisExchange(ctx, conn, reader, "authentication", address, "OK", "AUTH", password); err != nil {
			return err
		}
	}
	if err = redisExchange(ctx, conn, reader, "ping", address, "PONG", "PING"); err != nil || then == nil {
		return err
	}
	return then(conn, reader)
}

// redisInfo reads one INFO section as its field:value pairs.
func redisInfo(ctx context.Context, conn net.Conn, reader *bufio.Reader, address string, section string) (map[string]string, error) {
	if err := writeRedisCommand(ctx, conn, "INFO", section); err != nil {
		return nil, &redisProbeError{phase: "info", address: address, retryable: true, err: err}
	}
	body, err := readRedisBulk(ctx, conn, reader)
	var refused *redisReplyError
	if errors.As(err, &refused) {
		return nil, &redisProbeError{phase: "info", address: address, retryable: isRetryableRedisError(refused.message), err: err}
	}
	if err != nil {
		return nil, &redisProbeError{phase: "info", address: address, retryable: !errors.Is(err, errRedisProtocol), err: err}
	}
	info := map[string]string{}
	for _, line := range strings.Split(body, "\r\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, "#") {
			info[key] = value
		}
	}
	return info, nil
}

// redisReplyError is an error frame: the server refusing the command.
type redisReplyError struct{ message string }

func (e *redisReplyError) Error() string { return fmt.Sprintf("server replied %q", e.message) }

// readRedisBulk reads one bulk-string reply. An error frame is reported as the
// server's refusal, anything else as a peer that is not the redis we started.
func readRedisBulk(ctx context.Context, conn net.Conn, reader *bufio.Reader) (string, error) {
	if err := conn.SetReadDeadline(redisIODeadline(ctx)); err != nil {
		return "", err
	}
	header, err := readRedisLine(reader)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(header, "-") {
		return "", &redisReplyError{message: header[1:]}
	}
	if !strings.HasPrefix(header, "$") {
		return "", fmt.Errorf("%w: unexpected frame %q, want a bulk string", errRedisProtocol, header)
	}
	size, err := strconv.Atoi(header[1:])
	if err != nil || size < 0 || size > redisProbeMaxBulkBytes {
		return "", fmt.Errorf("%w: bulk length %q", errRedisProtocol, header[1:])
	}
	body := make([]byte, size+2)
	if _, err = io.ReadFull(reader, body); err != nil {
		return "", err
	}
	if string(body[size:]) != "\r\n" {
		return "", fmt.Errorf("%w: bulk string not CRLF terminated", errRedisProtocol)
	}
	return string(body[:size]), nil
}

func redisExchange(ctx context.Context, conn net.Conn, reader *bufio.Reader, phase string, address string, want string, args ...string) error {
	if err := writeRedisCommand(ctx, conn, args...); err != nil {
		return &redisProbeError{phase: phase, address: address, retryable: true, err: err}
	}
	kind, payload, err := readRedisReply(ctx, conn, reader)
	if err != nil {
		return &redisProbeError{phase: phase, address: address, retryable: !errors.Is(err, errRedisProtocol), err: err}
	}
	if kind == '-' {
		return &redisProbeError{phase: phase, address: address, retryable: isRetryableRedisError(payload), err: fmt.Errorf("server replied %q", payload)}
	}
	if payload != want {
		return &redisProbeError{phase: phase, address: address, err: fmt.Errorf("unexpected reply %q, want %q", payload, want)}
	}
	return nil
}

// isRetryableRedisError reports whether an error frame describes a server that
// is temporarily unable to serve rather than one refusing us outright.
func isRetryableRedisError(message string) bool {
	code, _, _ := strings.Cut(message, " ")
	switch code {
	case "LOADING", "BUSY", "MASTERDOWN", "CLUSTERDOWN", "TRYAGAIN":
		return true
	}
	return false
}

func writeRedisCommand(ctx context.Context, conn net.Conn, args ...string) error {
	if err := conn.SetWriteDeadline(redisIODeadline(ctx)); err != nil {
		return err
	}
	var command bytes.Buffer
	fmt.Fprintf(&command, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&command, "$%d\r\n%s\r\n", len(arg), arg)
	}
	_, err := conn.Write(command.Bytes())
	return err
}

// readRedisReply reads one complete RESP frame, however the peer fragmented it
// across TCP segments. AUTH and PING only ever answer with a simple string or an
// error, so any other frame type is a peer that is not the redis we started.
func readRedisReply(ctx context.Context, conn net.Conn, reader *bufio.Reader) (byte, string, error) {
	if err := conn.SetReadDeadline(redisIODeadline(ctx)); err != nil {
		return 0, "", err
	}
	line, err := readRedisLine(reader)
	if err != nil {
		return 0, "", err
	}
	if line == "" {
		return 0, "", fmt.Errorf("%w: empty frame", errRedisProtocol)
	}
	switch kind := line[0]; kind {
	case '+', '-':
		return kind, line[1:], nil
	default:
		return 0, "", fmt.Errorf("%w: unexpected frame type %q", errRedisProtocol, string(kind))
	}
}

func readRedisLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("%w: frame exceeds %d bytes", errRedisProtocol, redisProbeMaxFrameBytes)
		}
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", fmt.Errorf("%w: frame not CRLF terminated", errRedisProtocol)
	}
	return string(line[:len(line)-2]), nil
}

func redisIODeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(redisProbeIOTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		return ctxDeadline
	}
	return deadline
}
