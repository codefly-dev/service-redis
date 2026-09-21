package main

// image_sbom_test.go — image SBOM evidence for the pinned runtime image.
//
// The conformance tests below run everywhere: they check that what this agent
// claims about its own images survives sbom.ValidateCoverage, including that a
// fabricated "nothing to cover here" answer is rejected.
//
// The scanning tests need a working docker and network access to the registry.
// They skip when either is missing. Set REDIS_IMAGE_SBOM_TESTS=required (the CI
// image job does) to turn a skip into a failure so the job cannot pass without
// actually inventorying the shipped image.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services/sbom"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func requireImageSBOMInfrastructure(t *testing.T, missing string) {
	t.Helper()
	if os.Getenv("REDIS_IMAGE_SBOM_TESTS") == "required" {
		t.Fatalf("image SBOM tests are required but %s", missing)
	}
	t.Skipf("skipping: %s", missing)
}

// requireDockerForImageSBOM gates the scanning tests. They pull from the
// registry, so they are opt-in rather than merely docker-dependent: leaving
// REDIS_IMAGE_SBOM_TESTS unset keeps `go test ./...` off the network, where a
// machine with a running daemon but no connectivity would otherwise fail rather
// than skip.
func requireDockerForImageSBOM(t *testing.T) {
	t.Helper()
	if os.Getenv("REDIS_IMAGE_SBOM_TESTS") == "" {
		t.Skip("skipping: set REDIS_IMAGE_SBOM_TESTS=required to inventory the shipped image")
	}
	if reason, ok := dockerUsable(); !ok {
		requireImageSBOMInfrastructure(t, reason)
	}
}

func imageSBOMBuilder(t *testing.T) *Builder {
	t.Helper()
	builder, _ := newDeploymentTestBuilder(t)
	return builder
}

// expectedRuntimeSubjects derives coverage expectations from the lock file
// rather than from the enumeration under test, so the two can disagree.
func expectedRuntimeSubjects(t *testing.T, service string) []*builderv0.ImageSubject {
	t.Helper()
	content, err := os.ReadFile("runtime-image.json")
	if err != nil {
		t.Fatalf("read runtime-image.json: %v", err)
	}
	lock, err := parseRuntimeImageLock(content)
	if err != nil {
		t.Fatalf("parseRuntimeImageLock: %v", err)
	}
	var subjects []*builderv0.ImageSubject
	for _, platform := range lock.Platforms {
		subjects = append(subjects, &builderv0.ImageSubject{
			Reference: lock.FullName(),
			Platform:  platform,
			Role:      runtimeImageRole,
			Service:   service,
		})
	}
	return subjects
}

func TestRuntimeImageSubjectsCoverEveryShippedPlatform(t *testing.T) {
	builder := imageSBOMBuilder(t)
	subjects := builder.runtimeImageSubjects()

	if len(subjects) != len(runtimeImage.Platforms) {
		t.Fatalf("got %d subjects, want one per shipped platform (%d)", len(subjects), len(runtimeImage.Platforms))
	}
	for i, subject := range subjects {
		if subject.GetPlatform() != runtimeImage.Platforms[i] {
			t.Fatalf("subject %d platform = %q, want %q", i, subject.GetPlatform(), runtimeImage.Platforms[i])
		}
		if subject.GetReference() != image.FullName() {
			t.Fatalf("subject %d reference = %q, want the digest-pinned %q", i, subject.GetReference(), image.FullName())
		}
		if subject.GetRole() != runtimeImageRole {
			t.Fatalf("subject %d role = %q, want %q", i, subject.GetRole(), runtimeImageRole)
		}
		if subject.GetService() == "" {
			t.Fatalf("subject %d carries no service identity", i)
		}
		// The reference pins the manifest index; evidence binds to the child
		// digest of each platform. A subject carrying the index digest would
		// reject the agent's own correct scan as a digest mismatch.
		if subject.GetDigest() != "" {
			t.Fatalf("subject %d digest = %q, want it left to the resolved child manifest", i, subject.GetDigest())
		}
	}
}

