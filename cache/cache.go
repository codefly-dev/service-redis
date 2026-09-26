// Package cache is service-redis's implementation of the codefly.dev/cache
// interface (github.com/codefly-dev/interface-cache): a cache layer for a
// server speaking the Redis protocol. It is a Leaser, so a miss reaches the
// origin once across every process sharing the server, and a Notifier on
// Redis client tracking (tracking.go), so in-process layers above it drop a key
// that changed, whoever changed it. Tracking lives in a connection, so it is
// also a Resyncer: when that connection drops, the layers above are flushed.
//
// Importing it registers the "redis" driver with cache.Open. The connection is
// the redis:// URL service-redis emits in its "cache" configuration group.
// Consumers usually import it under a name:
//
//	import rediscache "github.com/codefly-dev/service-redis/cache"
//
// It is its own Go module, so importing it does not pull in the agent.
//
// # Change notices
//
// The Notifier is Redis server-assisted client tracking, in broadcast mode
// for the prefix of the layer's value keys (tracking.go). It reports every
// change by any client: a write through another stack, a plain SET or DEL from
// some other program, expiry and eviction, and this layer's own writes, which
// the interface allows. An echo of a stack's own write can evict the copy that
// write just stored in the tiers above; that costs one extra read, never a
// stale value. Tracking lives in a connection, so a dropped connection or a
// flush is reported through SubscribeResync, and the stack flushes the tiers
// above. The tracking connection is named TrackingClientName in CLIENT LIST.
//
// A Delete of a key the server does not hold is announced too: a process can
// hold a copy that the shared layer does not (one filled while this tier was
// skipped), and the delete must still reach it.
//
// With a goredis.ClusterClient the layer tracks every primary, and lists them
// again every few seconds: a change of primaries is a gap, so it resyncs.
// Reads the ClusterClient routes to cluster replicas are not covered by the
// ordering WithReplica gives.
//
// # What has been run
//
// service-redis's TestRealRedisCache* runs the interface's conformance suite,
// including the external-writer and resync cases, against:
//
//   - Redis as service-redis's own Runtime runs it, the pinned image under
//     Docker and the nix-provisioned server, single-process and as a primary
//     with read replicas, and a layer reading through a replica;
//   - Valkey 9.1.2;
//   - a local Redis Cluster of three primaries, through New with a
//     goredis.ClusterClient. Keys for one cache key share a hash tag, so the
//     two-key scripts stay in one slot. Open only builds single-server
//     clients: a cluster connection string is not supported.
//
// Not run: TLS (rediss://), which go-redis parses and the tracking connection
// dials through the client's own dialer, but which no test has served; and
// managed offerings such as ElastiCache or Memorystore, which CI cannot reach
// and some of which restrict CLIENT TRACKING. Nothing here claims they work.
//
// # Read replicas
//
// service-redis's cache group always names the primary, so a layer opened from
// it reads and writes there. WithReplica reads through a replica instead; see
// it for what that costs.
package cache

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	cache "github.com/codefly-dev/interface-cache/go/cache"
)

// DriverName is the name the driver registers with cache.Open.
const DriverName = "redis"

func init() {
	cache.Register(DriverName, Open)
}

// Open builds a Layer from a provider's configuration. It does not contact the
// server: an unreachable cache degrades reads, it does not stop a consumer
// from starting. Client timeouts and retries are tuned in the URL, e.g.
// ?dial_timeout=200ms&max_retries=-1 (in go-redis, 0 means the default of 3
// retries; -1 disables them).
func Open(_ context.Context, cfg cache.Config) (cache.Layer, error) {
	opts, err := goredis.ParseURL(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("redis cache: invalid connection: %w", err)
	}
	return New(goredis.NewClient(opts)), nil
}

// Layer is a cache.Layer over one Redis keyspace.
type Layer struct {
	client goredis.UniversalClient
	// reader serves Get and is where change notices are tracked: client, or a
	// replica given WithReplica.
	reader goredis.UniversalClient
	prefix string
	id     string // tells this layer's tracking connections from others'

	trackMu sync.Mutex
	tracker *tracker // running while anyone listens; guarded by trackMu
}

var (
	_ cache.Leaser   = (*Layer)(nil)
	_ cache.Notifier = (*Layer)(nil)
)

