package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseRuntimeImageLock(t *testing.T) {
	got, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907",
		"platforms": ["linux/amd64", "linux/arm64"]
	}`))
	if err != nil {
		t.Fatalf("parseRuntimeImageLock: %v", err)
	}
	const want = "docker.io/codeflydev/redis@sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907"
	if got.FullName() != want {
		t.Fatalf("FullName() = %q, want %q", got.FullName(), want)
	}
	if got.Tag != "redis-8.8.0-openssl-3.5.8-r0-alpine3.23" {
		t.Fatalf("Tag = %q", got.Tag)
	}
	if strings.Join(got.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("Platforms = %v", got.Platforms)
	}
}

func TestParseRuntimeImageLockRejectsMissingPlatforms(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907"
	}`))
	if err == nil || err.Error() != "runtime image platforms are required" {
		t.Fatalf("err = %v, want runtime image platforms are required", err)
	}
}

func TestParseRuntimeImageLockRejectsMalformedPlatform(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907",
		"platforms": ["amd64"]
	}`))
	if err == nil || err.Error() != `runtime image platform "amd64" must be in os/arch form` {
		t.Fatalf("err = %v, want malformed platform rejection", err)
	}
}

func TestParseRuntimeImageLockRejectsDuplicatePlatform(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23",
		"digest": "sha256:254a28dad5239310b81ee9246761f825d055675961c7f960bb9a8a5bc43fd907",
		"platforms": ["linux/amd64", "linux/amd64"]
	}`))
	if err == nil || err.Error() != `runtime image platform "linux/amd64" is duplicated` {
		t.Fatalf("err = %v, want duplicate platform rejection", err)
	}
}

func TestParseRuntimeImageLockRejectsIncompleteReference(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
		"tag": "redis-8.8.0-openssl-3.5.8-r0-alpine3.23"
	}`))
	if err == nil || err.Error() != "runtime image digest is required" {
		t.Fatalf("err = %v, want runtime image digest is required", err)
	}
}

func TestParseRuntimeImageLockRejectsNonSHA256Digest(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "docker.io/codeflydev/redis",
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
	if *expected.DockerImage != *image {
		t.Fatalf("image = %+v, want %+v", image, expected.DockerImage)
	}
	if strings.Join(expected.Platforms, ",") != strings.Join(runtimeImage.Platforms, ",") {
		t.Fatalf("platforms = %v, want %v", runtimeImage.Platforms, expected.Platforms)
	}
}

// The published image is built for exactly the platforms CI builds. A platform
// added to the build without being added here would ship with no image SBOM
// subject, and so with no evidence, while coverage still reported complete.
func TestRuntimeImagePlatformsMatchTheBuiltPlatforms(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci workflow: %v", err)
	}
	_, rest, found := strings.Cut(string(workflow), "--platform ")
	if !found {
		t.Fatal("ci workflow builds no explicit --platform list")
	}
	built, _, _ := strings.Cut(rest, " ")
	if want := strings.Join(runtimeImage.Platforms, ","); built != want {
		t.Fatalf("ci builds %q, runtime-image.json declares %q", built, want)
	}
}

func TestManagedImageIsPatchedDockerHubReference(t *testing.T) {
	if image.Name != "docker.io/codeflydev/redis" {
		t.Fatalf("image.Name = %q, want docker.io/codeflydev/redis", image.Name)
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
		"setpriv=2.41.6-r1",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}
