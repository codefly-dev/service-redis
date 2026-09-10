package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
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

	for i, server := range servers {
		if server != nil {
			t.Cleanup(func() { _ = server.Stop(context.Background()) })
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(server.configPath)) })
		}
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
	reclaimed, err := claimRuntimeRoot(filepath.Dir(servers[0].configPath))
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
