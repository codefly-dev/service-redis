package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
