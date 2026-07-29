package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)

	secret, err := os.ReadFile(filepath.Join(destination, "overlays", "test", "secret.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret), "kind: Secret") || !strings.Contains(string(secret), "c2VjcmV0") {
		t.Fatalf("ephemeral profile did not render populated Secret:\n%s", secret)
	}
}

func TestPromotableGitOpsPreparationDoesNotExportSecretConfiguration(t *testing.T) {
	builder := NewBuilder()
	err := builder.prepareDeployment(context.Background(), &services.KustomizeDeploymentContext{
		Request: &builderv0.DeploymentRequest{},
		Profile: builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPromotableGitOpsTemplates(t *testing.T) {
	ctx := context.Background()
	identity := &resources.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "redis",
		Version:   "1.2.3",
	}
	base := &services.Base{
		Wool:        wool.Get(ctx),
		Identity:    identity,
		Information: &services.Information{Service: resources.ToServiceWithCase(identity), Module: resources.ToModuleWithCase(identity)},
	}
	base.SetDockerImage(image)
	builder := &services.BuilderWrapper{Base: base}
	base.Builder = builder

	destination := t.TempDir()
	secretReferences := map[string]*builderv0.KubernetesSecretKeyReference{
		"REDIS_PASSWORD": {Name: "redis-credentials", Key: "password"},
	}
	deployment := &builderv0.KubernetesDeployment{
		Namespace:        "codefly-test",
		Destination:      destination,
		Profile:          builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		SecretReferences: secretReferences,
	}
	err := builder.KustomizeDeploy(
		ctx,
		&basev0.Environment{Name: "test"},
		deployment,
		deploymentFS,
		services.DeploymentParameters{SecretReferences: secretReferences},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(destination, "base", "namespace.yaml"),
		filepath.Join(destination, "overlays", "test", "secret.yaml"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(content)) != "" {
			t.Fatalf("%s must be empty for GitOps:\n%s", path, content)
		}
	}

	statefulSet, err := os.ReadFile(filepath.Join(destination, "base", "stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(statefulSet)
	for _, expected := range []string{
		"automountServiceAccountToken: false",
		"image: redis@" + image.Digest,
		"name: redis-credentials",
		"key: password",
	} {
		if !strings.Contains(manifest, expected) {
			t.Errorf("GitOps StatefulSet missing %q:\n%s", expected, manifest)
		}
	}
	if strings.Contains(manifest, "envFrom:") {
		t.Fatalf("GitOps StatefulSet uses an untyped Secret reference:\n%s", manifest)
	}

	validation := services.ValidateKubernetesManifestTree(
		ctx,
		destination,
		"test",
		"codefly-test",
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		false,
	)
	if validation.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("GitOps static conformance failed: %v", validation.GetViolations())
	}
}
