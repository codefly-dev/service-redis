package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testServiceLocation = "/workspace/modules/infra/services/redis"

// An unscoped run must land on exactly the directory earlier releases used, so
// upgrading never strands a developer's existing redis data somewhere else.
func TestRedisStateKeyPreservesLegacyUnscopedLocation(t *testing.T) {
	if got := redisStateKey(testServiceLocation, ""); got != testServiceLocation {
		t.Fatalf("unscoped state key = %q, want the historical location %q", got, testServiceLocation)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testServiceLocation))
	legacy := filepath.Join(cache, "codefly", "redis", hex.EncodeToString(sum[:])[:16])

	root, err := redisRuntimeRoot(redisStateKey(testServiceLocation, ""))
	if err != nil {
		t.Fatal(err)
	}
	if root != legacy {
		t.Fatalf("unscoped runtime root moved from %q to %q; existing data would be stranded", legacy, root)
	}
}

func TestRedisStateKeySeparatesInvocationScopes(t *testing.T) {
	first, err := redisRuntimeRoot(redisStateKey(testServiceLocation, "inv2yke77n7"))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := redisRuntimeRoot(redisStateKey(testServiceLocation, "inv2yke77n7"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := redisRuntimeRoot(redisStateKey(testServiceLocation, "inv9qb31xza"))
	if err != nil {
		t.Fatal(err)
	}
	unscoped, err := redisRuntimeRoot(redisStateKey(testServiceLocation, ""))
	if err != nil {
		t.Fatal(err)
	}

	if first != repeated {
		t.Fatalf("the same invocation scope produced unstable roots %q and %q", first, repeated)
	}
	for _, other := range []string{second, unscoped} {
		if first == other {
			t.Fatalf("distinct invocation identities shared runtime root %q", first)
		}
	}
}

func TestRedisRuntimeRootIsStableAndOutsideSource(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	source := filepath.Join(t.TempDir(), "workspace", "services", "redis")

	first, err := redisRuntimeRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := redisRuntimeRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("runtime root is not stable: %q != %q", first, second)
	}
	rel, err := filepath.Rel(source, first)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("runtime root %q is inside source %q", first, source)
	}
}

func TestClaimRuntimeRootRefusesConcurrentAdoption(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	owner, err := claimRuntimeRoot(ctx, root, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	if _, err = claimRuntimeRoot(ctx, root, 100*time.Millisecond); err == nil {
		t.Fatal("a second invocation adopted runtime state already owned by the first")
	}
	if !strings.Contains(err.Error(), "naming scope") {
		t.Fatalf("refusal does not point at the way out: %v", err)
	}

	// Releasing hands the same state back to the next invocation — that is what
	// makes a reusable scope reusable across restarts.
	releaseRuntimeRoot(owner)
	next, err := claimRuntimeRoot(ctx, root, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	releaseRuntimeRoot(next)
}

// Restarting a reusable session is stop-then-start, and the old agent process
// still holds the claim while it exits. A claim that gave up instantly would
// turn the most ordinary developer loop — Ctrl-C, run again — into a refusal
// telling the user to pick a naming scope they do not want.
func TestClaimRuntimeRootWaitsOutARestartHandover(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	previous, err := claimRuntimeRoot(ctx, root, time.Second)
	if err != nil {
		t.Fatalf("previous claim: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		releaseRuntimeRoot(previous)
	}()

	start := time.Now()
	restarted, err := claimRuntimeRoot(ctx, root, 5*time.Second)
	if err != nil {
		t.Fatalf("restart was refused instead of waiting for the handover: %v", err)
	}
	releaseRuntimeRoot(restarted)
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("claim returned in %s without waiting; it cannot have observed the handover", waited)
	}
}

// The runtime root path is derived from a hash of public inputs, so it is
// predictable. MkdirAll and Chmod both follow symlinks.
func TestPrepareRuntimeRootRefusesASymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "attacker-controlled")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "runtime-root")
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := prepareRuntimeRoot(root)
	if err == nil {
		t.Fatal("credentials would have been written through a symlinked runtime root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("refusal does not name the cause: %v", err)
	}
}