// replicaWait bounds how long a write through a WithReplica layer waits for the
// replicas to have it.
const replicaWait = time.Second

// Option configures a Layer.
type Option func(*Layer)

// WithPrefix namespaces every key the layer writes, so several caches can
// share one server. Default "cache".
func WithPrefix(prefix string) Option { return func(l *Layer) { l.prefix = prefix } }

// WithReplica reads through replica, a read replica of the client New is given:
// Get is served by it, and change notices are tracked there. Sets, deletes,
// leases and fills still go to the primary, which is the only server that
// accepts them.
//
// Notices come from the replica because a replica applies the primary's writes
// in order and reports a change to its tracking clients once it has applied
// it: re-reading on a notice never finds the old value there. A notice from the
// primary could arrive first, be followed by a re-read from a replica that has
// not caught up, and leave that stale value in the layers above.
//
// Replication is asynchronous, so each of the layer's own value writes (Set,
// Delete, Fill) is followed by WAIT for every replica the primary has
// connected at that moment, for at most a second, on the connection that made
// the write. The layer then reads its own writes through any of them. That
// costs two round trips per write, and a replica that falls further behind
// than that is not waited for past the second.
func WithReplica(replica goredis.UniversalClient) Option {
	return func(l *Layer) { l.reader = replica }
}

// New returns a Layer using client. The caller owns client.
func New(client goredis.UniversalClient, opts ...Option) *Layer {
	l := &Layer{client: client, prefix: "cache", id: randomToken()[:8]}
	for _, opt := range opts {
		opt(l)
	}
	if l.reader == nil {
		l.reader = l.client
	}
	return l
}

// Keys for one cache key share a hash tag, so they live in one slot and the
// scripts below stay valid on Redis Cluster.
func (l *Layer) valueKey(key string) string { return l.prefix + ":v:{" + key + "}" }
func (l *Layer) leaseKey(key string) string { return l.prefix + ":l:{" + key + "}" }

// Get implements cache.Layer.
func (l *Layer) Get(ctx context.Context, key string) (cache.Entry, error) {
	raw, err := l.reader.Get(ctx, l.valueKey(key)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return cache.Entry{}, cache.ErrMiss
	}
	if err != nil {
		return cache.Entry{}, err
	}
	return decode(raw)
}

// write runs a script that changes a value key on the primary. Reading
// through a replica, it then waits on the same connection until every
// connected replica has the write (see WithReplica).
func (l *Layer) write(ctx context.Context, script *goredis.Script, keys []string, args ...any) *goredis.Cmd {
	if l.reader == l.client {
		return script.Run(ctx, l.client, keys, args...)
	}
	replicas, err := l.connectedReplicas(ctx)
	if err != nil || replicas == 0 {
		cmd := script.Run(ctx, l.client, keys, args...)
		if err != nil && cmd.Err() == nil {
			cmd.SetErr(err)
		}
		return cmd
	}
	// WAIT counts the writes of the connection it runs on: a pipeline keeps
	// both on one.
	pipe := l.client.Pipeline()
	cmd := script.Eval(ctx, pipe, keys, args...)
	pipe.Do(ctx, "WAIT", replicas, replicaWait.Milliseconds())
	_, _ = pipe.Exec(ctx)
	return cmd
}

// connectedReplicas is how many replicas the primary has now.
func (l *Layer) connectedReplicas(ctx context.Context) (int, error) {
	info, err := l.client.Info(ctx, "replication").Result()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(info, "\r\n") {
		if n, ok := strings.CutPrefix(line, "connected_slaves:"); ok {
			return strconv.Atoi(n)
		}
	}
	return 0, nil
}

// setScript stores a value and revokes any fill lease on it: a lease taken
// before this write may be about to store an older value. Tracking tells every
// other client.
var setScript = goredis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('DEL', KEYS[2])
return 1`)

// Set implements cache.Layer.
func (l *Layer) Set(ctx context.Context, key string, e cache.Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return l.Delete(ctx, key)
	}
	return l.write(ctx, setScript,
		[]string{l.valueKey(key), l.leaseKey(key)},
		encode(e), milliseconds(ttl)).Err()
}

// deleteScript removes a value and its fill lease. Tracking only reports keys
// that actually change, and deleting a value the server does not hold changes
// nothing, yet a process can still hold a copy of it above this layer (one
// filled while this tier was skipped, say). So a delete that finds nothing
// sets and removes the key within the script: no client can observe it, and
// every tracking client hears that the key changed.
var deleteScript = goredis.NewScript(`
if redis.call('DEL', KEYS[1]) == 0 then
  redis.call('SET', KEYS[1], '', 'PX', 1)
  redis.call('DEL', KEYS[1])
