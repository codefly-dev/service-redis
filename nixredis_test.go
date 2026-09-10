package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	owner, err := claimRuntimeRoot(root)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	if _, err = claimRuntimeRoot(root); err == nil {
		t.Fatal("a second invocation adopted runtime state already owned by the first")
	}
	if !strings.Contains(err.Error(), "naming scope") {
		t.Fatalf("refusal does not point at the way out: %v", err)
	}

	// Releasing hands the same state back to the next invocation — that is what
	// makes a reusable scope reusable across restarts.
	releaseRuntimeRoot(owner)
	next, err := claimRuntimeRoot(root)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	releaseRuntimeRoot(next)
}

// The flake and its nix materialization are the same bytes for every
// invocation, so they are shared rather than copied into each scope's root.
func TestSharedFlakeIsOutsideEveryInvocationRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	sharedNixRoot, flakeDir, err := materializeSharedFlake()
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
