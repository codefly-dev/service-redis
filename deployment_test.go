package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, &deploymentTemplateParameters{})

	secret, err := os.ReadFile(filepath.Join(destination, "overlays", "test", "secret.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret), "kind: Secret") || !strings.Contains(string(secret), "c2VjcmV0") {
		t.Fatalf("ephemeral profile did not render populated Secret:\n%s", secret)
	}
}

func TestPromotableGitOpsDeploymentConfiguresAuthenticationAndReturnsConnectionReference(t *testing.T) {
	useSuccessfulKubectl(t)
	builder, networkMappings := newDeploymentTestBuilder(t)
	builder.RequirePass = true
	passwordEnvironmentKey := resources.ServiceSecretConfigurationKeyFromUnique(
		builder.Unique(),
		"redis",
		"REDIS_PASSWORD",
	)
	destination := t.TempDir()

	response, err := builder.Deploy(context.Background(), promotableDeploymentRequest(
		destination,
		networkMappings,
		map[string]*builderv0.KubernetesSecretKeyReference{
			passwordEnvironmentKey: {
				Name: "redis-credentials",
				Key:  "password",
			},
			"UNRELATED_SECRET": {
				Name: "unrelated-credentials",
				Key:  "token",
			},
		},
		true,
	))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}

	output := response.GetDeployment().GetKubernetes()
	if output.GetProfile() != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		t.Fatalf("output profile = %s", output.GetProfile())
	}
	if output.GetContractVersion() != services.KubernetesManifestContractVersion {
		t.Fatalf("contract version = %q", output.GetContractVersion())
	}
	if output.GetValidation().GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("static validation failed: %v", output.GetValidation().GetViolations())
	}
	if output.GetValidation().GetServerSideValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("server-side validation failed: %v", output.GetValidation().GetViolations())
	}

	configuration := response.GetConfiguration()
	if configuration.GetOrigin() != builder.Unique() {
		t.Fatalf("configuration origin = %q, want %q", configuration.GetOrigin(), builder.Unique())
	}
	if configuration.GetRuntimeContext() == nil {
		t.Fatal("connection configuration has no runtime context")
	}
	infos := configuration.GetInfos()
	if len(infos) != 1 || infos[0].GetName() != "redis" {
		t.Fatalf("connection configuration infos = %v", infos)
	}
	values := infos[0].GetConfigurationValues()
	if len(values) != 1 || values[0].GetKey() != "connection" {
		t.Fatalf("connection configuration values = %v", values)
	}
	if !values[0].GetSecret() || values[0].GetValue() != "" {
		t.Fatalf("connection descriptor = %+v, want a value-free secret reference", values[0])
	}

	statefulSet := readDeploymentFile(t, destination, "base", "stateful-set.yaml")
	for _, expected := range []string{
		"automountServiceAccountToken: false",
		"image: redis@" + image.Digest,
		`test -n "$REDIS_PASSWORD"`,
		`exec redis-server --requirepass "$REDIS_PASSWORD"`,
		"name: REDIS_PASSWORD",
		"name: REDISCLI_AUTH",
		"name: redis-credentials",
		"key: password",
	} {
		if !strings.Contains(statefulSet, expected) {
			t.Errorf("GitOps StatefulSet missing %q:\n%s", expected, statefulSet)
		}
	}
	for _, unexpected := range []string{
		"envFrom:",
		passwordEnvironmentKey,
		"UNRELATED_SECRET",
		"unrelated-credentials",
	} {
		if strings.Contains(statefulSet, unexpected) {
			t.Errorf("GitOps StatefulSet contains %q:\n%s", unexpected, statefulSet)
		}
	}

	tree := readManifestTree(t, destination)
	for _, unexpected := range []string{
		"kind: Namespace",
		"kind: Secret",
		"\ndata:",
		"\nstringData:",
	} {
		if strings.Contains(tree, unexpected) {
			t.Errorf("GitOps manifest tree contains %q:\n%s", unexpected, tree)
		}
	}
}

func TestPromotableGitOpsRequirePassRejectsUnusablePasswordReference(t *testing.T) {
	tests := []struct {
		name       string
		references func(*Builder) map[string]*builderv0.KubernetesSecretKeyReference
		message    string
	}{
		{
			name:    "missing",
			message: "requires a typed Kubernetes Secret reference",
		},
		{
			name: "optional",
			references: func(builder *Builder) map[string]*builderv0.KubernetesSecretKeyReference {
				key := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "redis", "REDIS_PASSWORD")
				return map[string]*builderv0.KubernetesSecretKeyReference{
					key: {
						Name:     "redis-credentials",
						Key:      "password",
						Optional: true,
					},
				}
			},
			message: "must not be optional",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder, networkMappings := newDeploymentTestBuilder(t)
			builder.RequirePass = true
			var references map[string]*builderv0.KubernetesSecretKeyReference
			if test.references != nil {
				references = test.references(builder)
			}

			response, err := builder.Deploy(context.Background(), promotableDeploymentRequest(
				t.TempDir(),
				networkMappings,
				references,
				false,
			))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetState().GetState() != builderv0.DeploymentStatus_ERROR {
				t.Fatalf("deployment status = %s, want ERROR", response.GetState().GetState())
			}
			if !strings.Contains(response.GetState().GetMessage(), test.message) {
				t.Fatalf("deployment error = %q, want %q", response.GetState().GetMessage(), test.message)
			}
			if response.GetConfiguration() != nil {
				t.Fatalf("failed deployment returned configuration: %+v", response.GetConfiguration())
			}
		})
	}
}

func newDeploymentTestBuilder(t *testing.T) (*Builder, []*basev0.NetworkMapping) {
	t.Helper()
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "redis",
		Version:   "1.2.3",
	}
	if err := builder.HeadlessLoad(ctx, identity); err != nil {
		t.Fatal(err)
	}
	builder.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("redis.example.com", 6379)
	instance.Access = resources.NewPublicNetworkAccess()
	return builder, []*basev0.NetworkMapping{{
		Endpoint:  builder.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{instance},
	}}
}

func promotableDeploymentRequest(
	destination string,
	networkMappings []*basev0.NetworkMapping,
	secretReferences map[string]*builderv0.KubernetesSecretKeyReference,
	validateServerSide bool,
) *builderv0.DeploymentRequest {
	return &builderv0.DeploymentRequest{
		Environment:     &basev0.Environment{Name: "test"},
		NetworkMappings: networkMappings,
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:          "codefly-test",
				Destination:        destination,
				Profile:            builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
				SecretReferences:   secretReferences,
				ValidateServerSide: validateServerSide,
			},
		}},
	}
}

func useSuccessfulKubectl(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(kubectl, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func readDeploymentFile(t *testing.T, directory string, elements ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{directory}, elements...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func readManifestTree(t *testing.T, root string) string {
	t.Helper()
	var manifests strings.Builder
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests.Write(content)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return manifests.String()
}