// Enumerating subjects reads the service identity, which exists only after
// Load. Reading it unloaded panics, and the recovered panic returns an empty
// response with no error — absent evidence dressed as success. Callers reach
// Builder.SBOM through a pass-through that does not load (core
// services.BuilderInstance.SBOM), so the unloaded call is reachable.
func TestImageScopeWithoutLoadReportsAnError(t *testing.T) {
	builder := NewBuilder()

	response, err := builder.SBOM(context.Background(), &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
	})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if response == nil {
		t.Fatal("SBOM returned no response")
	}
	if state := response.GetState().GetState(); state != builderv0.SBOMStatus_ERROR {
		t.Fatalf("state = %s, want ERROR", state)
	}
	if response.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Fatalf("scope = %s, want image", response.GetScope())
	}
	if len(response.GetImages()) != 0 {
		t.Fatalf("unloaded service returned %d inventories", len(response.GetImages()))
	}
}

// A no-image claim is how a service that ships nothing stays honest. Redis does
// ship an image, so the same claim here is the false coverage the contract
// exists to prevent, and it must not survive validation.
func TestNoImageClaimFailsCoverageForAShippedImage(t *testing.T) {
	builder := imageSBOMBuilder(t)
	expected := expectedRuntimeSubjects(t, builder.Unique())

	for _, reason := range []builderv0.NoImageReason{
		builderv0.NoImageReason_NO_IMAGE_REASON_NO_IMAGE,
		builderv0.NoImageReason_NO_IMAGE_REASON_EXTERNALLY_MANAGED,
	} {
		response, err := builder.Builder.SBOMNoImage(reason, "redis ships no image")
		if err != nil {
			t.Fatalf("SBOMNoImage: %v", err)
		}
		if err := sbom.ValidateCoverage(builder.Unique(), expected, response); err == nil {
			t.Fatalf("%s passed coverage for a service that ships %d images", reason, len(expected))
		}
	}
}

// A source inventory describes lockfiles, not image contents. Accepting one as
// image coverage is the substitution the contract forbids.
func TestSourceInventoryFailsImageCoverage(t *testing.T) {
	builder := imageSBOMBuilder(t)
	expected := expectedRuntimeSubjects(t, builder.Unique())

	response, err := builder.Builder.SBOMResponse(&agentv0.Bom{
		Components: []*agentv0.Component{{Name: "redis", Version: "8.8.0"}},
	}, "syft", "DOCKER", "abc123")
	if err != nil {
		t.Fatalf("SBOMResponse: %v", err)
	}
	if err := sbom.ValidateCoverage(builder.Unique(), expected, response); err == nil {
		t.Fatal("a source inventory passed image coverage")
	}
}

func TestEnumeratedSubjectsSatisfyCoverage(t *testing.T) {
	builder := imageSBOMBuilder(t)
	expected := expectedRuntimeSubjects(t, builder.Unique())

	var evidence []*builderv0.ImageSBOM
	for i, subject := range builder.runtimeImageSubjects() {
		evidence = append(evidence, &builderv0.ImageSBOM{
			Digest:   childDigest(i),
			Platform: subject.GetPlatform(),
			Subjects: []*builderv0.ImageSubject{subject},
			Bom:      &agentv0.Bom{Components: []*agentv0.Component{{Name: "busybox", Version: "1.37.0-r30"}}},
			Tool:     "syft",
			Sha256:   "deterministic",
		})
	}
	response, err := builder.Builder.SBOMImageResponse(evidence)
	if err != nil {
		t.Fatalf("SBOMImageResponse: %v", err)
	}
	if err := sbom.ValidateCoverage(builder.Unique(), expected, response); err != nil {
		t.Fatalf("enumerated subjects failed coverage: %v", err)
	}

	// Dropping one platform's evidence must fail: a multi-architecture image is
	// not covered by an inventory of one of its architectures.
	partial, err := builder.Builder.SBOMImageResponse(evidence[:1])
	if err != nil {
		t.Fatalf("SBOMImageResponse: %v", err)
	}
	if err := sbom.ValidateCoverage(builder.Unique(), expected, partial); err == nil {
		t.Fatal("evidence for one platform passed coverage for a multi-platform image")
	}
}

