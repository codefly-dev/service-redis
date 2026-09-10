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
// for it to answer an authenticated PING.
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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	"github.com/gofrs/flock"
)

//go:embed nix/flake.nix
var redisFlakeNix string

//go:embed nix/flake.lock
var redisFlakeLock string

const (
	// redisReadyTimeout bounds how long a freshly spawned server may take to
	// answer its first probe.
	redisReadyTimeout = 30 * time.Second
	// redisProbeInterval spaces readiness probes.
	redisProbeInterval = 500 * time.Millisecond
	// redisTeardownBudget bounds every teardown. Teardown deliberately ignores
	// the caller's cancellation — a cancelled context is usually the reason we
	// are tearing down, and skipping the stop is what orphans the process — so
	// this budget is the only thing bounding how far a Stop can run past the
	// caller's deadline. It is sized to just cover the runner's own
	// SIGTERM-then-SIGKILL escalation (5s + 2s) plus room to observe the exit,
	// and no longer.
	redisTeardownBudget = 10 * time.Second
)

// redisRunner is the slice of a runner environment the native lifecycle needs.
// newNixRedis is the only constructor and it always supplies a
// runners.NixEnvironment, so production redis is always the nix-provisioned
// binary; naming the two methods used keeps the lifecycle exercisable against a
// real process on a host without nix.
type redisRunner interface {
	Init(ctx context.Context) error
	NewProcess(bin string, args ...string) (runners.Proc, error)
}

// nixRedis runs a native redis server off a nix-provisioned binary.
type nixRedis struct {
	env        redisRunner
	flakeDir   string
	dataDir    string
	configPath string
	port       uint16
	password   string
	out        io.Writer
	// mu guards the lifecycle state below — proc, serverExit and the detached
	// context. Stop mutates all of it and can arrive on any RPC goroutine, so a
	// Stop racing a Destroy would otherwise be a data race on the handle itself.
	mu   sync.Mutex
	proc runners.Proc
	// serverCtx is the context the redis process runs under. It MUST outlive
	// Init: starting redis under the Init RPC's ctx kills it the instant Init
	// returns and that ctx is cancelled. Cancelled only by Stop.
	serverCtx    context.Context
	serverCancel context.CancelFunc
	// serverPath is the absolute redis-server the locked devShell exports.
	// Invoking it by absolute path runs the nix-provisioned redis even if a
	// system redis shadows PATH.
	serverPath string
	// serverExit carries the redis process's terminal result. Readiness and
	// Supervise both observe it, so a server that dies before answering PING
	// fails immediately instead of waiting out the readiness deadline, and a
	// server that dies mid-run is reported instead of silently disappearing.
	serverExit <-chan error
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
// assigned port, and waits until it answers an authenticated PING.
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
	return n.launch(ctx)
}

