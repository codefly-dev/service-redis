package main

// runtime_parity_test.go — both runtimes serve the one redis this agent pins.
//
// The Docker runtime serves the image runtime-image.json locks; the nix runtime
// serves whatever nixpkgs revision nix/flake.lock resolves. Nothing else ties
// the two together, and the conformance suite reports one verdict over both, so
// a drift between them turns a version difference into what reads as a flake
// on one target. These tests fail the drift by name instead: the lock's tag
// spells the version it pins, and each runtime is held to it.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pinnedRedisTag reads the redis version out of the lock's tag, which spells
// out what the image pins (redis-8.8.0-openssl-...).
var pinnedRedisTag = regexp.MustCompile(`^redis-(\d+\.\d+\.\d+)-`)

func pinnedRedisVersion(t *testing.T) string {
	t.Helper()
	match := pinnedRedisTag.FindStringSubmatch(runtimeImage.Tag)
	if match == nil {
		t.Fatalf("runtime-image.json tag %q does not spell the redis version it pins", runtimeImage.Tag)
	}
	return match[1]
}

// TestRealRedisRuntimesServeThePinnedRedis starts redis through the Runtime
// over each backend and holds what it reports to the lock. The docker row
// proves the lock's tag describes the image it locks; the nix row is the parity
// rule in AGENTS.md as a check rather than a reminder.
func TestRealRedisRuntimesServeThePinnedRedis(t *testing.T) {
	pinned := pinnedRedisVersion(t)
	for _, backend := range redisBackends {
		r := startRuntimeRedis(t, backend, 0, fmt.Sprintf("parity-%s-3VbNx5", backend))
		if r.version == pinned {
			continue
		}
		switch backend {
		case dockerBackend:
			t.Errorf("docker runtime serves redis %s, but runtime-image.json's tag %q pins %s: the lock's tag no longer describes the image it locks", r.version, runtimeImage.Tag, pinned)
		case nixBackend:
			t.Errorf("nix runtime serves redis %s, the lock pins %s: move nix/flake.nix to a nixpkgs revision shipping %s (runtime-image-pin skill, Parity)", r.version, pinned, pinned)
		}
	}
}

// nixSystemOf maps an OCI platform the lock can name to the nix system that
// serves it.
var nixSystemOf = map[string]string{"linux/amd64": "x86_64-linux", "linux/arm64": "aarch64-linux"}

var flakeSystems = regexp.MustCompile(`systems\s*=\s*\[([^\]]*)\]`)

// TestNixFlakeCoversEveryRuntimeImagePlatform keeps the platform lists in
// step: CI builds the image for exactly the lock's platforms, and the native
// runtime has to be able to serve on each of them.
func TestNixFlakeCoversEveryRuntimeImagePlatform(t *testing.T) {
	match := flakeSystems.FindStringSubmatch(redisFlakeNix)
	if match == nil {
		t.Fatal("nix/flake.nix declares no systems list")
	}
	systems := strings.Fields(strings.ReplaceAll(match[1], `"`, " "))
	for _, platform := range runtimeImage.Platforms {
		system, known := nixSystemOf[platform]
		if !known {
			t.Errorf("runtime-image.json ships %s, which nixSystemOf maps to no nix system", platform)
			continue
		}
		if !slices.Contains(systems, system) {
			t.Errorf("runtime-image.json ships %s but nix/flake.nix declares no %s: the native runtime cannot serve where the image does", platform, system)
		}
	}
}
