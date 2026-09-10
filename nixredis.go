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
// for it to answer PING.
//
// Both runtimes serve on the same assigned port, so the rest of the agent
// (WaitForReady, connection strings) is unchanged.
//
// On-disk state is split by mutability. The embedded flake and its nix
// materialization cache are immutable and identical for every invocation, so
// they live in one content-addressed root shared by all of them. The
// credential-bearing redis.conf and the data dir are per-session: their root is
// keyed by the service location AND the codefly naming scope, so two concurrent
// invocations of the same service never write each other's config or data.

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
	"github.com/gofrs/flock"
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
	// owner is the exclusive lock this invocation holds on runtimeRoot for the
	// whole session. It is what makes the config and data below runtimeRoot
	// *this* invocation's resources: a second invocation that would land on the
	// same root is refused rather than silently rewriting them. Released by Stop
	// (and by the kernel if the agent dies).
	owner *flock.Flock
	// sharedNixRoot holds the immutable flake and its materialization cache,
	// shared by every invocation on this host.
	sharedNixRoot string
}

// newNixRedis prepares a native redis whose mutable, credential-bearing state
// (redis.conf, data) lives in a private user cache directory owned exclusively
// by this invocation, and whose immutable nix inputs are shared with every
// other invocation on the host. Keeping all of it out of the source checkout
// avoids invalidating parent flakes and committing runtime state.
//
// stateKey comes from redisStateKey: the service location plus the codefly
// naming scope. It never contains the password.
func newNixRedis(ctx context.Context, stateKey string, port uint16, password string, out io.Writer) (*nixRedis, error) {
	runtimeRoot, err := redisRuntimeRoot(stateKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create redis runtime root: %w", err)
	}
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("secure redis runtime root: %w", err)
	}
	owner, err := claimRuntimeRoot(runtimeRoot)
	if err != nil {
		return nil, err
	}
	sharedNixRoot, flakeDir, err := materializeSharedFlake()
	if err != nil {
		releaseRuntimeRoot(owner)
		return nil, err
	}
	env, err := runners.NewNixEnvironment(ctx, flakeDir)
	if err != nil {
		releaseRuntimeRoot(owner)
		return nil, fmt.Errorf("nix environment (is nix installed?): %w", err)
	}
	env.WithCacheDir(filepath.Join(sharedNixRoot, "cache"))
	return &nixRedis{
		env:           env,
		flakeDir:      flakeDir,
		dataDir:       filepath.Join(runtimeRoot, "data"),
		configPath:    filepath.Join(runtimeRoot, "redis.conf"),
		port:          port,
		password:      password,
		out:           out,
		owner:         owner,
		sharedNixRoot: sharedNixRoot,
	}, nil
}

// redisStateKey identifies the mutable state a redis invocation owns. An
// unscoped run keeps the historical per-location key, so an existing user's
// data and config stay exactly where they are across this upgrade. A run that
// codefly gave a naming scope — the carrier for a disposable invocation
// identity — gets its own key, so concurrent invocations of the same service
// cannot read or truncate each other's credential-bearing config.
func redisStateKey(serviceLocation, namingScope string) string {
	if namingScope == "" {
		return serviceLocation
	}
	return serviceLocation + "\x00naming-scope\x00" + namingScope
}

func redisServiceHash(stateKey string) string {
	sum := sha256.Sum256([]byte(stateKey))
	return hex.EncodeToString(sum[:])
}

// redisRuntimeRoot is the out-of-source root for one invocation's mutable
// state, keyed by its scoped state identity so same-scope restarts reuse their
// data while different scopes never meet.
func redisRuntimeRoot(stateKey string) (string, error) {
	cache, err := redisCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, redisServiceHash(stateKey)[:16]), nil
}

func redisCacheRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "codefly", "redis"), nil
}

