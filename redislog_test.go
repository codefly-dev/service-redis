package main

// redislog_test.go — the redis log writer under the concurrency the runtimes
// put it under. The native runtime hands one writer to the primary and to
// each replica process, and core forwards every process's stdout and stderr
// from a goroutine of its own, one complete line per Write. A writer that
// buffers lines without a lock loses or garbles them under that, and panics
// when another goroutine reslices the buffer under it, which takes the whole
// test binary down with the real-server suite in it.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/core/wool"
)

// recordingProcessor keeps every record a Wool emits.
type recordingProcessor struct {
	mu   sync.Mutex
	logs []*wool.Log
}

func (r *recordingProcessor) Process(msg *wool.Log) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, msg)
}

func (r *recordingProcessor) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]string, 0, len(r.logs))
	for _, l := range r.logs {
		messages = append(messages, l.Message)
	}
	return messages
}

func TestRedisLogWriterTakesConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	sink := &recordingProcessor{}
	writer := newRedisLogWriter(wool.New(ctx, &wool.Resource{Kind: "agent", Unique: "redis"}).WithLogger(sink).Get(ctx))

	// A primary and two replicas, each forwarding its own stream.
	const processes, lines = 3, 500
	var wg sync.WaitGroup
	for pid := range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range lines {
				line := fmt.Sprintf("%d:S 20 Oct 2026 14:56:37.312 * process %d line %d\n", 100+pid, pid, i)
				if _, err := writer.Write([]byte(line)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	messages := sink.messages()
	if len(messages) != processes*lines {
		t.Fatalf("%d records for %d lines written", len(messages), processes*lines)
	}
	seen := map[string]bool{}
	for _, m := range messages {
		if !strings.HasPrefix(m, "process ") || seen[m] {
			t.Fatalf("record %q is garbled or duplicated", m)
		}
		seen[m] = true
	}
}
