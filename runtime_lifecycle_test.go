package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// nixInitRequest is an Init request that resolves: a native mapping for the
// runtime's own TCP endpoint. Tests that need Init to get past validation and
// reach the backend it is about to build use this.
func nixInitRequest(t *testing.T, rt *Runtime, port uint16) *runtimev0.InitRequest {
	t.Helper()
	rt.TcpEndpoint = &basev0.Endpoint{Name: "tcp", Module: "module", Service: "redis", Api: "tcp"}
	native := resources.NewNetworkInstance("localhost", port)
	native.Access = resources.NewNativeNetworkAccess()
	return &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextNix(),
		ProposedNetworkMappings: []*basev0.NetworkMapping{{
			Endpoint:  rt.TcpEndpoint,
			Instances: []*basev0.NetworkInstance{native},
		}},
	}
}

// breakRuntimeRoot makes newNixRedis fail on its first filesystem call, which
// is the step immediately after Init releases the runtime it already owns.
// Nothing on that path reaches nix, so the failure is identical on every host.
func breakRuntimeRoot(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocker)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(blocker, "cache"))
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

// TestRuntimeInitReleasesTheRuntimeItAlreadyOwns covers the re-Init leak: a
// second Init used to overwrite the handle, dropping the only reference to a
// live server that kept holding the assigned port. The release happens before
// any other Init work, so an Init that fails afterwards still leaves nothing
// behind — this one fails on its (deliberately empty) network mappings.
func TestRuntimeInitReleasesTheRuntimeItAlreadyOwns(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	// Init gets a request it can resolve, so it commits to building a
	// replacement; constructing that replacement then fails.
	breakRuntimeRoot(t)
	resp, err := rt.Init(context.Background(), nixInitRequest(t, rt, port))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.InitStatus_ERROR {
		t.Fatalf("Init status = %v, want ERROR", state)
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if rt.nativeRuntime() != nil {
		t.Error("Init kept ownership of the runtime it released")
	}
}

// TestRuntimeInitKeepsOwnershipWhenReleaseFails: an Init that cannot release
// the incumbent must not proceed to start a second server, and must not lose
// track of the one it failed to stop.
func TestRuntimeInitKeepsOwnershipWhenReleaseFails(t *testing.T) {
	rt := newLoadedRuntime(t)
	rt.setNativeRuntime(&nixRedis{port: 6379, proc: stubborn{}})

	resp, err := rt.Init(context.Background(), nixInitRequest(t, rt, 6379))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.InitStatus_ERROR {
		t.Fatalf("Init status = %v, want ERROR", state)
	}
	if !strings.Contains(resp.GetStatus().GetMessage(), stubbornStopMessage) {
		t.Errorf("Init message %q does not explain the failed release", resp.GetStatus().GetMessage())
	}
	if rt.nativeRuntime() == nil {
		t.Fatal("Init dropped ownership of a runtime it could not stop")
	}
}

// TestRuntimeStopKeepsRedisRunningWhenAskedTo: warm reuse is the documented
// opt-out from releasing execution resources.
func TestRuntimeStopKeepsRedisRunningWhenAskedTo(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)
	rt.Settings.KeepRunning = true

	resp, err := rt.Stop(context.Background(), &runtimev0.StopRequest{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("Stop status = %v (%s)", state, resp.GetStatus().GetMessage())
	}
	if !processAlive(pid) {
		t.Fatal("keep-running Stop terminated the server anyway")
	}
	if rt.nativeRuntime() == nil {
		t.Error("keep-running Stop gave up ownership of a server it left running")
	}
	requireRedisAnswers(t, port)

	// Clearing the opt-in releases it, so the test does not leak the server.
	rt.Settings.KeepRunning = false
	if _, err = rt.Stop(context.Background(), &runtimev0.StopRequest{}); err != nil {
		t.Fatalf("releasing Stop: %v", err)
	}
	requireProcessGone(t, pid)
}

// TestRuntimeConcurrentStopAndDestroyReleaseOnce: both arrive on their own gRPC
// goroutine and both mutate the same handle. Run under -race, this is what
// catches teardown state being written without a lock.
func TestRuntimeConcurrentStopAndDestroyReleaseOnce(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	var wg sync.WaitGroup
	wg.Add(2)
	stopState := make(chan runtimev0.StopStatus_Status, 1)
	destroyState := make(chan runtimev0.DestroyStatus_Status, 1)
	go func() {
		defer wg.Done()
		resp, err := rt.Stop(context.Background(), &runtimev0.StopRequest{})
		if err != nil {
			t.Error("Stop:", err)
			return
		}
		stopState <- resp.GetStatus().GetState()
	}()
	go func() {
		defer wg.Done()
		resp, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
		if err != nil {
			t.Error("Destroy:", err)
			return
		}
		destroyState <- resp.GetStatus().GetState()
	}()
	wg.Wait()

	if state := <-stopState; state != runtimev0.StopStatus_SUCCESS {
		t.Errorf("Stop status = %v", state)
	}
	if state := <-destroyState; state != runtimev0.DestroyStatus_SUCCESS {
		t.Errorf("Destroy status = %v", state)
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if rt.nativeRuntime() != nil {
		t.Error("concurrent teardown left ownership behind")
	}
}

// TestRuntimeInitKeepsAWorkingRedisWhenTheRequestIsInvalid: releasing the
// incumbent is how Init avoids owning two servers, but it must not be the
// first thing Init does. A request that never resolves is a request that was
// never going to produce a replacement, and answering it by killing the redis
// that is currently serving turns a rejected call into an outage.
func TestRuntimeInitKeepsAWorkingRedisWhenTheRequestIsInvalid(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)

	// No network mappings: Init cannot resolve an endpoint, so it never reaches
	// the point of building anything.
	resp, err := rt.Init(context.Background(), &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextNix(),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.InitStatus_ERROR {
		t.Fatalf("Init status = %v, want ERROR", state)
	}
	if !processAlive(pid) {
		t.Fatal("a request that failed validation killed the redis that was serving")
	}
	if rt.nativeRuntime() == nil {
		t.Fatal("Init gave up ownership of a server it left running")
	}
	requireRedisAnswers(t, port)

	// Ownership survived, so the server is still reachable — prove it rather
	// than leaking it.
	if _, err = rt.Stop(context.Background(), &runtimev0.StopRequest{}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	requireProcessGone(t, pid)
}

// TestRuntimeDestroyReleasesRedisEvenWhenKeepRunningIsSet: keep-running is an
// opt-out from Stop releasing execution resources, not a way to make a server
// outlive Destroy. Honouring it here would strand the process permanently,
// since Destroy is the last call anything makes.
func TestRuntimeDestroyReleasesRedisEvenWhenKeepRunningIsSet(t *testing.T) {
	rt, n, port := startedNativeRuntime(t)
	pid := ownedPID(t, n)
	rt.Settings.KeepRunning = true

	resp, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if state := resp.GetStatus().GetState(); state != runtimev0.DestroyStatus_SUCCESS {
		t.Fatalf("Destroy status = %v (%s)", state, resp.GetStatus().GetMessage())
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if rt.nativeRuntime() != nil {
		t.Error("Destroy kept ownership of a runtime it released")
	}
}