end
redis.call('DEL', KEYS[2])
return 1`)

// Delete implements cache.Layer. It revokes any fill lease on key, and is
// reported to every Notifier subscriber whether or not the server held key.
func (l *Layer) Delete(ctx context.Context, key string) error {
	return l.write(ctx, deleteScript, []string{l.valueKey(key), l.leaseKey(key)}).Err()
}

// Acquire implements cache.Leaser.
func (l *Layer) Acquire(ctx context.Context, key string, ttl time.Duration) (cache.Lease, bool, error) {
	token := randomToken()
	ok, err := l.client.SetNX(ctx, l.leaseKey(key), token, ttl).Result()
	if err != nil || !ok {
		return cache.Lease{}, false, err
	}
	return cache.Lease{Key: key, Token: token}, true, nil
}

// fillScript stores a value only while the caller still holds the lease. A
// Set, a Delete or expiry removes the lease key, so a fill that raced any of
// them stores nothing.
var fillScript = goredis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
redis.call('DEL', KEYS[2])
return 1`)

// Fill implements cache.Leaser.
func (l *Layer) Fill(ctx context.Context, lease cache.Lease, e cache.Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return l.Release(ctx, lease)
	}
	stored, err := l.write(ctx, fillScript,
		[]string{l.valueKey(lease.Key), l.leaseKey(lease.Key)},
		lease.Token, encode(e), milliseconds(ttl)).Int()
	if err != nil {
		return err
	}
	if stored == 0 {
		return cache.ErrLeaseLost
	}
	return nil
}

var releaseScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('DEL', KEYS[1]) end
return 1`)

// Release implements cache.Leaser. Releasing a lease someone else now holds
// leaves theirs alone.
func (l *Layer) Release(ctx context.Context, lease cache.Lease) error {
	return releaseScript.Run(ctx, l.client, []string{l.leaseKey(lease.Key)}, lease.Token).Err()
}

// Entries are stored as: format byte, flags byte, FreshUntil as Unix
// nanoseconds (0 = none), the version length as a uvarint, the version, then
// the value bytes.
const (
	formatV1    = 1
	flagMissing = 1 << 0
)

func encode(e cache.Entry) []byte {
	buf := make([]byte, 0, 2+8+binary.MaxVarintLen64+len(e.Version)+len(e.Value))
	var flags byte
	if e.Missing {
		flags |= flagMissing
	}
	buf = append(buf, formatV1, flags)
	var fresh int64
	if !e.FreshUntil.IsZero() {
		fresh = e.FreshUntil.UnixNano()
	}
	buf = binary.BigEndian.AppendUint64(buf, uint64(fresh))
	buf = binary.AppendUvarint(buf, uint64(len(e.Version)))
	buf = append(buf, e.Version...)
	return append(buf, e.Value...)
}

func decode(raw []byte) (cache.Entry, error) {
	if len(raw) < 10 || raw[0] != formatV1 {
		return cache.Entry{}, fmt.Errorf("redis cache: entry in an unknown format (%d bytes)", len(raw))
	}
	e := cache.Entry{Missing: raw[1]&flagMissing != 0}
	if fresh := int64(binary.BigEndian.Uint64(raw[2:10])); fresh != 0 {
		e.FreshUntil = time.Unix(0, fresh)
	}
	n, size := binary.Uvarint(raw[10:])
	if size <= 0 || uint64(len(raw)-10-size) < n {
		return cache.Entry{}, errors.New("redis cache: truncated entry")
	}
	rest := raw[10+size:]
	e.Version = string(rest[:n])
	if !e.Missing {
		e.Value = append([]byte{}, rest[n:]...)
	}
	return e, nil
}

func milliseconds(d time.Duration) int64 {
	return max(d.Milliseconds(), 1)
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}
