package main

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
)

func newLoadedRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt := NewRuntime()
	if err := rt.HeadlessLoad(context.Background(), &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "redis",
		Version:   "1.2.3",
	}); err != nil {
		t.Fatalf("HeadlessLoad: %v", err)
	}
	rt.Runtime.WithContext(resources.NewRuntimeContextNix())
	return rt
}

// startedNativeRuntime hands back a runtime that owns a running native redis,
// which is the state every lifecycle operation below has to unwind.
func startedNativeRuntime(t *testing.T) (*Runtime, *nixRedis, uint16) {
	t.Helper()
	n, port := newFakeNativeRedis(t, fakeRedisServe)
	if err := n.launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	rt := newLoadedRuntime(t)
	rt.setNativeRuntime(n)
	t.Logf("runtime owns redis pid %d on port %d", ownedPID(t, n), port)
	return rt, n, port
}

// TestRuntimeStopReleasesNativeRedis is the defect this contract closes: Stop
// used to report STOPPED while the native server kept running and kept the
// assigned port.
func TestRuntimeStopReleasesNativeRedis(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	resp, err := rt.Stop(context.Background(), &runtimev0.StopRequest{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("Stop status = %v (%s)", state, resp.GetStatus().GetMessage())
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if rt.nativeRuntime() != nil {
		t.Error("Stop kept ownership of a runtime it released")
	}

	// A second Stop has nothing to own and must still succeed.
	resp, err = rt.Stop(context.Background(), &runtimev0.StopRequest{})
	if err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("second Stop status = %v (%s)", state, resp.GetStatus().GetMessage())
	}
}

// TestRuntimeDestroyReleasesNativeRedisAndRepeats covers Destroy reaching a
// runtime that is still running, and then a runtime that is already stopped.
func TestRuntimeDestroyReleasesNativeRedisAndRepeats(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
		if err != nil {
			t.Fatalf("Destroy %d: %v", attempt, err)
		}
		if state := resp.GetStatus().GetState(); state != runtimev0.DestroyStatus_SUCCESS {
			t.Fatalf("Destroy %d status = %v (%s)", attempt, state, resp.GetStatus().GetMessage())
		}
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if rt.nativeRuntime() != nil {
		t.Error("Destroy kept ownership of a runtime it released")
	}
}

// TestRuntimeStopAndDestroyKeepOwnershipWhenTeardownFails: cleanup that fails
// must leave something behind to retry with, not silently forget the process.
func TestRuntimeStopAndDestroyKeepOwnershipWhenTeardownFails(t *testing.T) {
	rt := newLoadedRuntime(t)
	rt.setNativeRuntime(&nixRedis{port: 6379, proc: stubborn{}})

	stop, err := rt.Stop(context.Background(), &runtimev0.StopRequest{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := stop.GetStatus().GetState(); state != runtimev0.StopStatus_ERROR {
		t.Fatalf("Stop status = %v, want ERROR", state)
	}
	if rt.nativeRuntime() == nil {
		t.Fatal("Stop dropped ownership of a runtime it could not stop")
	}

	destroy, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if state := destroy.GetStatus().GetState(); state != runtimev0.DestroyStatus_ERROR {
		t.Fatalf("Destroy status = %v, want ERROR", state)
	}
	if !strings.Contains(destroy.GetStatus().GetMessage(), stubbornStopMessage) {
		t.Errorf("Destroy message %q does not explain the failure", destroy.GetStatus().GetMessage())
	}
	if rt.nativeRuntime() == nil {
		t.Fatal("Destroy dropped ownership of a runtime it could not stop")
	}
}

// TestRuntimeSupervisionRevokesReadyOnUnexpectedExit: a native server that dies
// mid-run must take the STARTED status down with it.
func TestRuntimeSupervisionRevokesReadyOnUnexpectedExit(t *testing.T) {
	rt, n, _ := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	if _, err := rt.Runtime.StartResponse(); err != nil {
		t.Fatalf("StartResponse: %v", err)
	}
	rt.superviseNative(n)

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill redis: %v", err)
	}
	awaitStartState(t, rt, runtimev0.StartStatus_ERROR)
}

// TestRuntimeSupervisionIgnoresSupersededRuntime: the exit of a handle the
// runtime no longer owns says nothing about the redis serving now, and must not
// revoke its status.
func TestRuntimeSupervisionIgnoresSupersededRuntime(t *testing.T) {
	rt, superseded, _ := startedNativeRuntime(t)
	pid := ownedPID(t, superseded)

	if _, err := rt.Runtime.StartResponse(); err != nil {
		t.Fatalf("StartResponse: %v", err)
	}
	rt.superviseNative(superseded)

	// A re-Init replaces the handle; the old process is no longer this
	// runtime's redis.
	rt.setNativeRuntime(&nixRedis{port: superseded.port})

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill redis: %v", err)
	}
	requireProcessGone(t, pid)
	// Give the supervision goroutine room to misbehave before concluding it did
	// not.
	time.Sleep(500 * time.Millisecond)
	requireStartState(t, rt, runtimev0.StartStatus_STARTED)
}

func startState(t *testing.T, rt *Runtime) runtimev0.StartStatus_Status {
	t.Helper()
	info, err := rt.Information(context.Background(), &runtimev0.InformationRequest{})
	if err != nil {
		t.Fatalf("Information: %v", err)
	}
	return info.GetStartStatus().GetState()
}

func awaitStartState(t *testing.T, rt *Runtime, want runtimev0.StartStatus_Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if startState(t, rt) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("start status = %v, want %v", startState(t, rt), want)
}

func requireStartState(t *testing.T, rt *Runtime, want runtimev0.StartStatus_Status) {
	t.Helper()
	if got := startState(t, rt); got != want {
		t.Fatalf("start status = %v, want %v", got, want)
	}
}
