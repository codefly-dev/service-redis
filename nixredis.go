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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

//go:embed nix/flake.nix
var redisFlakeNix string

//go:embed nix/flake.lock
var redisFlakeLock string

// nixRedis runs a native redis server off a nix-provisioned binary.
type nixRedis struct {
	env        *runners.NixEnvironment
	flakeDir   string
	dataDir    string
	configPath string
	port       uint16
	password   string
	out        io.Writer
	proc       runners.Proc
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

// newNixRedis materializes the embedded flake and keeps all mutable state in a
// private per-service user cache directory. Keeping Nix inputs, cache files,
// Redis data, and the secret-bearing config out of the source checkout avoids
// invalidating parent flakes and accidentally committing runtime state.
func newNixRedis(ctx context.Context, baseDir string, port uint16, password string, out io.Writer) (*nixRedis, error) {
	runtimeRoot, err := redisRuntimeRoot(baseDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create redis runtime root: %w", err)
	}
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("secure redis runtime root: %w", err)
	}
	flakeDir := filepath.Join(runtimeRoot, "nix")
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
	env.WithCacheDir(filepath.Join(runtimeRoot, ".nix-cache"))
	return &nixRedis{
		env:        env,
		flakeDir:   flakeDir,
		dataDir:    filepath.Join(runtimeRoot, "data"),
		configPath: filepath.Join(runtimeRoot, "redis.conf"),
		port:       port,
		password:   password,
		out:        out,
	}, nil
}

func redisServiceHash(baseDir string) string {
	sum := sha256.Sum256([]byte(baseDir))
	return hex.EncodeToString(sum[:])
}

func redisRuntimeRoot(baseDir string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "codefly", "redis", redisServiceHash(baseDir)[:16]), nil
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
	if err := os.MkdirAll(n.dataDir, 0o700); err != nil {
		return fmt.Errorf("create redis data dir: %w", err)
	}
	if err := n.writeConfig(); err != nil {
		return err
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

// writeConfig keeps the password out of process argv (and therefore ps/process
// inspection). The parent runtime directory and this file are owner-only.
func (n *nixRedis) writeConfig() error {
	lines := []string{
		"port " + strconv.Itoa(int(n.port)),
		"bind 127.0.0.1",
		"protected-mode yes",
		"dir " + strconv.Quote(n.dataDir),
		`save ""`,
		"appendonly no",
		"daemonize no",
	}
	if n.password != "" {
		lines = append(lines, "requirepass "+strconv.Quote(n.password))
	}
	contents := []byte(strings.Join(lines, "\n") + "\n")
	file, err := os.OpenFile(n.configPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create redis config: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure redis config: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write redis config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close redis config: %w", err)
	}
	return nil
}

// startServer launches redis-server with only the private config path in argv.
func (n *nixRedis) startServer(ctx context.Context) error {
	proc, err := n.env.NewProcess(filepath.Join(n.binDir, "redis-server"), n.serverArgs()...)
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

func (n *nixRedis) serverArgs() []string {
	return []string{n.configPath}
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
