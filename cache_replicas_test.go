package main

// cache_replicas_test.go — codefly.dev/cache over a primary with read replicas,
// as the Runtime runs them. The emitted cache group names the primary, so every
// cache operation lands there; a layer pointed at a replica has its writes
// refused by Redis; a layer that reads through a replica is invalidated by the
// primary's writes; and replication lag is measured, not assumed.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
	"github.com/codefly-dev/interface-cache/go/cache/cachetest"
	rediscache "github.com/codefly-dev/service-redis/cache"
	goredis "github.com/redis/go-redis/v9"
)

func runReplicaCache(t *testing.T, r *runtimeRedis) {
	conf := r.consumer(t)
	primaryConnection, err := conf.Secret(cacheiface.Group, cacheiface.KeyConnection)
	if err != nil {
		t.Fatal(err)
	}
	readConnection, err := conf.Secret("redis", readConnectionKey)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("WritesReachThePrimary", func(t *testing.T) {
		ctx := context.Background()
		if got, want := connectionAddress(t, primaryConnection), r.address(t, r.write); got != want {
			t.Fatalf("cache.connection points at %s, want the primary behind %s", got, want)
		}
		ns := cachetest.Namespace(t)
		layer := rediscache.New(redisClient(t, primaryConnection), rediscache.WithPrefix(ns))
		mustCache(t, layer.Set(ctx, "set", cacheiface.Entry{Value: []byte("v")}, time.Minute))
		lease, held, err := layer.Acquire(ctx, "filled", time.Minute)
		if err != nil || !held {
			t.Fatalf("Acquire on the primary = %v, %v", held, err)
		}
		mustCache(t, layer.Fill(ctx, lease, cacheiface.Entry{Value: []byte("f")}, time.Minute))
		mustCache(t, layer.Delete(ctx, "set"))

		primary := redisClient(t, primaryConnection)
		if role := replicationInfoOf(t, primary)["role"]; role != "master" {
			t.Fatalf("the server cache.connection reaches is a %q, want the primary", role)
		}
		if n, err := primary.Exists(ctx, ns+":v:{filled}").Result(); err != nil || n != 1 {
			t.Fatalf("the fill is not on the primary: %d, %v", n, err)
		}
		if n, err := primary.Exists(ctx, ns+":v:{set}", ns+":l:{filled}").Result(); err != nil || n != 0 {
			t.Fatalf("the delete or the lease release did not reach the primary: %d keys left, %v", n, err)
		}
	})

	t.Run("WritesToAReplicaAreRefused", func(t *testing.T) {
		ctx := context.Background()
		ns := cachetest.Namespace(t)
		layer := rediscache.New(redisClient(t, readConnection), rediscache.WithPrefix(ns))
		_, _, acquireErr := layer.Acquire(ctx, "k", time.Minute)
		// A fill checks its lease before it writes, so it needs a real one,
		// taken on the primary and replicated, to reach the write at all.
		lease, held, err := rediscache.New(redisClient(t, primaryConnection), rediscache.WithPrefix(ns)).Acquire(ctx, "filled", time.Minute)
		if err != nil || !held {
			t.Fatalf("Acquire on the primary = %v, %v", held, err)
		}
		replica := redisClient(t, readConnection)
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if n, _ := replica.Exists(ctx, ns+":l:{filled}").Result(); n == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the lease never reached the replica")
			}
		}
		for name, err := range map[string]error{
			"Set":     layer.Set(ctx, "k", cacheiface.Entry{Value: []byte("v")}, time.Minute),
			"Delete":  layer.Delete(ctx, "k"),
			"Acquire": acquireErr,
			"Fill":    layer.Fill(ctx, lease, cacheiface.Entry{Value: []byte("v")}, time.Minute),
		} {
			if err == nil || !strings.Contains(err.Error(), "READONLY") {
				t.Errorf("%s on a replica = %v, want Redis to refuse it as READONLY", name, err)
			}
		}
	})

	// Every replica, each read through by its own stack.
	replicaConnections := make([]string, len(r.rt.replicaAddresses))
	for i, address := range r.rt.replicaAddresses {
		replicaConnections[i] = strings.Replace(readConnection, connectionAddress(t, readConnection), address, 1)
	}

	t.Run("ReplicaReaderIsInvalidatedByThePrimary", func(t *testing.T) {
		ctx := context.Background()
		ns := cachetest.Namespace(t)
		p := cacheiface.NewPartition("tenant-" + ns)
		writer := replicaStack(t, rediscache.New(redisClient(t, primaryConnection), rediscache.WithPrefix(ns)))
		var readers []*cacheiface.Stack
		for _, connection := range replicaConnections {
			readers = append(readers, replicaStack(t, rediscache.New(redisClient(t, primaryConnection),
				rediscache.WithPrefix(ns), rediscache.WithReplica(redisClient(t, connection)))))
		}
		mustCache(t, writer.Set(ctx, p, "k", []byte("one")))
		for _, reader := range readers {
			eventuallyGets(t, reader, p, "k", "one", 3*time.Second)
		}
		mustCache(t, writer.Set(ctx, p, "k", []byte("two")))
		for _, reader := range readers {
			eventuallyGets(t, reader, p, "k", "two", 3*time.Second)
		}
	})

	t.Run("ReplicationLag", func(t *testing.T) {
		measureReplicationLag(t, primaryConnection, replicaConnections[0])
	})
}

