package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/gofrs/flock"
)

// TestNixRedisConcurrentInvocationsAreIsolated runs two real nix-provisioned
// redis servers for the same service directory at the same time, the way two
// codefly flows of one workspace do. Before invocation scope reached the native
// runtime they shared one redis.conf and one data dir, so whichever started
// second overwrote the first's credentials while it was being read.
//
// Requires a working nix; skipped (never silently passed) when it is missing.
func TestNixRedisConcurrentInvocationsAreIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real redis servers off a nix materialization")
	}
	if !runners.CheckNixInstalled() {
		t.Skipf("nix is not installed: %s", runners.NixInstallCommand())
	}

	ctx := context.Background()
	location := t.TempDir()
	sessions := []struct {
		scope    string
		password string
	}{
		{scope: "inv" + uniqueScopeSuffix(t) + "a", password: "alpha-secret-4d1f"},
		{scope: "inv" + uniqueScopeSuffix(t) + "b", password: "beta-secret-9c72"},
	}

	servers := make([]*nixRedis, len(sessions))
	ports := make([]uint16, len(sessions))
	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, session := range sessions {
		ports[i] = freePort(t)
		wg.Add(1)
		go func(i int, scope, password string) {
			defer wg.Done()
			server, err := newNixRedis(ctx, redisStateKey(location, scope), ports[i], password, nil)
			if err != nil {
				errs[i] = err
				return
			}
			servers[i] = server
			// Materialization can pull redis from a binary cache on a cold host.
			initCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			errs[i] = server.Init(initCtx)
		}(i, session.scope, session.password)
	}
	wg.Wait()

	// t.Cleanup runs LIFO, so the removal is registered FIRST and the stop
	// second: the server is reaped before its state is deleted. Registering
	// every session before asserting on any error keeps a failure in one from
	// stranding a claimed, populated root belonging to the other.
	for _, server := range servers {
		if server != nil {
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(server.configPath)) })
			t.Cleanup(func() { _ = server.Stop(context.Background()) })
		}
	}
	for i := range servers {
		if errs[i] != nil {
			t.Fatalf("session %d: %v", i, errs[i])
		}
	}

	// Distinct mutable roots — the identity fix — while the immutable nix inputs
	// are the one shared materialization.
	if servers[0].configPath == servers[1].configPath {
		t.Fatalf("both invocations wrote the same config %q", servers[0].configPath)
	}
	if servers[0].dataDir == servers[1].dataDir {
		t.Fatalf("both invocations wrote the same data dir %q", servers[0].dataDir)
	}
	if servers[0].flakeDir != servers[1].flakeDir {
		t.Fatalf("immutable nix inputs were duplicated: %q vs %q", servers[0].flakeDir, servers[1].flakeDir)
	}

	for i, session := range sessions {
		for _, path := range []string{servers[i].configPath, servers[i].dataDir} {
			if strings.Contains(path, session.password) {
				t.Fatalf("session %d path %q leaks the password", i, path)
			}
		}
		assertPrivate(t, servers[i].configPath, 0o600)
		assertPrivate(t, servers[i].dataDir, 0o700)
		assertConfigRequires(t, servers[i].configPath, session.password)
	}

	// Each live server authenticates with its own password and rejects the
	// other's — proof that neither config was rewritten by the other's start.
	for i, session := range sessions {
		other := sessions[1-i]
		if reply := redisAuth(t, ports[i], session.password); !strings.HasPrefix(reply, "+OK") {
			t.Fatalf("session %d rejected its own password: %s", i, reply)
		}
		if reply := redisAuth(t, ports[i], other.password); !strings.HasPrefix(reply, "-") {
			t.Fatalf("session %d accepted the other invocation's password: %s", i, reply)
		}
	}

	// Stopping one invocation releases only its own resources: the other keeps
	// serving, and neither session's state is deleted.
	if err := servers[0].Stop(ctx); err != nil {
		t.Fatalf("stop session 0: %v", err)
	}
	if reply := redisAuth(t, ports[1], sessions[1].password); !strings.HasPrefix(reply, "+OK") {
		t.Fatalf("stopping one invocation disturbed the other: %s", reply)
	}
	for i, session := range sessions {
		assertConfigRequires(t, servers[i].configPath, session.password)
		if _, err := os.Stat(servers[i].dataDir); err != nil {
			t.Fatalf("session %d data dir was removed by the other's Stop: %v", i, err)
		}
	}

	// The stopped invocation released its claim, so the same scope is reusable.
	reclaimed, err := claimRuntimeRoot(ctx, filepath.Dir(servers[0].configPath), time.Second)
	if err != nil {
		t.Fatalf("a stopped invocation kept ownership of its runtime root: %v", err)
	}
	releaseRuntimeRoot(reclaimed)
}

