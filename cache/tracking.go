package cache

// tracking.go — the Notifier, on Redis server-assisted client tracking.
//
// The layer keeps one RESP3 connection per server that holds its keys (one
// server, or every primary of a cluster) and turns tracking on there in
// broadcast mode for the prefix of its value keys:
//
//	HELLO 3 [AUTH user pass] SETNAME <TrackingClientName(prefix)>
//	CLIENT TRACKING ON BCAST PREFIX <prefix>:v:
//
// The server then pushes an invalidation for every change to a key under that
// prefix, by any client, including expiry and eviction. That is the contract's
// "changed by any writer", which pub/sub could not give: a notice only existed
// if our own code published one.
//
// Tracking state lives in the connection and dies with it. So the connection
// is kept alive (PING, with a read deadline), and when it drops, or a flush
// invalidates everything at once, the layer reconnects and signals a resync:
// the stack then flushes the layers above instead of trusting them.
//
// The connection is the driver's own, not one of go-redis's pooled
// connections: a pooled connection only reads pushes when a command happens to
// run on it, and an idle subscriber would never hear anything.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	cache "github.com/codefly-dev/interface-cache/go/cache"
)

const (
	// trackingPingEvery keeps an idle tracking connection proven alive; a
	// connection that answers nothing for three intervals is a gap.
	trackingPingEvery = 2 * time.Second
	// trackingTopologyEvery is how often a cluster's primaries are listed
	// again: a primary the layer does not track is a gap.
	trackingTopologyEvery = 5 * time.Second
	trackingRetryMax      = 2 * time.Second
)

var _ cache.Resyncer = (*Layer)(nil)

// TrackingClientName is the client name of this layer's tracking
// connections, as CLIENT LIST shows them: codefly-cache:<prefix>:<layer id>.
func (l *Layer) TrackingClientName() string {
	name := []rune("codefly-cache:" + l.prefix + ":" + l.id)
	for i, r := range name {
		if r <= ' ' || r > '~' {
			name[i] = '_'
		}
	}
	return string(name)
}

// Subscribe implements cache.Notifier. It reports every change to one of the
// layer's keys, by any writer, including this layer's own client.
func (l *Layer) Subscribe(ctx context.Context, fn func(key string)) (func(), error) {
	return l.listen(ctx, listener{key: fn})
}

// SubscribeResync implements cache.Resyncer. fn runs once tracking is back
// after its connection dropped, and when the server invalidates every key at
// once (FLUSHDB, FLUSHALL).
func (l *Layer) SubscribeResync(ctx context.Context, fn func()) (func(), error) {
	return l.listen(ctx, listener{resync: fn})
}

type listener struct {
	key    func(string)
	resync func()
}

// tracker is a layer's tracking connections, shared by all its listeners and
// running while there is at least one.
type tracker struct {
	layer     *Layer
	listeners map[int]listener
	next      int
	stop      context.CancelFunc
	done      chan struct{}
}

func (l *Layer) listen(ctx context.Context, fn listener) (func(), error) {
	l.trackMu.Lock()
	defer l.trackMu.Unlock()
	if l.tracker == nil {
		conns, err := l.connectTracking(ctx)
		if err != nil {
			return nil, err
		}
		runCtx, stop := context.WithCancel(context.Background())
		l.tracker = &tracker{layer: l, listeners: map[int]listener{}, stop: stop, done: make(chan struct{})}
		go l.tracker.run(runCtx, conns)
	}
	tr := l.tracker
	id := tr.next
	tr.next++
	tr.listeners[id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			l.trackMu.Lock()
			delete(tr.listeners, id)
			last := len(tr.listeners) == 0 && l.tracker == tr
			if last {
				l.tracker = nil
			}
			l.trackMu.Unlock()
			if last {
				tr.stop()
				<-tr.done
			}
		})
	}, nil
}

// snapshot is the listeners at this moment, called without the lock held.
func (tr *tracker) snapshot() []listener {
	tr.layer.trackMu.Lock()
	defer tr.layer.trackMu.Unlock()
	out := make([]listener, 0, len(tr.listeners))
	for _, fn := range tr.listeners {
		out = append(out, fn)
	}
	return out
}

