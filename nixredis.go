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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
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
	// serverPath is the absolute redis-server the locked devShell exports.
	// Invoking it by absolute path runs the nix-provisioned redis even if a
	// system redis shadows PATH.
	serverPath string
	// resolutionPath caches serverPath against the flake fingerprint so a
	// restart on an unchanged lock skips re-evaluating the flake.
	resolutionPath string
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
	if err := prepareRuntimeRoot(runtimeRoot); err != nil {
		return nil, err
	}
	owner, err := claimRuntimeRoot(ctx, runtimeRoot, runtimeRootClaimTimeout)
	if err != nil {
		return nil, err
	}
	releaseLegacyNixCache(runtimeRoot, out)
	sharedNixRoot, flakeDir, err := materializeSharedFlake(ctx, out)
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
		env:            env,
		flakeDir:       flakeDir,
		dataDir:        filepath.Join(runtimeRoot, "data"),
		configPath:     filepath.Join(runtimeRoot, "redis.conf"),
		resolutionPath: filepath.Join(runtimeRoot, "redis-server.json"),
		port:           port,
		password:       password,
		out:            out,
		owner:          owner,
		sharedNixRoot:  sharedNixRoot,
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

// runtimeRootClaimTimeout bounds the wait for a previous invocation of the same
// reusable state to finish shutting down. Restarting a run — stop it, start it
// again — hands the root over while the old agent process is still exiting and
// still holds the lock, and that handover is not a collision. Only a peer that
// is still holding the root after this long is one.
const runtimeRootClaimTimeout = 5 * time.Second

// prepareRuntimeRoot creates the invocation's mutable root and refuses to use
// one that is not a real directory we own. MkdirAll and Chmod both follow
// symlinks, so without the Lstat a pre-planted symlink at this path redirects
// the credential-bearing config into whatever it points at — and the path is
// predictable, being derived from a hash of public inputs.
func prepareRuntimeRoot(runtimeRoot string) error {
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return fmt.Errorf("create redis runtime root: %w", err)
	}
	info, err := os.Lstat(runtimeRoot)
	if err != nil {
		return fmt.Errorf("inspect redis runtime root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("redis runtime root %s is a symlink; refusing to write credentials through it", runtimeRoot)
	}
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		return fmt.Errorf("secure redis runtime root: %w", err)
	}
	return nil
}

// claimRuntimeRoot takes the invocation's exclusive lock on its mutable state.
// Two invocations only ever land on the same root when they share a state key,
// which means an operator asked for reusable state; adopting it concurrently is
// ambiguous — one would overwrite the other's config while it is being read —
// so the second is refused with the way out, but only after waiting long enough
// for an ordinary restart to complete its handover.
func claimRuntimeRoot(ctx context.Context, runtimeRoot string, wait time.Duration) (*flock.Flock, error) {
	owner := flock.New(filepath.Join(runtimeRoot, ".owner.lock"), flock.SetPermissions(0o600))
	claimCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	held, err := owner.TryLockContext(claimCtx, 50*time.Millisecond)
	if held {
		return owner, nil
	}
	_ = owner.Close()
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("redis runtime state %s is still held by another codefly invocation after %s: stop that run, or give this one its own naming scope to isolate it", runtimeRoot, wait)
	}
	return nil, fmt.Errorf("claim redis runtime root %s: %w", runtimeRoot, err)
}