func childDigest(i int) string {
	return fmt.Sprintf("sha256:%064x", i+1)
}

func TestImageSBOMCoversEveryShippedPlatform(t *testing.T) {
	requireDockerForImageSBOM(t)

	builder := imageSBOMBuilder(t)
	ctx := context.Background()
	response, err := builder.SBOM(ctx, &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := response.GetState().GetState(); state != builderv0.SBOMStatus_COMPLETE {
		t.Fatalf("state = %s: %s", state, response.GetState().GetMessage())
	}
	if response.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Fatalf("scope = %s, want image", response.GetScope())
	}
	if err := sbom.ValidateCoverage(builder.Unique(), expectedRuntimeSubjects(t, builder.Unique()), response); err != nil {
		t.Fatalf("scanned evidence failed coverage: %v", err)
	}

	if len(response.GetImages()) != len(runtimeImage.Platforms) {
		t.Fatalf("got %d inventories, want one per shipped platform (%d)", len(response.GetImages()), len(runtimeImage.Platforms))
	}
	seen := map[string]string{}
	for _, evidence := range response.GetImages() {
		// Evidence must name the platform's own child manifest. The index
		// digest would describe the manifest list, which contains no packages.
		if evidence.GetDigest() == image.Digest {
			t.Fatalf("%s is bound to the manifest index digest, not a platform child", evidence.GetPlatform())
		}
		if previous, ok := seen[evidence.GetDigest()]; ok {
			t.Fatalf("platforms %s and %s share digest %s", previous, evidence.GetPlatform(), evidence.GetDigest())
		}
		seen[evidence.GetDigest()] = evidence.GetPlatform()

		assertImageInventory(t, evidence)
	}
	for _, platform := range runtimeImage.Platforms {
		if !containsPlatform(seen, platform) {
			t.Fatalf("no evidence for shipped platform %s", platform)
		}
	}
}

// assertImageInventory checks the inventory describes the image rather than the
// repository: OS packages and the installed redis server, neither of which any
// source lockfile in this repository would produce.
func assertImageInventory(t *testing.T, evidence *builderv0.ImageSBOM) {
	t.Helper()
	var osPackages, redis int
	for _, component := range evidence.GetBom().GetComponents() {
		if strings.HasPrefix(component.GetPurl(), "pkg:apk/") {
			osPackages++
		}
		if component.GetName() == "redis" {
			redis++
		}
	}
	if osPackages == 0 {
		t.Fatalf("%s inventory lists no OS packages", evidence.GetPlatform())
	}
	if redis == 0 {
		t.Fatalf("%s inventory lists no redis application component", evidence.GetPlatform())
	}
	if evidence.GetSha256() == "" {
		t.Fatalf("%s inventory carries no document checksum", evidence.GetPlatform())
	}
}

func containsPlatform(seen map[string]string, platform string) bool {
	for _, got := range seen {
		if got == platform {
			return true
		}
	}
	return false
}

// A platform the image does not ship must fail loudly. Reporting it as covered,
// or degrading to the platforms that do exist, is the silent gap the contract
// forbids.
func TestImageSBOMReportsAnUnshippedPlatformAsFailure(t *testing.T) {
	requireDockerForImageSBOM(t)

	builder := imageSBOMBuilder(t)
	response, err := builder.SBOM(context.Background(), &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{{
			Reference: image.FullName(),
			Platform:  "linux/riscv64",
			Role:      runtimeImageRole,
			Service:   builder.Unique(),
		}},
	})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := response.GetState().GetState(); state != builderv0.SBOMStatus_ERROR {
		t.Fatalf("state = %s, want ERROR", state)
	}
	if response.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Fatalf("scope = %s, want image so the caller can tell which request failed", response.GetScope())
	}
	if len(response.GetImages()) != 0 {
		t.Fatalf("failed scan returned %d inventories", len(response.GetImages()))
	}
}