func mustCache(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// replicaStack is a consumer process: an in-memory tier over layer.
func replicaStack(t *testing.T, layer cacheiface.Layer) *cacheiface.Stack {
	t.Helper()
	s, err := cacheiface.New(context.Background(),
		cacheiface.WithTier(cacheiface.NewMemory(), time.Hour),
		cacheiface.WithTier(layer, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// primaryNotices reads through a replica but takes its notices from the
// primary: the arrangement WithReplica exists to avoid, measured below.
type primaryNotices struct {
	*rediscache.Layer
	notices *rediscache.Layer
}

func (l primaryNotices) Subscribe(ctx context.Context, fn func(string)) (func(), error) {
	return l.notices.Subscribe(ctx, fn)
}

// measureReplicationLag answers, on the local servers the Runtime started:
// after a write to the primary, can a replica still serve the old value, and
// for how long; and what does a stack reading through the replica return once
// it has been invalidated. Timings are logged, not asserted — they describe
// this host. What is asserted is the property the driver relies on: a notice
// delivered by the replica never precedes the write it announces there.
func measureReplicationLag(t *testing.T, primaryConnection, replicaConnection string) {
	ctx := context.Background()
	const writes = 1000
	primary, replica := redisClient(t, primaryConnection), redisClient(t, replicaConnection)
	ns := cachetest.Namespace(t)
	writer := rediscache.New(primary, rediscache.WithPrefix(ns))
	// Reads the replica alone; its notices are not used here.
	onReplica := rediscache.New(primary, rediscache.WithPrefix(ns), rediscache.WithReplica(replica))
	holds := func(key, want string) bool {
		got, err := onReplica.Get(ctx, key)
		return err == nil && string(got.Value) == want
	}

	// 1. Lag: a poller reads the replica continuously and stamps when each
	// write first appears there; lag is that stamp minus the moment the
	// primary's acknowledgement reached the writer. Negative means the replica
	// had the write before the writer heard back. Separately, a read issued
	// after the ack that still returns the previous value is a stale read.
	var mu sync.Mutex
	seen := map[string]time.Time{}
	var stopPoll atomic.Bool
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for !stopPoll.Load() {
			got, err := onReplica.Get(ctx, "lag")
			if err != nil {
				continue
			}
			mu.Lock()
			if _, ok := seen[string(got.Value)]; !ok {
				seen[string(got.Value)] = time.Now()
			}
			mu.Unlock()
		}
	}()
	var stale int
	acks := make([]time.Time, writes)
	for i := range writes {
		want := fmt.Sprint(i)
		mustCache(t, writer.Set(ctx, "lag", cacheEntry(want), time.Minute))
		acks[i] = time.Now()
		if !holds("lag", want) {
			stale++
		}
		for deadline := acks[i].Add(5 * time.Second); !holds("lag", want); {
			if time.Now().After(deadline) {
				t.Fatalf("write %d never reached the replica", i)
			}
		}
	}
	time.Sleep(10 * time.Millisecond)
	stopPoll.Store(true)
	<-polled
	var lags []time.Duration
	mu.Lock()
	for i, acked := range acks {
		if at, ok := seen[fmt.Sprint(i)]; ok {
			lags = append(lags, at.Sub(acked))
		}
	}
	mu.Unlock()
	slices.Sort(lags)
	if len(lags) == 0 {
		t.Fatal("the poller never saw a write on the replica")
	}
	t.Logf("replica lag behind the primary's ack over %d writes (%d caught by the poller): min %s, p50 %s, p99 %s, max %s; a replica read issued after the ack returned the previous value %d/%d times",
		writes, len(lags), lags[0], lags[len(lags)/2], lags[len(lags)*99/100], lags[len(lags)-1], stale, writes)

	// 2. When a notice arrives, is the write it announces already on the
	// replica? A subscriber on the replica, and one on the primary, re-read
	// the replica the moment each notice lands.
	check := func(subscriber *goredis.Client) (int, int) {
		var mu sync.Mutex
		var expected string
		var staleOnNotice, notices int
		layer := rediscache.New(subscriber, rediscache.WithPrefix(ns))
		stop, err := layer.Subscribe(ctx, func(key string) {
			if key != "notice" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			notices++
			if !holds("notice", expected) {
				staleOnNotice++
			}
		})
		mustCache(t, err)
		defer stop()
		for i := range writes / 2 {
			mu.Lock()
			expected = fmt.Sprint(i)
			mu.Unlock()
			mustCache(t, writer.Set(ctx, "notice", cacheEntry(fmt.Sprint(i)), time.Minute))
			// Let the notice land before the next write moves the expectation.
			deadline := time.Now().Add(time.Second)
			for {
				mu.Lock()
				n := notices
				mu.Unlock()
				if n > i || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Microsecond)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return staleOnNotice, notices
	}
	replicaStale, replicaNotices := check(replica)
	primaryStale, primaryNoticeCount := check(primary)
	t.Logf("replica still old when the notice arrived: %d/%d notices delivered by the replica, %d/%d delivered by the primary",
		replicaStale, replicaNotices, primaryStale, primaryNoticeCount)
	if replicaStale != 0 {
		t.Errorf("%d notices delivered by the replica arrived before their write had: WithReplica's ordering claim is false", replicaStale)
	}

	// 3. What a stack reading through the replica returns. A reader re-reads
	// continuously, as a busy consumer would, so an eviction is refilled at
	// once; after each write settles, the stack must serve that write.
	stuck := func(layer cacheiface.Layer) int {
		p := cacheiface.NewPartition("tenant-" + ns)
		reader := replicaStack(t, layer)
		writerStack := replicaStack(t, rediscache.New(primary, rediscache.WithPrefix(ns)))
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				_, _ = reader.Get(ctx, p, "stack")
			}
		}()
		defer func() { stop.Store(true); <-done }()
		var stuck int
		for i := range 100 {
			want := fmt.Sprint(i)
			mustCache(t, writerStack.Set(ctx, p, "stack", []byte(want)))
			time.Sleep(20 * time.Millisecond)
			if got, _ := reader.Get(ctx, p, "stack"); string(got) != want {
				stuck++
			}
		}
		return stuck
	}
	withReplica := stuck(rediscache.New(primary, rediscache.WithPrefix(ns), rediscache.WithReplica(replica)))
	withPrimaryNotices := stuck(primaryNotices{
		Layer:   rediscache.New(primary, rediscache.WithPrefix(ns), rediscache.WithReplica(replica)),
		notices: rediscache.New(primary, rediscache.WithPrefix(ns)),
	})
	t.Logf("stack reading through the replica, 20ms after each of 100 writes: stale %d/100 with the replica's notices (WithReplica), %d/100 with the primary's",
		withReplica, withPrimaryNotices)
	if withReplica != 0 {
		t.Errorf("a stack reading through the replica with its notices still served %d stale values after the write settled", withReplica)
	}
}

func cacheEntry(value string) cacheiface.Entry { return cacheiface.Entry{Value: []byte(value)} }
