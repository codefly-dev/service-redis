package main

// cache_format_test.go — what a stack over this driver does with an entry an
// older driver left behind. cache/v0.2.0 wrote a format the current driver
// does not read, and the driver's decode argues that such a value must be a
// miss the stack refills, never an error: a Get error counts against the
// tier's breaker, so a keyspace of old entries would read as a failing server
// for as long as they were reached. cache/cache_test.go pins that on bytes;
// this proves it end to end, on every real server the suite runs, by planting
// the old entry in the keyspace and reading through a stack.

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
	"github.com/codefly-dev/interface-cache/go/cache/cachetest"
)

// encodeV1 writes an entry as cache/v0.2.0 and before did: format 1, flags,
// FreshUntil as Unix nanoseconds, the version length as a uvarint, the
// version, then the value. The format is history, so this copy and the one in
// cache/cache_test.go cannot drift apart.
func encodeV1(value, version string, freshUntil time.Time) []byte {
	buf := []byte{1, 0}
	buf = binary.BigEndian.AppendUint64(buf, uint64(freshUntil.UnixNano()))
	buf = binary.AppendUvarint(buf, uint64(len(version)))
	buf = append(buf, version...)
	return append(buf, value...)
}

// tierKeys records, through the stack's observer, every key the first tier
// was read or written under, as that tier saw it: the stack keys its layers by
// schema, partition and generation, so the keys the driver stores are its to
// name, not the test's to guess. The one key the observer does not report is
// the generation's, which cache.GenerationKey names.
type tierKeys struct {
	mu   sync.Mutex
	keys []string
}

func (k *tierKeys) observe(ev cacheiface.Event) {
	if ev.Tier != 0 || ev.LayerKey == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !slices.Contains(k.keys, ev.LayerKey) {
		k.keys = append(k.keys, ev.LayerKey)
	}
}

func (k *tierKeys) seen() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.keys)
}

// runOldFormatEntries fills a stack's shared tier, plants under every key it
// used the entry cache/v0.2.0 would have written there, and reads through the
// stack again: FailClosed, with the breaker set to open on a single failure,
// so an error on an old entry is refused rather than degraded around. The read
// must be served from the origin, and must leave the tier refilled: the next
// read is a hit, and the driver reads the origin's value back from the server.
func runOldFormatEntries(t *testing.T, s redisSuite) {
	ctx := context.Background()
	ns := cachetest.Namespace(t)
	p := cacheiface.NewPartition("tenant-" + ns)
	layer := s.New(t, ns)
	var loads atomic.Int32
	origin := cacheiface.SourceFunc(func(context.Context, string, string) (cacheiface.Entry, error) {
		loads.Add(1)
		return cacheiface.Entry{Value: []byte("from origin"), Version: "1"}, nil
	})
	var keys tierKeys
	stack, err := cacheiface.New(ctx,
		cacheiface.WithTier(layer, time.Hour), cacheiface.WithOrigin(origin),
		cacheiface.WithBreaker(1, time.Hour), cacheiface.WithObserver(keys.observe))
	mustCache(t, err)
	t.Cleanup(stack.Close)

	get := func(want int32, opts ...cacheiface.ModeOption) {
		t.Helper()
		v, err := stack.Get(ctx, p, "k", opts...)
		if err != nil || string(v) != "from origin" {
			t.Fatalf("Get = %q, %v; want the origin's value", v, err)
		}
		if n := loads.Load(); n != want {
			t.Fatalf("the origin was loaded %d times, want %d", n, want)
		}
	}
	get(1)
	generation := cacheiface.GenerationKey("k")
	stored := append(keys.seen(), generation)
	if len(stored) < 2 {
		t.Fatal("the stack used no key of its own in the shared layer")
	}

	// What cache/v0.2.0 would have left under each key, planted through the
	// writer and waited for through the layer: a layer reading through a
	// replica sees the primary's write once it is replicated.
	for _, key := range stored {
		old := encodeV1("from an older driver", "1", time.Now().Add(time.Hour))
		mustCache(t, s.writer.Set(ctx, ns+":v:{"+key+"}", old, time.Minute).Err())
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			_, err := layer.Get(ctx, key)
			if errors.Is(err, cacheiface.ErrMiss) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Get of an entry in the old format = %v, want cache.ErrMiss", err)
			}
		}
	}

	// Served around: a tier error would be refused here as ErrUnavailable,
	// and would open the breaker on the way.
	get(2, cacheiface.FailClosed())
	// And refilled: the tier serves the next read, which an open breaker
	// would have skipped; the generation is current again, and the origin's
	// value is held under a key the stack used, in a format the driver reads.
	get(2)
	if _, err := layer.Get(ctx, generation); err != nil {
		t.Fatalf("the generation key was not refilled: %v", err)
	}
	refilled := false
	for _, key := range keys.seen() {
		if e, err := layer.Get(ctx, key); err == nil && string(e.Value) == "from origin" {
			refilled = true
		}
	}
	if !refilled {
		t.Fatalf("no key the stack used (%q) holds the origin's value", keys.seen())
	}
}
