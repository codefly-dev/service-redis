package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A cell's execution admission refuses any pod whose container declares an
// exec probe or an exec lifecycle hook: both run a command of the delivering
// party's choosing inside a pod that already holds the workload identity. The
// primary and every replica are probed on the redis port instead, and nothing
// the deployment renders execs.
func TestRenderedWorkloadsDeclareNoExecProbeOrHook(t *testing.T) {
	_, _, destination := replicaDeployment(t, 2, true)
	workloads := 0
	err := filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(content)))
		for {
			var document map[string]any
			if decoder.Decode(&document) != nil {
				break
			}
			containers := podContainers(document)
			if containers == nil {
				continue
			}
			workloads++
			for _, container := range containers {
				for _, probe := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
					spec, _ := container[probe].(map[string]any)
					if _, execs := spec["exec"]; execs {
						t.Errorf("%s: container %v %s execs", path, container["name"], probe)
					}
					if spec != nil && spec["tcpSocket"] == nil {
						t.Errorf("%s: container %v %s does not probe the port", path, container["name"], probe)
					}
				}
				lifecycle, _ := container["lifecycle"].(map[string]any)
				for hook, spec := range lifecycle {
					if handler, ok := spec.(map[string]any); ok && handler["exec"] != nil {
						t.Errorf("%s: container %v lifecycle %s execs", path, container["name"], hook)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if workloads != 2 {
		t.Fatalf("checked %d workloads, want the primary and the replicas", workloads)
	}
}

// podContainers returns the containers and init containers of a workload's pod
// template, or nil for a document that is not a workload.
func podContainers(document map[string]any) []map[string]any {
	switch kind, _ := document["kind"].(string); kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
	default:
		return nil
	}
	spec, _ := document["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	var containers []map[string]any
	for _, key := range []string{"containers", "initContainers"} {
		list, _ := podSpec[key].([]any)
		for _, item := range list {
			if container, ok := item.(map[string]any); ok {
				containers = append(containers, container)
			}
		}
	}
	return containers
}
