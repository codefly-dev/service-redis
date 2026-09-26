package main

// main_test.go — tests run the agent the way agents.Serve does.
//
// Every RPC method starts with `defer s.Wool.Catch()`. Catch recovers a panic
// and re-raises it only when wool.SetRethrowAfterCatch(true) is set, which
// agents.Serve does: in a served agent a panic reaches the recovery interceptor
// and becomes an RPC error. Tests never call Serve, so without the switch a
// panicking method returns its zero values instead, and a test can pass on a
// (nil, nil) it never meant to accept.

import (
	"context"
	"os"
	"testing"

	"github.com/codefly-dev/core/wool"
)

func TestMain(m *testing.M) {
	wool.SetRethrowAfterCatch(true)
	os.Exit(m.Run())
}

// A panic inside an agent method must reach the test, as it reaches the host in
// a served agent. The method panics for real: a nil network instance is
// dereferenced once the configuration loads.
func TestAgentPanicReachesTheCaller(t *testing.T) {
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		_, _ = NewService().CreateConnectionConfiguration(context.Background(), nil, nil)
		return false
	}()
	if !panicked {
		t.Fatal("CreateConnectionConfiguration returned for a nil instance: a panic inside Catch was swallowed, and a test would read its zero values as success")
	}
}

// The hazard TestMain removes, stated so it cannot quietly come back: with the
// switch off, the same call returns (nil, nil).
func TestAgentPanicIsSwallowedWithoutRethrow(t *testing.T) {
	wool.SetRethrowAfterCatch(false)
	t.Cleanup(func() { wool.SetRethrowAfterCatch(true) })
	conf, err := NewService().CreateConnectionConfiguration(context.Background(), nil, nil)
	if conf != nil || err != nil {
		t.Fatalf("without rethrow = %v, %v; want the (nil, nil) a swallowed panic returns", conf, err)
	}
}