// uniqueScopeSuffix keeps concurrent runs of this test — and a developer's own
// codefly sessions on the same host — off each other's runtime roots.
func uniqueScopeSuffix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err = listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

func redisAuth(t *testing.T, port uint16, password string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial redis on %d: %v", port, err)
	}
	defer func() { _ = conn.Close() }()
	command := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password)
	if _, err = conn.Write([]byte(command)); err != nil {
		t.Fatalf("send AUTH: %v", err)
	}
	if err = conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read AUTH reply: %v", err)
	}
	return strings.TrimSpace(reply)
}

func assertPrivate(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %o, want %o", path, got, want)
	}
}

func assertConfigRequires(t *testing.T, configPath, password string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "requirepass " + fmt.Sprintf("%q", password); !strings.Contains(string(data), want) {
		t.Fatalf("%s does not carry its own credentials; it was overwritten by another invocation:\n%s", configPath, data)
	}
}

// A failed Init must hand back everything newNixRedis took. The caller drops
// the *nixRedis on error and can never call Stop on it, so a claim retained
// here outlives the attempt: the next Init in this process would be refused
// with "already in use by another codefly invocation", blaming a peer that does
// not exist for what was a transient materialization failure.
func TestNixRedisFailedInitReleasesItsClaim(t *testing.T) {
	if !runners.CheckNixInstalled() {
		t.Skipf("nix is not installed: %s", runners.NixInstallCommand())
	}

	location := t.TempDir()
	server, err := newNixRedis(context.Background(),
		redisStateKey(location, "inv"+uniqueScopeSuffix(t)+"fail"), freePort(t), "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Dir(server.configPath)
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })

	// Hold the shared nix lock so Init cannot materialize, and give it a
	// cancelled context so it fails at that point instead of waiting.
	blocker := flock.New(filepath.Join(server.sharedNixRoot, "materialize.lock"), flock.SetPermissions(0o600))
	held, err := blocker.TryLock()
	if err != nil || !held {
		t.Fatalf("could not hold the shared nix lock: held=%v err=%v", held, err)
	}
	defer func() { _ = blocker.Unlock(); _ = blocker.Close() }()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = server.Init(cancelled); err == nil {
		t.Fatal("Init reported success while the shared nix root was locked away from it")
	}

	reclaimed, err := claimRuntimeRoot(context.Background(), runtimeRoot, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a failed Init leaked the runtime-root claim: %v", err)
	}
	releaseRuntimeRoot(reclaimed)
}

// stubborn is a process that refuses to confirm it stopped.
type stubborn struct{ runners.Proc }

func (stubborn) Stop(context.Context) error { return errors.New("process did not terminate") }

// serverCancel only *requests* termination. Releasing the claim before the
// process is confirmed gone hands the root — and the data dir the server still
// has open — to another invocation, which is the collision the claim exists to
// prevent.
func TestNixRedisStopKeepsItsClaimWhenTheServerWillNotDie(t *testing.T) {
	runtimeRoot := t.TempDir()
	owner, err := claimRuntimeRoot(context.Background(), runtimeRoot, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	server := &nixRedis{owner: owner, proc: stubborn{}, configPath: filepath.Join(runtimeRoot, "redis.conf")}

	if err = server.Stop(context.Background()); err == nil {
		t.Fatal("Stop reported success for a process it could not terminate")
	}
	if server.owner == nil {
		t.Fatal("Stop released the claim while its server may still be running")
	}

	if _, err = claimRuntimeRoot(context.Background(), runtimeRoot, 200*time.Millisecond); err == nil {
		t.Fatal("another invocation adopted a root whose server was never confirmed dead")
	}
	releaseRuntimeRoot(server.owner)
}
