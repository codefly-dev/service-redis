package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

// hostileEnvironmentValue carries every character that breaks a value pasted
// verbatim between two double quotes in YAML: a double quote, a backslash and
// a newline. A workspace configuration whose value is a JSON document is the
// production shape of the first one.
const hostileEnvironmentValue = "{\"documents\":{\"path\":\"C:\\\\data\"}}\nsecond line"

// TestEnvironmentValuesRenderAsEscapedYAMLScalars pins the overlay templates to
// emitting every ConfigMap and Secret value as a properly escaped YAML
// double-quoted scalar. Rendered as `"{{ $value }}"`, a value holding a double
// quote produced a manifest the deploy renderer refused with "did not find
// expected key"; each value must survive a YAML round trip unchanged, and a
// value with nothing to escape must still render exactly as it always did.
func TestEnvironmentValuesRenderAsEscapedYAMLScalars(t *testing.T) {
	values := services.EnvironmentMap{"HOSTILE": hostileEnvironmentValue, "PLAIN": "abc"}
	overlay := renderEnvironmentValues(t, values, &deploymentTemplateParameters{ServicePorts: []servicePort{{Port: 6379}}, Headless: true})
	for _, manifest := range []string{"secret.yaml"} {
		t.Run(manifest, func(t *testing.T) {
			rendered, err := os.ReadFile(filepath.Join(overlay, manifest))
			if err != nil {
				t.Fatalf("read %s: %v", manifest, err)
			}
			var parsed struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.Unmarshal(rendered, &parsed); err != nil {
				t.Fatalf("%s is not valid YAML: %v\n%s", manifest, err, rendered)
			}
			for key, want := range values {
				if got := parsed.Data[key]; got != want {
					t.Errorf("%s data[%s] = %q, want %q\n%s", manifest, key, got, want, rendered)
				}
			}
			if !strings.Contains(string(rendered), `PLAIN: "abc"`) {
				t.Errorf("%s must keep rendering a plain value as a double-quoted scalar:\n%s", manifest, rendered)
			}
		})
	}
}

// renderEnvironmentValues renders the deployment templates under the ephemeral
// profile — the one that materializes both the ConfigMap and the Secret — with
// the given values in both maps, the way agenttesting.AssertKustomizeTemplates
// does with its fixed, harmless value. It returns the rendered overlay
// directory.
func renderEnvironmentValues(t *testing.T, values services.EnvironmentMap, parameters any) string {
	t.Helper()
	ctx := context.Background()
	identity := &resources.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "example-service",
		Version:   "1.2.3",
	}
	base := &services.Base{
		Wool:        wool.Get(ctx),
		Identity:    identity,
		Information: &services.Information{Service: resources.ToServiceWithCase(identity), Module: resources.ToModuleWithCase(identity)},
	}
	base.SetDockerImage(resources.NewDockerImage("example/service:1.2.3"))
	builder := &services.BuilderWrapper{Base: base}
	base.Builder = builder

	destination := t.TempDir()
	deployment := &builderv0.KubernetesDeployment{
		Namespace:   "codefly-test",
		Destination: destination,
		Profile:     builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	}
	params := services.DeploymentParameters{ConfigMap: values, SecretMap: values, Parameters: parameters}
	if err := builder.KustomizeDeploy(ctx, &basev0.Environment{Name: "test"}, deployment, deploymentFS, params); err != nil {
		t.Fatalf("render kustomize templates: %v", err)
	}
	return filepath.Join(destination, "overlays", "test")
}
