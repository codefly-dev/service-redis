package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/gofrs/flock"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// These tests cover what happens to a native runtime root when its run ends.
// The server is the fake of nixredis_lifecycle_test.go, a real process on a
// real socket; the record on the root and the decision to remove it are core's
// (runners/runstate), reached the way production reaches them.

var disposableRun = runstate.Run{Workspace: "/work/shop", NamingScope: "invdisposable", Disposable: true}

// claimedNativeRuntime is a Runtime serving a native redis whose root is
// claimed for run, as Init leaves it. It returns the root too.
func claimedNativeRuntime(t *testing.T, run runstate.Run) (*Runtime, *nixRedis, string) {
	t.Helper()
	n, _ := newFakeNativeRedis(t, fakeRedisServe)
	n.root, n.run = filepath.Dir(n.configPath), run
	var err error
	n.owner, n.state, err = claimRunState(context.Background(), n.root, run, time.Second)
	require.NoError(t, err)
	require.NoError(t, n.launch(context.Background()))
	require.NoError(t, os.WriteFile(filepath.Join(n.dataDir, "dump.rdb"), []byte("rows"), 0o600))
	rt := newLoadedRuntime(t)
	rt.setNativeRuntime(n)
	rt.claimNativeStates(n)
	return rt, n, n.root
}

func stopRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	stopped, err := rt.Stop(context.Background(), &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StopStatus_SUCCESS, stopped.GetStatus().GetState(), stopped.GetStatus().GetMessage())
}

func destroyRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	destroyed, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, destroyed.GetStatus().GetState(), destroyed.GetStatus().GetMessage())
}

// TestDisposableRunStateIsRemovedWhenTheRunDestroysIt: a run whose caller
// declared its state disposable (a generated naming scope: temporary ports, a
// test's dependency stack) leaves no runtime root behind. Stop still retains
// it, because a service restarts inside a run; the root goes on Destroy.
func TestDisposableRunStateIsRemovedWhenTheRunDestroysIt(t *testing.T) {
	rt, n, root := claimedNativeRuntime(t, disposableRun)
	pid := ownedPID(t, n)

	stopRuntime(t, rt)
	requireProcessGone(t, pid)
	require.FileExists(t, filepath.Join(root, "data", "dump.rdb"), "stop is not the end of a run")
	removed, err := runstate.Collect(root)
	require.NoError(t, err)
	require.False(t, removed, "a stopped service of a live run still claims its root")

	destroyRuntime(t, rt)
	require.NoDirExists(t, root, "a disposable run must not leave its runtime root behind")

	destroyRuntime(t, rt)
}

// TestDisposableRunStateIsRemovedByDestroyWithoutStop: Destroy is also the
// first thing a run may call on a serving agent.
func TestDisposableRunStateIsRemovedByDestroyWithoutStop(t *testing.T) {
	rt, n, root := claimedNativeRuntime(t, disposableRun)
	pid := ownedPID(t, n)

	destroyRuntime(t, rt)
	requireProcessGone(t, pid)
	require.NoDirExists(t, root)
}

// TestPersistentRunStateIsNeverRemoved: a plain run, and a run under a scope a
// developer named, is that developer's data. No operation of this agent, and
// no later collection, removes it.
func TestPersistentRunStateIsNeverRemoved(t *testing.T) {
	for name, run := range map[string]runstate.Run{
		"plain run":    runstate.For("/work/shop", &basev0.Environment{Name: "local"}),
		"named scope":  runstate.For("/work/shop", &basev0.Environment{Name: "local", NamingScope: "mine"}),
		"no scope":     runstate.For("/work/shop", &basev0.Environment{Name: "local", Disposable: true}),
		"no statement": runstate.For("/work/shop", nil),
	} {
		t.Run(name, func(t *testing.T) {
			rt, _, root := claimedNativeRuntime(t, run)
			stopRuntime(t, rt)
			destroyRuntime(t, rt)
			require.FileExists(t, filepath.Join(root, "data", "dump.rdb"))
			require.FileExists(t, filepath.Join(root, "redis.conf"))

			collectAbandonedRunState(filepath.Dir(root), filepath.Join(filepath.Dir(root), "self"))
			removed, err := runstate.Collect(root)
			require.NoError(t, err)
			require.False(t, removed)
			require.FileExists(t, filepath.Join(root, "data", "dump.rdb"), "a developer's data is never removed")
		})
	}
}

