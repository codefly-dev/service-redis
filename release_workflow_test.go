package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseWorkflowProvidesWritableGitHubToken(t *testing.T) {
	content, err := os.ReadFile(".github/workflows/releaser.yml")
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Secrets any `yaml:"secrets"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(content, &workflow); err != nil {
		t.Fatal(err)
	}

	if got := workflow.Permissions["contents"]; got != "write" {
		t.Errorf("release workflow contents permission = %q, want %q", got, "write")
	}
	secrets, _ := workflow.Jobs["release"].Secrets.(map[string]any)
	got, _ := secrets["GH_PAT"].(string)
	if got != "${{ secrets.GITHUB_TOKEN }}" {
		t.Errorf("release workflow GH_PAT binding = %q, want the per-run GitHub token", got)
	}
}