// Releases before the shared nix root kept the flake and its materialization
// under each service's own runtime root. That materialization is GC-rooted, so
// leaving it behind pins its closure forever with nothing referencing it.
func TestReleaseLegacyNixCacheDropsGCRootsAndKeepsData(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"nix", ".nix-cache", "data"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(root, ".nix-cache", "nix-devshell-profile")
	for _, file := range []string{
		filepath.Join(root, "nix", "flake.nix"),
		profile,
		filepath.Join(root, "data", "dump.rdb"),
		filepath.Join(root, "redis.conf"),
	} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	releaseLegacyNixCache(root, nil)

	for _, gone := range []string{filepath.Join(root, "nix"), filepath.Join(root, ".nix-cache")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("legacy nix state %s survived; its GC root still pins the old closure", gone)
		}
	}
	for _, kept := range []string{filepath.Join(root, "data", "dump.rdb"), filepath.Join(root, "redis.conf")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("cleanup destroyed user state %s: %v", kept, err)
		}
	}
}

// The flake and its nix materialization are the same bytes for every
// invocation, so they are shared rather than copied into each scope's root.
func TestSharedFlakeIsOutsideEveryInvocationRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	sharedNixRoot, flakeDir, err := materializeSharedFlake(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"flake.nix", "flake.lock"} {
		data, readErr := os.ReadFile(filepath.Join(flakeDir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(data) == 0 {
			t.Fatalf("%s was not materialized", name)
		}
	}

	// Nix copies this whole directory into the store as a `path:` flake, so
	// anything else in it changes the flake's hash. A second invocation must
	// also leave the existing files alone rather than rewriting them under a
	// concurrent evaluation.
	before, err := os.Stat(filepath.Join(flakeDir, "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = materializeSharedFlake(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(flakeDir, "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a repeat invocation rewrote the immutable flake; nix may copy it mid-write")
	}
	entries, err := os.ReadDir(flakeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("flake dir holds files that are not the flake: %v", names)
	}
	for _, scope := range []string{"", "inv2yke77n7", "inv9qb31xza"} {
		root, rootErr := redisRuntimeRoot(redisStateKey(testServiceLocation, scope))
		if rootErr != nil {
			t.Fatal(rootErr)
		}
		if rel, relErr := filepath.Rel(root, sharedNixRoot); relErr == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("shared nix root %q lives inside invocation root %q", sharedNixRoot, root)
		}
	}
}

func TestWriteRedisConfigKeepsSecretInPrivateFile(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "redis.conf")
	n := &nixRedis{
		dataDir:    filepath.Join(root, "data"),
		configPath: configPath,
		port:       16379,
		password:   `space and # "quotes"`,
	}
	if err := n.writeConfig(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"port 16379", "bind 127.0.0.1", "requirepass \"space and # \\\"quotes\\\"\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
	if args := n.serverArgs(); strings.Contains(strings.Join(args, " "), n.password) {
		t.Fatal("password leaked into redis-server argv")
	}
}

// A rewrite must never be observable as a truncated file: redis-server reads
// the config at startup, concurrently with whatever else holds this root.
func TestWriteRedisConfigReplacesAtomicallyAndStaysPrivate(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "redis.conf")
	n := &nixRedis{dataDir: filepath.Join(root, "data"), configPath: configPath, port: 16379, password: "first"}
	if err := n.writeConfig(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	n.password = "second"
	if err = n.writeConfig(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("config was rewritten in place; a concurrent reader can see a truncated file")
	}
	if got := after.Mode().Perm(); got != 0o600 {
		t.Fatalf("replaced config permissions = %o, want 600", got)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "redis.conf" {
			t.Fatalf("atomic write left %q behind", entry.Name())
		}
	}
}