// TestNativeLaunchTracksTheServerOnItsRunState: the server is in its own
// process group and survives an agent that is killed. Recorded on the claim,
// it keeps its root in use for as long as it runs, whoever else is gone.
func TestNativeLaunchTracksTheServerOnItsRunState(t *testing.T) {
	rt, n, root := claimedNativeRuntime(t, disposableRun)
	t.Cleanup(func() { destroyRuntime(t, rt) })
	pid := ownedPID(t, n)

	holders, err := filepath.Glob(filepath.Join(root, "*", "holders", "*.json"))
	require.NoError(t, err)
	require.Len(t, holders, 1)
	data, err := os.ReadFile(holders[0])
	require.NoError(t, err)
	var holder struct {
		Tracked []struct {
			PID int `json:"pid"`
		} `json:"tracked"`
	}
	require.NoError(t, json.Unmarshal(data, &holder))
	require.Len(t, holder.Tracked, 1, string(data))
	require.Equal(t, pid, holder.Tracked[0].PID)
}

const claimHelperDir = "REDIS_TEST_CLAIM_DIR"

// TestHelperClaimRunState is not a test: re-executed with claimHelperDir set,
// it claims that root as an agent would and exits, which leaves the record of
// a run that was killed.
func TestHelperClaimRunState(t *testing.T) {
	root := os.Getenv(claimHelperDir)
	if root == "" {
		return
	}
	if os.Getenv(claimHelperDir+"_LAUNCHER") != "" {
		// Stand in for the run: launch the agent, and be gone before the test
		// looks, as a killed CLI is.
		agent := exec.Command(os.Args[0], "-test.run=^TestHelperClaimRunState$")
		agent.Env = append(os.Environ(), claimHelperDir+"_LAUNCHER=")
		out, err := agent.CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	run := runstate.Run{Workspace: "/work/shop", NamingScope: os.Getenv(claimHelperDir + "_SCOPE")}
	run.Disposable = run.NamingScope != ""
	_, hold, err := claimRunState(context.Background(), root, run, time.Second)
	require.NoError(t, err)
	if pidFile := os.Getenv(claimHelperDir + "_SERVER"); pidFile != "" {
		// The server the agent leaves running: a process of its own session,
		// which outlives the agent and its run.
		server := exec.Command("sleep", "300")
		server.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, server.Start())
		require.NoError(t, hold.Track(server.Process.Pid))
		require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(server.Process.Pid)), 0o600))
	}
}

// abandonedServer is the runtime root of an agent that is gone and the PID of
// the server it left running there. killedRun says whether the run that
// launched the agent is gone too.
func abandonedServer(t *testing.T, cache, name, scope string, killedRun bool) (string, int) {
	t.Helper()
	root := filepath.Join(cache, name)
	pidFile := filepath.Join(t.TempDir(), "server.pid")
	claim := exec.Command(os.Args[0], "-test.run=^TestHelperClaimRunState$")
	claim.Env = append(os.Environ(), claimHelperDir+"="+root, claimHelperDir+"_SCOPE="+scope, claimHelperDir+"_SERVER="+pidFile)
	if killedRun {
		claim.Env = append(claim.Env, claimHelperDir+"_LAUNCHER=1")
	}
	out, err := claim.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.True(t, processRuns(pid), "the server the agent left never ran")
	return root, pid
}

func processRuns(pid int) bool {
	_, err := runners.InspectProcess(pid)
	return !errors.Is(err, runners.ErrProcessNotFound)
}