// claimRuntimeRoot takes the invocation's exclusive lock on its mutable state.
// Two invocations only ever land on the same root when they share a state key,
// which means an operator asked for reusable state; adopting it concurrently is
// ambiguous — one would overwrite the other's config while it is being read —
// so the second is refused with the way out.
func claimRuntimeRoot(runtimeRoot string) (*flock.Flock, error) {
	owner := flock.New(filepath.Join(runtimeRoot, ".owner.lock"), flock.SetPermissions(0o600))
	held, err := owner.TryLock()
	if err != nil {
		_ = owner.Close()
		return nil, fmt.Errorf("claim redis runtime root %s: %w", runtimeRoot, err)
	}
	if !held {
		_ = owner.Close()
		return nil, fmt.Errorf("redis runtime state %s is already in use by another codefly invocation: give this run its own naming scope to isolate it", runtimeRoot)
	}
	return owner, nil
}

func releaseRuntimeRoot(owner *flock.Flock) {
	_ = owner.Unlock()
	_ = owner.Close()
}

// materializeSharedFlake writes the embedded flake to a root addressed by its
// own contents and returns (shared root, flake dir). The flake and the nix
// materialization it produces are immutable and identical for every invocation,
// so they are shared rather than duplicated per session — a fresh scope pays
// for redis.conf and a data dir, not for another nix download.
//
// The two files are written through a rename so a concurrent nix evaluation
// reads either the previous or the complete new file, never a truncated one.
func materializeSharedFlake() (string, string, error) {
	cache, err := redisCacheRoot()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(redisFlakeNix + "\x00" + redisFlakeLock))
	sharedNixRoot := filepath.Join(cache, "nix", hex.EncodeToString(sum[:])[:16])
	flakeDir := filepath.Join(sharedNixRoot, "flake")
	if err := os.MkdirAll(flakeDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create nix flake dir: %w", err)
	}
	for name, contents := range map[string]string{"flake.nix": redisFlakeNix, "flake.lock": redisFlakeLock} {
		if err := writeFileAtomic(filepath.Join(flakeDir, name), []byte(contents), 0o644); err != nil {
			return "", "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	return sharedNixRoot, flakeDir, nil
}

// writeFileAtomic replaces path with contents in one rename, so a reader either
// sees the previous file or the complete new one.
func writeFileAtomic(path string, contents []byte, perm os.FileMode) error {
	dir, name := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Init materializes the nix env, locates redis-server, launches it bound to the
// assigned port, and waits until it answers PING.
func (n *nixRedis) Init(ctx context.Context) error {
	if err := n.initSharedEnv(ctx); err != nil {
		return err
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

// initSharedEnv materializes the shared nix env under an exclusive lock.
// Materialization writes a devshell env cache and a GC-root profile in the
// shared root; concurrent invocations evaluating the same flake would race on
// both. The lock is held only for materialization — the per-session config and
// data below runtimeRoot are never covered by it.
func (n *nixRedis) initSharedEnv(ctx context.Context) error {
	materialization := flock.New(filepath.Join(n.sharedNixRoot, "materialize.lock"), flock.SetPermissions(0o600))
	if _, err := materialization.TryLockContext(ctx, 50*time.Millisecond); err != nil {
		_ = materialization.Close()
		return fmt.Errorf("lock nix redis materialization in %s: %w", n.sharedNixRoot, err)
	}
	defer func() {
		_ = materialization.Unlock()
		_ = materialization.Close()
	}()
	if err := n.env.Init(ctx); err != nil {
		return fmt.Errorf("materialize nix redis env: %w", err)
	}
	return nil
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
// inspection). The parent runtime directory and this file are owner-only, and
// the file is replaced in one rename so a redis-server reading it can never see
// a half-written config.
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
	if err := writeFileAtomic(n.configPath, contents, 0o600); err != nil {
		return fmt.Errorf("write redis config: %w", err)
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

// Stop terminates the redis server process and releases this invocation's claim
// on its runtime root. Config and data are left in place: another invocation's
// state is never reachable from here, and this invocation's own state is what a
// same-scope restart is meant to reuse.
func (n *nixRedis) Stop(ctx context.Context) error {
	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.owner != nil {
		releaseRuntimeRoot(n.owner)
		n.owner = nil
	}
	if n.proc == nil {
		return nil
	}
	return n.proc.Stop(ctx)
}
