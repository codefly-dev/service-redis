package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseRuntimeImageLock(t *testing.T) {
	got, err := parseRuntimeImageLock([]byte(`{
		"name": "ghcr.io/codefly-dev/service-redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907"
	}`))
	if err != nil {
		t.Fatalf("parseRuntimeImageLock: %v", err)
	}
	const want = "ghcr.io/codefly-dev/service-redis@sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907"
	if got.FullName() != want {
		t.Fatalf("FullName() = %q, want %q", got.FullName(), want)
	}
	if got.Tag != "redis-8.8.0-openssl-3.5.8-r0-alpine3.23" {
		t.Fatalf("Tag = %q", got.Tag)
	}
}

func TestParseRuntimeImageLockRejectsIncompleteReference(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "ghcr.io/codefly-dev/service-redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23"
	}`))
	if err == nil || err.Error() != "runtime image digest is required" {
		t.Fatalf("err = %v, want runtime image digest is required", err)
	}
}

func TestParseRuntimeImageLockRejectsNonSHA256Digest(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "ghcr.io/codefly-dev/service-redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha512:deadbeef"
	}`))
	if err == nil || err.Error() != "runtime image digest must be a sha256 digest" {
		t.Fatalf("err = %v, want runtime image digest must be a sha256 digest", err)
	}
}

func TestDefaultImageMatchesRuntimeImageLock(t *testing.T) {
	lock, err := os.ReadFile("runtime-image.json")
	if err != nil {
		t.Fatalf("read runtime-image.json: %v", err)
	}
	expected, err := parseRuntimeImageLock(lock)
	if err != nil {
		t.Fatalf("parseRuntimeImageLock: %v", err)
	}
	if *expected != *image {
		t.Fatalf("image = %+v, want %+v", image, expected)
	}
}

func TestManagedImageIsPatchedGHCRReference(t *testing.T) {
	if image.Name != "ghcr.io/codefly-dev/service-redis" {
		t.Fatalf("image.Name = %q, want ghcr.io/codefly-dev/service-redis", image.Name)
	}
	if !strings.HasPrefix(image.Digest, "sha256:") {
		t.Fatalf("image.Digest = %q, want sha256-pinned reference", image.Digest)
	}
}

func TestRuntimeDockerfilePinsPatchedOpenSSL(t *testing.T) {
	content, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(content)
	for _, want := range []string{
		"# syntax=docker/dockerfile:1.7@sha256:",
		"ARG REDIS_IMAGE=redis:8.8.0-alpine@sha256:",
		"ARG SOURCE_DATE_EPOCH=0",
		"libcrypto3=3.5.8-r0",
		"libssl3=3.5.8-r0",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}