// TestAbandonedDisposableRunsServerIsStopped: a killed run leaves its server
// running, and a root is in use for as long as that server runs. The next
// agent to initialize stops the server such a run tracked on its root and
// removes the root, when the run was disposable and is gone, and only then.
func TestAbandonedDisposableRunsServerIsStopped(t *testing.T) {
	cache := t.TempDir()
	killed, killedServer := abandonedServer(t, cache, "killed", "invkilled", true)
	persistent, persistentServer := abandonedServer(t, cache, "persistent", "", true)
	// The agent died; the run that launched it (this test) goes on.
	running, runningServer := abandonedServer(t, cache, "running", "invrunning", false)

	collectAbandonedRunState(cache, filepath.Join(cache, "self"))

	require.Eventually(t, func() bool { return !processRuns(killedServer) }, 5*time.Second, 50*time.Millisecond,
		"the server a killed disposable run left is still running")
	require.NoDirExists(t, killed)
	require.True(t, processRuns(persistentServer), "a developer's server is theirs to stop")
	require.DirExists(t, persistent)
	require.True(t, processRuns(runningServer), "a run that goes on keeps its server")
	require.DirExists(t, running)
}

// abandonedRoot is the runtime root of a run whose agent is gone.
func abandonedRoot(t *testing.T, cache, name, scope string) string {
	t.Helper()
	root := filepath.Join(cache, name)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "dump.rdb"), []byte("rows"), 0o600))
	claim := exec.Command(os.Args[0], "-test.run=^TestHelperClaimRunState$")
	claim.Env = append(os.Environ(), claimHelperDir+"="+root, claimHelperDir+"_SCOPE="+scope)
	out, err := claim.CombinedOutput()
	require.NoError(t, err, string(out))
	return root
}

// TestAbandonedDisposableRunStateIsCollected: a killed run never destroys its
// agents. The next agent to initialize on the machine removes the root such a
// run left, when that run was disposable, and only then.
func TestAbandonedDisposableRunStateIsCollected(t *testing.T) {
	cache := t.TempDir()
	disposable := abandonedRoot(t, cache, "disposable", "invkilled")
	persistent := abandonedRoot(t, cache, "persistent", "")
	self := abandonedRoot(t, cache, "self", "invself")

	// A root from before the record existed: locked like any other, claimed by
	// nobody.
	unclaimed := filepath.Join(cache, "unclaimed")
	require.NoError(t, os.MkdirAll(filepath.Join(unclaimed, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(unclaimed, ownerLockName), nil, 0o600))

	// A stopped service of a live run: its lock is free, its claim is not.
	stopped := filepath.Join(cache, "stopped")
	owner, _, err := claimRunState(context.Background(), stopped, disposableRun, time.Second)
	require.NoError(t, err)
	releaseRuntimeRoot(owner)

	// A root another invocation is starting on right now.
	starting := abandonedRoot(t, cache, "starting", "invstarting")
	lock := flock.New(filepath.Join(starting, ownerLockName))
	held, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, held)
	t.Cleanup(func() { releaseRuntimeRoot(lock) })

	collectAbandonedRunState(cache, self)

	require.NoDirExists(t, disposable)
	require.FileExists(t, filepath.Join(persistent, "data", "dump.rdb"), "a developer's data is never removed")
	require.DirExists(t, self, "the root being initialized is not a leftover")
	require.DirExists(t, unclaimed, "a root nobody declared disposable is somebody's data")
	require.DirExists(t, stopped, "a live agent still uses it")
	require.DirExists(t, starting, "a locked root is in use")
}

// TestClaimRunStateRecreatesARootRemovedWhileItWaited: a disposable root is
// removed under its lock. A claim that was waiting for that lock must not end
// up holding a lock on a file that is no longer on disk, guarding nothing.
func TestClaimRunStateRecreatesARootRemovedWhileItWaited(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	previous, hold, err := claimRunState(context.Background(), root, disposableRun, time.Second)
	require.NoError(t, err)

	type claim struct {
		owner *flock.Flock
		err   error
	}
	claimed := make(chan claim, 1)
	go func() {
		owner, _, err := claimRunState(context.Background(), root, disposableRun, 10*time.Second)
		claimed <- claim{owner, err}
	}()
	time.Sleep(200 * time.Millisecond)

	// The removal, as one step: a waiting claim retries its lock file into a
	// directory that is being emptied, which is not what is under test here.
	require.NoError(t, hold.Release())
	require.NoError(t, os.Rename(root, root+".removed"))
	releaseRuntimeRoot(previous)

	next := <-claimed
	require.NoError(t, next.err)
	t.Cleanup(func() { releaseRuntimeRoot(next.owner) })
	require.FileExists(t, filepath.Join(root, ownerLockName))
	_, err = claimRuntimeRoot(context.Background(), root, 100*time.Millisecond)
	require.Error(t, err, "the claim holds the lock that is on disk")
	removed, err := runstate.Collect(root)
	require.NoError(t, err)
	require.False(t, removed, "the new claim is recorded on the recreated root")
}

