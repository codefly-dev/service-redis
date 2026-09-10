package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scriptedRedis answers every connection with the canned frames of reply,
// optionally after reading the client's commands. It stands in for a redis
// server that is broken, still loading, or not redis at all.
type scriptedRedis struct {
	t        *testing.T
	listener net.Listener
	// replies are handed out one connection at a time; the last one repeats.
	replies []string
}

func newScriptedRedis(t *testing.T, replies ...string) *scriptedRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &scriptedRedis{t: t, listener: listener, replies: replies}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *scriptedRedis) address() string { return s.listener.Addr().String() }

func (s *scriptedRedis) serve() {
	for attempt := 0; ; attempt++ {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		reply := s.replies[len(s.replies)-1]
		if attempt < len(s.replies) {
			reply = s.replies[attempt]
		}
		go func() {
			defer conn.Close()
			if reply == "" {
				return
			}
			// Write in two pieces with a pause between them so a reply that
			// arrives fragmented across TCP segments is exercised too.
			middle := len(reply) / 2
			if _, err = io.WriteString(conn, reply[:middle]); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(conn, reply[middle:])
		}()
	}
}

func TestProbeRedisRequiresParsedPong(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		wantOK    bool
		retryable bool
	}{
		{name: "pong", reply: "+PONG\r\n", wantOK: true},
		{name: "noauth", reply: "-NOAUTH Authentication required.\r\n"},
		{name: "loading", reply: "-LOADING Redis is loading the dataset in memory\r\n", retryable: true},
		{name: "busy", reply: "-BUSY Redis is busy running a script\r\n", retryable: true},
		{name: "http", reply: "HTTP/1.1 503 Service Unavailable\r\n\r\n"},
		{name: "empty", reply: "", retryable: true},
		{name: "truncated", reply: "+PON", retryable: true},
		{name: "no crlf", reply: "+PONG\n"},
		{name: "integer", reply: ":1\r\n"},
		{name: "unterminated frame", reply: "+" + strings.Repeat("A", 4096) + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newScriptedRedis(t, tc.reply)
			err := probeRedis(context.Background(), server.address(), "")
			if tc.wantOK {
				if err != nil {
					t.Fatalf("probeRedis: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("probeRedis accepted %q as ready", tc.reply)
			}
			var probeErr *redisProbeError
			if !errors.As(err, &probeErr) {
				t.Fatalf("error is not a *redisProbeError: %v", err)
			}
			if probeErr.retryable != tc.retryable {
				t.Fatalf("retryable = %t, want %t (%v)", probeErr.retryable, tc.retryable, err)
			}
		})
	}
}

// authRedis is the minimum redis that actually checks a password: it parses the
// client's commands and answers AUTH against a fixed secret.
type authRedis struct {
	listener net.Listener
	password string
}

func newAuthRedis(t *testing.T, password string) *authRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &authRedis{listener: listener, password: password}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.handle(conn)
		}
	}()
	return server
}

func (s *authRedis) address() string { return s.listener.Addr().String() }

func (s *authRedis) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	authenticated := s.password == ""
	for {
		command, err := readCommand(reader)
		if err != nil {
			return
		}
		switch {
		case len(command) >= 2 && strings.EqualFold(command[0], "AUTH"):
			if command[len(command)-1] == s.password {
				authenticated = true
				_, _ = io.WriteString(conn, "+OK\r\n")
				continue
			}
			_, _ = io.WriteString(conn, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
		case len(command) == 1 && strings.EqualFold(command[0], "PING"):
			if !authenticated {
				_, _ = io.WriteString(conn, "-NOAUTH Authentication required.\r\n")
				continue
			}
			_, _ = io.WriteString(conn, "+PONG\r\n")
		default:
			_, _ = io.WriteString(conn, "-ERR unknown command\r\n")
		}
	}
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, errors.New("not an array")
	}
	count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, sizeErr := reader.ReadString('\n')
		if sizeErr != nil {
			return nil, sizeErr
		}
		size, sizeErr := strconv.Atoi(strings.TrimSpace(sizeLine[1:]))
		if sizeErr != nil {
			return nil, sizeErr
		}
		payload := make([]byte, size+2)
		if _, err = io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		args = append(args, string(payload[:size]))
	}
	return args, nil
}

func TestProbeRedisAuthenticates(t *testing.T) {
	const password = "correct horse battery staple"
	server := newAuthRedis(t, password)

	if err := probeRedis(context.Background(), server.address(), password); err != nil {
		t.Fatalf("probeRedis with the configured password: %v", err)
	}

	for _, tc := range []struct {
		name     string
		password string
	}{
		{name: "wrong password", password: "hunter2"},
		{name: "missing password", password: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := probeRedis(context.Background(), server.address(), tc.password)
			if err == nil {
				t.Fatal("probeRedis reported ready without valid credentials")
			}
			var probeErr *redisProbeError
			if !errors.As(err, &probeErr) {
				t.Fatalf("error is not a *redisProbeError: %v", err)
			}
			if probeErr.retryable {
				t.Fatalf("credential failure reported as retryable: %v", err)
			}
			if strings.Contains(err.Error(), password) || (tc.password != "" && strings.Contains(err.Error(), tc.password)) {
				t.Fatalf("error leaks credentials: %v", err)
			}
			if !strings.Contains(err.Error(), server.address()) {
				t.Fatalf("error does not name the address: %v", err)
			}
		})
	}
}

func TestProbeRedisIsCancellable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	closed := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		// Never answer: the probe must come back on cancellation, and the client
		// side of this connection must be closed when it does.
		_, _ = io.Copy(io.Discard, conn)
		closed <- struct{}{}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err = probeRedis(ctx, listener.Addr().String(), ""); err == nil {
		t.Fatal("probeRedis succeeded against a server that never answered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probeRedis returned after %s, want the cancellation budget", elapsed)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("probe connection was left open after cancellation")
	}
}

func TestWaitForRedisPongRetriesUntilServerLoads(t *testing.T) {
	server := newScriptedRedis(t,
		"-LOADING Redis is loading the dataset in memory\r\n",
		"-LOADING Redis is loading the dataset in memory\r\n",
		"+PONG\r\n",
	)
	if err := waitForRedisPong(context.Background(), redisWaitOptions{
		address: server.address(),
		budget:  10 * time.Second,
	}); err != nil {
		t.Fatalf("waitForRedisPong: %v", err)
	}
}

func TestWaitForRedisPongStopsOnCredentialFailure(t *testing.T) {
	server := newAuthRedis(t, "the-password")

	start := time.Now()
	err := waitForRedisPong(context.Background(), redisWaitOptions{
		address:  server.address(),
		password: "not-the-password",
		budget:   time.Minute,
	})
	if err == nil {
		t.Fatal("waitForRedisPong reported ready with invalid credentials")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForRedisPong retried a credential failure for %s", elapsed)
	}
	if !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("error is not an authentication diagnostic: %v", err)
	}
}

func TestWaitForRedisPongRespectsBudget(t *testing.T) {
	server := newScriptedRedis(t, "-LOADING Redis is loading the dataset in memory\r\n")

	start := time.Now()
	err := waitForRedisPong(context.Background(), redisWaitOptions{
		address: server.address(),
		budget:  300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("waitForRedisPong reported a permanently loading server as ready")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForRedisPong overran its budget by %s", elapsed)
	}
	if !strings.Contains(err.Error(), "LOADING") {
		t.Fatalf("error does not carry the last actionable probe failure: %v", err)
	}
}