func (tr *tracker) changed(key string) {
	for _, fn := range tr.snapshot() {
		if fn.key != nil {
			fn.key(key)
		}
	}
}

func (tr *tracker) resync() {
	for _, fn := range tr.snapshot() {
		if fn.resync != nil {
			fn.resync()
		}
	}
}

// run serves conns until they fail, then reconnects and signals the gap, until
// stopped.
func (tr *tracker) run(ctx context.Context, conns []*trackingConn) {
	defer close(tr.done)
	for {
		tr.serve(ctx, conns)
		for _, c := range conns {
			_ = c.conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		backoff := 50 * time.Millisecond
		for {
			var err error
			if conns, err = tr.layer.connectTracking(ctx); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, trackingRetryMax)
		}
		// Tracking is on again: anything changed while it was off went
		// unreported.
		tr.resync()
	}
}

// serve reads every connection until one fails, the cluster's primaries
// change, or ctx ends.
func (tr *tracker) serve(ctx context.Context, conns []*trackingConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(2)
		go func() { defer wg.Done(); defer cancel(); tr.read(c) }()
		go func() { defer wg.Done(); defer cancel(); c.ping(ctx) }()
	}
	if cluster, ok := tr.layer.reader.(*goredis.ClusterClient); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			watchPrimaries(ctx, cluster, conns)
		}()
	}
	<-ctx.Done()
	for _, c := range conns {
		_ = c.conn.Close() // unblocks the readers
	}
	wg.Wait()
}

// read handles pushes until the connection fails.
func (tr *tracker) read(c *trackingConn) {
	valuePrefix := tr.layer.prefix + ":v:{"
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * trackingPingEvery))
		v, err := readRESP(c.reader)
		if err != nil {
			return
		}
		if v.kind != '>' || len(v.elems) < 2 || v.elems[0].str != "invalidate" {
			continue // PONG, or a push this layer has no use for
		}
		keys := v.elems[1]
		if keys.null {
			// Every key at once: a flush.
			tr.resync()
			continue
		}
		for _, k := range keys.elems {
			if key, ok := strings.CutPrefix(k.str, valuePrefix); ok && strings.HasSuffix(key, "}") {
				tr.changed(key[:len(key)-1])
			}
		}
	}
}

// trackingConn is one server's tracking connection.
type trackingConn struct {
	addr   string
	conn   net.Conn
	reader *bufio.Reader
}

func (c *trackingConn) ping(ctx context.Context) {
	ticker := time.NewTicker(trackingPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(trackingPingEvery))
			if _, err := c.conn.Write(respCommand("PING")); err != nil {
				return
			}
		}
	}
}

// watchPrimaries returns when the cluster's primaries are no longer the ones
// conns track.
func watchPrimaries(ctx context.Context, cluster *goredis.ClusterClient, conns []*trackingConn) {
	tracked := make([]string, 0, len(conns))
	for _, c := range conns {
		tracked = append(tracked, c.addr)
	}
	slices.Sort(tracked)
	ticker := time.NewTicker(trackingTopologyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cluster.ReloadState(ctx)
		nodes, err := trackedServers(ctx, cluster)
		if err != nil {
			continue
		}
		current := make([]string, 0, len(nodes))
		for _, n := range nodes {
			current = append(current, n.Addr)
		}
		slices.Sort(current)
		if !slices.Equal(current, tracked) {
			return
		}
	}
}

// trackedServers are the servers whose changes the layer must hear: the one
// it reads, or every primary of a cluster.
func trackedServers(ctx context.Context, reader goredis.UniversalClient) ([]*goredis.Options, error) {
	switch c := reader.(type) {
	case *goredis.Client:
		return []*goredis.Options{c.Options()}, nil
	case *goredis.ClusterClient:
		var mu sync.Mutex
		var nodes []*goredis.Options
		err := c.ForEachMaster(ctx, func(_ context.Context, node *goredis.Client) error {
			mu.Lock()
			defer mu.Unlock()
			nodes = append(nodes, node.Options())
			return nil
		})
		return nodes, err
	}
	return nil, fmt.Errorf("redis cache: change notices need a *redis.Client or a *redis.ClusterClient, not %T", reader)
}