// launch spawns redis and waits for it to answer, releasing the process again
// if it never does. The rollback is armed from the moment redis exists, so a
// readiness check that fails — or that the caller cancels — cannot return an
// error while leaving a live server holding the assigned port. The data
// directory and config are left in place: a failed start disposes of execution
// resources, not of state.
func (n *nixRedis) launch(ctx context.Context) (err error) {
	defer func() {
		if err == nil {
			return
		}
		// Teardown runs on its own budget: the caller's ctx is very often the
		// reason we are rolling back, and a cancelled ctx must not skip the stop.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTeardownBudget)
		defer cancel()
		err = errors.Join(err, n.Stop(cleanupCtx))
	}()
	if startErr := n.startServer(ctx); startErr != nil {
		return startErr
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

// resolveStore selects redis-server from the PATH the locked devShell exports.
// Anything else — scanning /nix/store, or a bare command name — lets an
// unrelated redis derivation or a host install decide which server runs.
//
// The selection is deliberately not cached on disk. NixEnvironment already owns
// and caches the materialization this reads; a second cache, keyed on a second
// copy of the flake fingerprint, is one more thing that can disagree with the
// environment redis actually runs in — and it would disagree silently.
func (n *nixRedis) resolveStore(ctx context.Context) error {
	w := wool.Get(ctx).In("nixRedis.resolveStore")
	devShellPath, err := n.devShellPath(ctx)
	if err != nil {
		return err
	}
	server, err := lookupExecutable("redis-server", devShellPath)
	if err != nil {
		return fmt.Errorf("locked nix environment in %s does not provide redis-server: %w", n.flakeDir, err)
	}
	n.serverPath = server
	w.Info("resolved redis-server from locked nix environment", wool.Field("server", server))
	return nil
}

// devShellPath returns the PATH the locked devShell exports. This is the same
// value the codefly nix runner resolves binaries against, so the server we pick
// here is the one the flake provisions.
func (n *nixRedis) devShellPath(ctx context.Context) (string, error) {
	dir := n.flakeDir
	if abs, absErr := filepath.Abs(dir); absErr == nil {
		dir = abs
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
		// Stop terminates this server, and the lifecycle contract is that
		// stopping ends execution while the dataset survives. Redis only writes
		// an RDB on SIGTERM when at least one save point is configured; with
		// `save ""` the shutdown path skips the dump and the whole dataset goes
		// with the process. The save point is what makes the retained data dir
		// actually hold anything, and what a later Init reloads.
		"save 60 1",
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
	serverCtx, serverCancel := context.WithCancel(context.Background())
	n.mu.Lock()
	n.serverCtx, n.serverCancel = serverCtx, serverCancel
	n.mu.Unlock()
	if err := proc.Start(serverCtx); err != nil {
		serverCancel()
		return fmt.Errorf("start redis: %w", err)
	}
	exit := make(chan error, 1)
	go func() {
		exit <- proc.Wait(context.Background())
		close(exit)
	}()
	n.mu.Lock()
	n.proc = proc
	n.serverExit = exit
	n.mu.Unlock()
	return nil
}

func (n *nixRedis) serverArgs() []string {
	return []string{n.configPath}
}

// waitReady polls the redis port until the server completes an authenticated
// PING, giving up when the server dies, when the caller cancels, or when the
// readiness budget runs out. The configured password is required: a passworded
// server answers an unauthenticated PING with "-NOAUTH …", which proves the
// socket is open but not that the projected credentials work. A server that has
// already exited is never ready: whatever answers on the port after that belongs
// to someone else.
func (n *nixRedis) waitReady(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", n.port)
	ctx, cancel := context.WithTimeout(ctx, redisReadyTimeout)
	defer cancel()
	var lastErr error
	for {
		if exitErr, exited := n.exited(); exited {
			return redisExitedBeforeReady(exitErr)
		}
		lastErr = probeRedis(ctx, addr, n.password)
		if lastErr == nil {
			if exitErr, exited := n.exited(); exited {
				return redisExitedBeforeReady(exitErr)
			}
			return nil
		}
		// Bad credentials, or a peer that does not speak RESP: no amount of
		// waiting turns either into a redis this agent can drive.
		var probeErr *redisProbeError
		if errors.As(lastErr, &probeErr) && !probeErr.retryable {
			return lastErr
		}
		n.mu.Lock()
		exit := n.serverExit
		n.mu.Unlock()
		select {
		case exitErr, ok := <-exit:
			if !ok {
				exitErr = nil
			}
			return redisExitedBeforeReady(exitErr)
		case <-ctx.Done():
			return fmt.Errorf("redis did not become ready on %s: %w (last probe: %v)", addr, ctx.Err(), lastErr)
		case <-time.After(redisProbeInterval):
		}
	}
}

// exited reports the process's terminal result if it has already exited,
// without blocking.
func (n *nixRedis) exited() (error, bool) {
	n.mu.Lock()
	exit := n.serverExit
	n.mu.Unlock()
	select {
	case err, ok := <-exit:
		if !ok {
			return nil, true
		}
		return err, true
	default:
		return nil, false
	}
}

func redisExitedBeforeReady(err error) error {
	if err == nil {
		return errors.New("redis exited before it became ready")
	}
	return fmt.Errorf("redis exited before it became ready: %w", err)
}

// Supervise reports a termination of the running server that a deliberate Stop
// did not ask for, exactly once. The native runtime is a host process the agent
// owns directly, with no container engine behind it: without this the server can
// die mid-run — a crash, an outside SIGTERM, a port stolen on restart — while
// the agent keeps reporting STARTED and every dependent spins on connection
// refused.
//
// Call it once, after a successful launch: startServer has published serverExit
// and waitReady has finished observing it, so this goroutine is its sole
// remaining reader.
func (n *nixRedis) Supervise(onExit func(error)) {
	n.mu.Lock()
	exit := n.serverExit
	// Stop cancels serverCtx before terminating the process, so a cancelled
	// context is the authoritative "this shutdown was orderly" signal.
	var stopped <-chan struct{}
	if n.serverCtx != nil {
		stopped = n.serverCtx.Done()
	}
	n.mu.Unlock()
	if exit == nil {
		return
	}
	go func() {
		err, ok := <-exit
		if !ok {
			return
		}
		if stopped != nil {
			select {
			case <-stopped:
				return
			default:
			}
		}
		onExit(err)
	}()
}

// Stop terminates the redis server, waits for it to be reaped so the assigned
// port is free by the time Stop returns, and only then releases this
// invocation's claim on its runtime root — in that order, so the claim outlives
// the resource it protects. Config and data are left in place: stopping ends
// execution, it does not dispose of state, and this invocation's own state is
// what a same-scope restart is meant to reuse.
//
// A server that cannot be confirmed dead leaves BOTH the handle and the claim
// in place, and reports why. Dropping the handle orphans a process that may
// still be alive; dropping the claim hands the data dir to another invocation
// while that process still has it open.
func (n *nixRedis) Stop(ctx context.Context) error {
	// Held for the whole teardown: a concurrent Stop and Destroy would otherwise
	// both read the handle, both signal, and both clear it. Serialized, the
	// second one finds no process and is the no-op it should be.
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.proc != nil {
		// Teardown gets its own bounded budget and ignores the caller's
		// cancellation: a cancelled ctx is very often the reason we are tearing
		// down, and skipping the stop is exactly what orphans the process.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTeardownBudget)
		defer cancel()
		stopErr := n.proc.Stop(stopCtx)
		if n.serverExit != nil {
			select {
			case <-n.serverExit:
			case <-stopCtx.Done():
				// serverCancel only *requests* termination, and proc.Stop can
				// give up on a process that refuses to die. A server we cannot
				// confirm dead may still hold the port and the data dir, so
				// neither the handle nor the claim may be given up here.
				return errors.Join(stopErr, fmt.Errorf("redis on port %d did not terminate: %w", n.port, stopCtx.Err()))
			}
		}
		if stopErr != nil {
			return stopErr
		}
		n.proc = nil
		n.serverExit = nil
	}
	if n.owner != nil {
		releaseRuntimeRoot(n.owner)
		n.owner = nil
	}
	return nil
}