// releaseLegacyNixCache drops the per-service nix state written by releases
// that kept the flake and its materialization under the service's own runtime
// root. That materialization is rooted by a nix profile registered under
// /nix/var/nix/gcroots/auto, so leaving it behind pins its whole closure —
// gigabytes per service — against nix-collect-garbage with nothing referencing
// it. Removing the profile makes the auto-root dangling, which nix then
// collects. Only these two agent-owned directories are touched; redis.conf and
// the data dir are never candidates.
//
// Best effort by design: this reclaims disk, and failing a service start over
// disk reclamation would be a worse outcome than the leak. The failure is
// reported rather than swallowed so a host that keeps leaking is diagnosable.
func releaseLegacyNixCache(runtimeRoot string, out io.Writer) {
	for _, legacy := range []string{"nix", ".nix-cache"} {
		if err := os.RemoveAll(filepath.Join(runtimeRoot, legacy)); err != nil && out != nil {
			_, _ = fmt.Fprintf(out, "could not release legacy nix state %s: %v\n", filepath.Join(runtimeRoot, legacy), err)
		}
	}
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
// The write happens under the shared lock and only when the contents differ,
// which after the first invocation is never. Nix consumes flakeDir as a
// `path:` flake and copies the WHOLE directory into the store, so a transient
// file there changes the flake's hash: nothing may appear in it that is not
// part of the flake, and no write may overlap an evaluation.
func materializeSharedFlake(ctx context.Context, out io.Writer) (string, string, error) {
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
	err = withSharedNixLock(ctx, sharedNixRoot, out, func() error {
		for name, contents := range map[string]string{"flake.nix": redisFlakeNix, "flake.lock": redisFlakeLock} {
			path := filepath.Join(flakeDir, name)
			if current, readErr := os.ReadFile(path); readErr == nil && string(current) == contents {
				continue
			}
			if writeErr := os.WriteFile(path, []byte(contents), 0o644); writeErr != nil {
				return fmt.Errorf("write %s: %w", name, writeErr)
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return sharedNixRoot, flakeDir, nil
}

// withSharedNixLock serializes every access to the shared nix root: writing the
// flake, and evaluating it. Materialization writes a devshell env cache and a
// GC-root profile that concurrent evaluations would race on, and an evaluation
// must never observe a flake mid-write.
//
// The wait is deliberately bounded only by ctx. A peer holding this lock is
// doing the exact materialization the waiter would otherwise do itself — on a
// cold binary cache that is minutes — so failing early would trade a wait for a
// duplicated download. It announces the wait instead of looking like a hang.
func withSharedNixLock(ctx context.Context, sharedNixRoot string, out io.Writer, operation func() error) error {
	lock := flock.New(filepath.Join(sharedNixRoot, "materialize.lock"), flock.SetPermissions(0o600))
	held, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock shared nix root %s: %w", sharedNixRoot, err)
	}
	if !held {
		if out != nil {
			_, _ = fmt.Fprintf(out, "waiting for another codefly invocation to materialize the nix redis environment in %s\n", sharedNixRoot)
		}
		if _, err = lock.TryLockContext(ctx, 250*time.Millisecond); err != nil {
			_ = lock.Close()
			return fmt.Errorf("lock shared nix root %s: %w", sharedNixRoot, err)
		}
	}
	defer func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}()
	return operation()
}

// writeFileAtomic replaces path with contents in one rename, so a reader either
// sees the previous file or the complete new one. Only for mutable files whose
// directory is not consumed wholesale by another tool — see materializeSharedFlake.
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
//
// A failed Init releases everything it acquired, including the runtime-root
// claim taken by newNixRedis. The caller drops the *nixRedis on error and can
// never call Stop on it, so anything still held here is held until the agent
// process exits: the claim would make every later attempt in this process fail
// with "already in use by another codefly invocation" — naming a cause that is
// not the real one and prescribing a fix that cannot work — and a redis started
// just before a readiness timeout would keep running, holding the port and the
// data dir, unreferenced.
func (n *nixRedis) Init(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			// Not ctx: Init may be failing *because* ctx was cancelled, and the
			// process still has to be reaped.
			_ = n.Stop(context.Background())
		}
	}()
	if err := n.initSharedEnv(ctx); err != nil {
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

// initSharedEnv materializes the shared nix env and resolves redis-server out
// of it, both under the shared lock. The lock covers the shared root only — the
// per-session config and data below runtimeRoot are never held by it. Resolving
// here rather than after is what keeps every evaluation of the shared flake
// serialized: resolveStore evaluates it too.
func (n *nixRedis) initSharedEnv(ctx context.Context) error {
	return withSharedNixLock(ctx, n.sharedNixRoot, n.out, func() error {
		if err := n.env.Init(ctx); err != nil {
			return fmt.Errorf("materialize nix redis env: %w", err)
		}
		return n.resolveStore(ctx)
	})
}

// resolvedServer is the on-disk record of which redis-server a given
// materialization of the flake selected.
type resolvedServer struct {
	Fingerprint string `json:"fingerprint"`
	ServerPath  string `json:"server_path"`
}

// resolveStore selects redis-server from the PATH the locked devShell exports.
// Anything else — scanning /nix/store, or a bare command name — lets an
// unrelated redis derivation or a host install decide which server runs.
func (n *nixRedis) resolveStore(ctx context.Context) error {
	w := wool.Get(ctx).In("nixRedis.resolveStore")
	fingerprint, err := n.flakeFingerprint()
	if err != nil {
		return err
	}
	if cached := n.cachedServer(fingerprint); cached != "" {
		n.serverPath = cached
		w.Debug("reusing resolved redis-server", wool.Field("server", cached))
		return nil
	}
	devShellPath, err := n.devShellPath(ctx)
	if err != nil {
		return err
	}
	server, err := lookupExecutable("redis-server", devShellPath)
	if err != nil {
		return fmt.Errorf("locked nix environment in %s does not provide redis-server: %w", n.flakeDir, err)
	}
	n.serverPath = server
	if err := n.cacheServer(fingerprint, server); err != nil {
		w.Debug("could not record redis-server resolution", wool.ErrField(err))
	}
	w.Info("resolved redis-server from locked nix environment",
		wool.Field("server", server), wool.Field("flake", fingerprint))
	return nil
}

// flakeFingerprint hashes the materialized flake so the cached resolution is
// discarded the moment the flake or its lock changes.
func (n *nixRedis) flakeFingerprint() (string, error) {
	sum := sha256.New()
	for _, name := range []string{"flake.nix", "flake.lock"} {
		data, err := os.ReadFile(filepath.Join(n.flakeDir, name))
		if err != nil {
			return "", fmt.Errorf("fingerprint nix flake: %w", err)
		}
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (n *nixRedis) cachedServer(fingerprint string) string {
	data, err := os.ReadFile(n.resolutionPath)
	if err != nil {
		return ""
	}
	var cached resolvedServer
	if err := json.Unmarshal(data, &cached); err != nil {
		return ""
	}
	if cached.Fingerprint != fingerprint || !isExecutable(cached.ServerPath) {
		return ""
	}
	return cached.ServerPath
}

func (n *nixRedis) cacheServer(fingerprint string, server string) error {
	data, err := json.Marshal(resolvedServer{Fingerprint: fingerprint, ServerPath: server})
	if err != nil {
		return err
	}
	return os.WriteFile(n.resolutionPath, data, 0o600)
}

// devShellPath returns the PATH the locked devShell exports. This is the same
// value the codefly nix runner resolves binaries against, so the server we pick
// here is the one the flake provisions.
func (n *nixRedis) devShellPath(ctx context.Context) (string, error) {
	dir, err := filepath.Abs(n.flakeDir)
	if err != nil {
		return "", fmt.Errorf("resolve nix flake dir: %w", err)
	}
	// #nosec G204
	cmd := exec.CommandContext(ctx, "nix", "--extra-experimental-features", "nix-command flakes",
		"print-dev-env", "--json", "path:"+dir)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("nix print-dev-env failed: %s: %w", strings.TrimSpace(string(exitErr.Stderr)), err)
		}
		return "", fmt.Errorf("nix print-dev-env failed: %w", err)
	}
	var payload struct {
		Variables map[string]struct {
			// Non-scalar entries (bash arrays, functions) carry a value shape
			// that is not a string, so PATH is decoded on its own below.
			Value json.RawMessage `json:"value"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("parse nix print-dev-env json: %w", err)
	}
	// A missing, non-string, or empty PATH all mean the same thing: this
	// devShell cannot say where redis lives.
	var devShellPath string
	if entry, ok := payload.Variables["PATH"]; ok {
		_ = json.Unmarshal(entry.Value, &devShellPath)
	}
	if devShellPath == "" {
		return "", fmt.Errorf("nix devShell for %s exports no PATH", dir)
	}
	return devShellPath, nil
}

// lookupExecutable finds name in pathValue. Empty PATH entries mean "current
// directory" to a shell; they are skipped because the working directory is not
// part of the locked environment.
func lookupExecutable(name string, pathValue string) (string, error) {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q is not on the devShell PATH", name)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
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
	proc, err := n.env.NewProcess(n.serverPath, n.serverArgs()...)
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

// Stop terminates the redis server process and then releases this invocation's
// claim on its runtime root — in that order, so the claim outlives the resource
// it protects. Config and data are left in place: another invocation's state is
// never reachable from here, and this invocation's own state is what a
// same-scope restart is meant to reuse.
func (n *nixRedis) Stop(ctx context.Context) error {
	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.proc != nil {
		if err := n.proc.Stop(ctx); err != nil {
			// Keep the claim. serverCancel only *requests* termination, so a
			// server we cannot confirm dead may still hold the data dir, and
			// handing the root to another invocation now is precisely the
			// collision the claim exists to prevent.
			return err
		}
		n.proc = nil
	}
	if n.owner != nil {
		releaseRuntimeRoot(n.owner)
		n.owner = nil
	}
	return nil
}
