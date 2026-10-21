package main

import (
	"context"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// The CLI verifies every agent before dispatching a build to a selected Buildx
// builder (an environment that mirrors a registry builds on one of its own);
// an agent that cannot answer fails the whole render.
func TestBuildCapabilitiesHonourASelectedBuilder(t *testing.T) {
	capabilities, err := NewBuilder().BuildCapabilities(context.Background(), &builderv0.BuildCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.GetBuildxSelection() {
		t.Fatal("a selected Buildx builder must be honoured")
	}
}