// connectTracking opens a tracking connection to every server the layer
// reads, and fails unless tracking is on for all of them.
func (l *Layer) connectTracking(ctx context.Context) ([]*trackingConn, error) {
	servers, err := trackedServers(ctx, l.reader)
	if err != nil {
		return nil, err
	}
	conns := make([]*trackingConn, 0, len(servers))
	for _, opts := range servers {
		c, err := dialTracking(ctx, opts, l.TrackingClientName(), l.prefix+":v:")
		if err != nil {
			for _, open := range conns {
				_ = open.conn.Close()
			}
			return nil, fmt.Errorf("redis cache: track changes on %s: %w", opts.Addr, err)
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// dialTracking connects with the client's own dialer (TLS included), speaks
// RESP3, authenticates as the client does and turns broadcast tracking on for
// prefix.
func dialTracking(ctx context.Context, opts *goredis.Options, name, prefix string) (*trackingConn, error) {
	dial := opts.Dialer
	if dial == nil {
		dial = (&net.Dialer{Timeout: opts.DialTimeout}).DialContext
	}
	network := opts.Network
	if network == "" {
		network = "tcp"
	}
	conn, err := dial(ctx, network, opts.Addr)
	if err != nil {
		return nil, err
	}
	c := &trackingConn{addr: opts.Addr, conn: conn, reader: bufio.NewReader(conn)}
	if err = c.setup(ctx, opts, name, prefix); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *trackingConn) setup(ctx context.Context, opts *goredis.Options, name, prefix string) error {
	username, password := opts.Username, opts.Password
	switch {
	case opts.CredentialsProviderContext != nil:
		var err error
		if username, password, err = opts.CredentialsProviderContext(ctx); err != nil {
			return err
		}
	case opts.CredentialsProvider != nil:
		username, password = opts.CredentialsProvider()
	}
	hello := []string{"HELLO", "3"}
	if password != "" {
		if username == "" {
			username = "default"
		}
		hello = append(hello, "AUTH", username, password)
	}
	hello = append(hello, "SETNAME", name)
	deadline := time.Now().Add(max(opts.ReadTimeout, 3*time.Second))
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	for _, command := range [][]string{hello, {"CLIENT", "TRACKING", "ON", "BCAST", "PREFIX", prefix}} {
		if _, err := c.conn.Write(respCommand(command...)); err != nil {
			return err
		}
		reply, err := readRESP(c.reader)
		if err != nil {
			return err
		}
		if reply.kind == '-' || reply.kind == '!' {
			return fmt.Errorf("%s: %s", command[0], reply.str)
		}
	}
	return nil
}

func respCommand(args ...string) []byte {
	b := []byte("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b = append(b, "$"+strconv.Itoa(len(a))+"\r\n"+a+"\r\n"...)
	}
	return b
}

// respValue is one RESP3 value, as much of it as tracking needs.
type respValue struct {
	kind  byte
	str   string
	elems []respValue
	null  bool
}

var errRESP = errors.New("redis cache: malformed RESP3 reply")

// readRESP reads one complete RESP3 value.
func readRESP(r *bufio.Reader) (respValue, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return respValue{}, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return respValue{}, errRESP
	}
	kind, body := line[0], line[1:len(line)-2]
	v := respValue{kind: kind}
	switch kind {
	case '+', '-', ':', ',', '(', '#':
		v.str = body
	case '_':
		v.null = true
	case '$', '!', '=':
		n, err := strconv.Atoi(body)
		if err != nil {
			return v, errRESP
		}
		if n < 0 {
			v.null = true
			return v, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return v, err
		}
		v.str = string(buf[:n])
	case '*', '>', '~', '%', '|':
		n, err := strconv.Atoi(body)
		if err != nil {
			return v, errRESP
		}
		if n < 0 {
			v.null = true
			return v, nil
		}
		if kind == '%' || kind == '|' {
			n *= 2
		}
		for range n {
			e, err := readRESP(r)
			if err != nil {
				return v, err
			}
			v.elems = append(v.elems, e)
		}
		if kind == '|' {
			// An attribute annotates the value that follows it.
			return readRESP(r)
		}
	default:
		return v, errRESP
	}
	return v, nil
}