// TestRealRedisDisposableRunStateOverNix is the same contract through the
// Runtime on real nix-provisioned servers, a primary and a replica, each with
// its own root: a restart inside the run finds its data, and Destroy leaves
// neither root behind.
func TestRealRedisDisposableRunStateOverNix(t *testing.T) {
	requireBackend(t, nixBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ports, err := reserveLoopbackPorts(2)
	require.NoError(t, err)
	read, write := proposedRedisMapping("read", ports[0]), proposedRedisMapping("write", ports[1])
	unique := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	rt := NewRuntime()
	require.NoError(t, rt.HeadlessLoad(ctx, &basev0.ServiceIdentity{
		Workspace: "redis-disposable-" + unique, Module: "module", Name: "redis", Version: "1.0.0",
	}))
	rt.Runtime.SetEnvironment(&basev0.Environment{Name: "local", NamingScope: "inv" + unique, Disposable: true})
	rt.WithReadReplicas, rt.ReadReplicas = true, 1
	require.NoError(t, rt.resolveTCPEndpoints(ctx, []*basev0.Endpoint{read.Endpoint, write.Endpoint}))

	stateKey := redisStateKey(rt.Location, rt.Environment.GetNamingScope())
	var roots []string
	for _, key := range []string{stateKey, redisReplicaStateKey(stateKey, 1)} {
		root, err := redisRuntimeRoot(key)
		require.NoError(t, err)
		roots = append(roots, root)
	}
	t.Cleanup(func() {
		_, _ = rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
		for _, root := range roots {
			_ = os.RemoveAll(root)
		}
	})

	request := &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextNix(),
		ProposedNetworkMappings: []*basev0.NetworkMapping{read, write},
	}
	initialize := func() {
		t.Helper()
		init, err := rt.Init(ctx, request)
		require.NoError(t, err)
		require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus().GetMessage())
	}
	initialize()
	primary := rt.nativeRuntime()
	client := goredis.NewClient(&goredis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", primary.port)})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Set(ctx, "kept", "through a restart", 0).Err())

	stopRuntime(t, rt)
	for _, root := range roots {
		require.DirExists(t, filepath.Join(root, "data"), "stop is not the end of a run")
	}
	initialize()
	kept, err := client.Get(ctx, "kept").Result()
	require.NoError(t, err)
	require.Equal(t, "through a restart", kept)

	destroyRuntime(t, rt)
	for _, root := range roots {
		require.NoDirExists(t, root, "a disposable run must not leave its runtime roots behind")
	}
}

// TestRuntimeRootIsWithinTheSweepOfCodeflyClear: a killed run's root is
// collected by the caller's runstate.Sweep of runstate.Root(), which only
// looks so deep. This agent's layout, <root>/redis/<state>, is inside it.
func TestRuntimeRootIsWithinTheSweepOfCodeflyClear(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	// The layout on a developer's machine, not this suite's own store.
	suite := redisCacheRoot
	redisCacheRoot = machineRedisCacheRoot
	t.Cleanup(func() { redisCacheRoot = suite })
	root, err := runstate.Root()
	require.NoError(t, err)
	runtimeRoot, err := redisRuntimeRoot(redisStateKey("/work/shop/redis", "invkilled"))
	require.NoError(t, err)
	replicaRoot, err := redisRuntimeRoot(redisReplicaStateKey(redisStateKey("/work/shop/redis", "invkilled"), 1))
	require.NoError(t, err)
	for _, dir := range []string{runtimeRoot, replicaRoot} {
		abandonedRoot(t, filepath.Dir(dir), filepath.Base(dir), "invkilled")
	}

	swept, err := runstate.Sweep(root, runstate.SweepOptions{})
	require.NoError(t, err)
	var dirs []string
	for _, s := range swept {
		dirs = append(dirs, s.Dir)
	}
	require.ElementsMatch(t, []string{runtimeRoot, replicaRoot}, dirs)
	require.NoDirExists(t, runtimeRoot)
}
