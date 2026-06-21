package main

// nixredis.go — Docker-free redis runtime (mirrors postgres' nixpg.go and
// neo4j's nixneo4j.go).
//
// The redis service agent runs the server in a container by default
// (NewDockerHeadlessEnvironment). On hosts without Docker, the same agent can
// run redis NATIVELY from a nix-provisioned binary: the codefly NixEnvironment
// materializes `redis` from the embedded flake (no system install required),
// and this file drives the native lifecycle — launch `redis-server` bound to
// the agent-assigned port on loopback (with the configured password) and wait
// for it to answer PING. redis is config-light, so unlike neo4j there is no
// writable-conf seeding: everything is passed as CLI flags, and the only
// writable state is the data dir.
//
// Both runtimes serve on the same assigned port, so the rest of the agent
// (WaitForReady, connection strings) is unchanged.

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

//go:embed nix/flake.nix
var redisFlakeNix string

//go:embed nix/flake.lock
var redisFlakeLock string

// nixRedis runs a native redis server off a nix-provisioned binary.
type nixRedis struct {
	env      *runners.NixEnvironment
	flakeDir string
	dataDir  string
	port     uint16
	password string
	out      io.Writer
	proc     runners.Proc
	// serverCtx is the context the redis process runs under. It MUST outlive
	// Init: starting redis under the Init RPC's ctx kills it the instant Init
	// returns and that ctx is cancelled. Cancelled only by Stop.
	serverCtx    context.Context
	serverCancel context.CancelFunc
	// binDir is the absolute nix store bin dir holding redis-server. Invoking it
	// by absolute path runs the nix-built redis even if a system redis shadows
	// PATH.
	binDir string
}

// newNixRedis materializes the embedded flake under baseDir/nix and prepares a
// native redis rooted at baseDir/redis. baseDir is the agent's local service
// dir, so data persists across restarts exactly like the Docker volume.
func newNixRedis(ctx context.Context, baseDir string, port uint16, password string, out io.Writer) (*nixRedis, error) {
	flakeDir := filepath.Join(baseDir, "nix")
	if err := os.MkdirAll(flakeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create nix flake dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), []byte(redisFlakeNix), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.lock"), []byte(redisFlakeLock), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.lock: %w", err)
	}
	env, err := runners.NewNixEnvironment(ctx, flakeDir)
	if err != nil {
		return nil, fmt.Errorf("nix environment (is nix installed?): %w", err)
	}
	env.WithCacheDir(filepath.Join(baseDir, ".nix-cache"))
	return &nixRedis{
		env:      env,
		flakeDir: flakeDir,
		dataDir:  filepath.Join(baseDir, "redis"),
		port:     port,
		password: password,
		out:      out,
	}, nil
}

// Init materializes the nix env, locates redis-server, launches it bound to the
// assigned port, and waits until it answers PING.
func (n *nixRedis) Init(ctx context.Context) error {
	if err := n.env.Init(ctx); err != nil {
		return fmt.Errorf("materialize nix redis env: %w", err)
	}
	if err := n.resolveStore(); err != nil {
		return err
	}
	if err := os.MkdirAll(n.dataDir, 0o755); err != nil {
		return fmt.Errorf("create redis data dir: %w", err)
	}
	if err := n.startServer(ctx); err != nil {
		return err
	}
	return n.waitReady(ctx)
}

// resolveStore locates the nix-store redis-server binary by absolute path —
// rather than a bare command on PATH — so we run the nix-built redis even if a
// system redis shadows PATH.
func (n *nixRedis) resolveStore() error {
	matches, err := filepath.Glob("/nix/store/*-redis-*/bin/redis-server")
	if err != nil {
		return fmt.Errorf("glob nix redis: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no nix redis with bin/redis-server found in /nix/store (materialization may have failed)")
	}
	n.binDir = filepath.Dir(matches[0])
	return nil
}

// startServer launches redis-server bound to loopback on the assigned port. The
// nix store is read-only, so the working/data dir is redirected to dataDir and
// snapshotting is disabled (ephemeral dev runtime).
func (n *nixRedis) startServer(ctx context.Context) error {
	args := []string{
		"--port", strconv.Itoa(int(n.port)),
		"--bind", "127.0.0.1",
		"--dir", n.dataDir,
		"--save", "", // no RDB snapshots
		"--appendonly", "no",
		"--daemonize", "no",
	}
	if n.password != "" {
		args = append(args, "--requirepass", n.password)
	}
	proc, err := n.env.NewProcess(filepath.Join(n.binDir, "redis-server"), args...)
	if err != nil {
		return err
	}
	if n.out != nil {
		proc.WithOutput(n.out)
	}
	// Run redis under a context that outlives Init — NOT the Init RPC ctx, which
	// is cancelled the moment Init returns and would SIGTERM the server.
	n.serverCtx, n.serverCancel = context.WithCancel(context.Background())
	if err := proc.Start(n.serverCtx); err != nil {
		n.serverCancel()
		return fmt.Errorf("start redis: %w", err)
	}
	n.proc = proc
	return nil
}

// waitReady polls the redis port with a PING until it answers. A passworded
// server replies "-NOAUTH …" to an unauthenticated PING, which still proves it
// is up and accepting connections — so any reply counts as ready.
func (n *nixRedis) waitReady(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", n.port)
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_, _ = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
		buf := make([]byte, 16)
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		nr, rerr := conn.Read(buf)
		_ = conn.Close()
		if rerr == nil && nr > 0 {
			return nil
		}
		lastErr = rerr
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("redis did not become ready on %s: %w", addr, lastErr)
}

// Stop terminates the redis server process.
func (n *nixRedis) Stop(ctx context.Context) error {
	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.proc == nil {
		return nil
	}
	return n.proc.Stop(ctx)
}
