package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// readHandshakeEndpoint consumes the "<startup protocol>|<endpoint>" line the
// agent prints once it is listening, asserting the startup protocol the host
// speaks.
func readHandshakeEndpoint(t *testing.T, stdout io.Reader, stderr *bytes.Buffer) string {
	t.Helper()

	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		lines <- result{line: line, err: err}
	}()

	select {
	case r := <-lines:
		if r.err != nil {
			t.Fatalf("read handshake: %v\nagent stderr:\n%s", r.err, stderr.String())
		}
		version, endpoint, found := strings.Cut(strings.TrimSpace(r.line), "|")
		if !found {
			t.Fatalf("malformed handshake %q\nagent stderr:\n%s", r.line, stderr.String())
		}
		if spoken, err := strconv.Atoi(version); err != nil || spoken != agents.ProtocolVersion {
			t.Fatalf("handshake startup protocol %q, want %d", version, agents.ProtocolVersion)
		}
		return endpoint
	case <-time.After(30 * time.Second):
		t.Fatalf("agent did not emit a handshake\nagent stderr:\n%s", stderr.String())
		return ""
	}
}

// TestAgentDiscoveryDeclaresHostContract drives the agent the way the host does
// — a real process, the stdout handshake, then authenticated discovery — and
// runs the host's own admission check against what comes back. The declaration
// the host gates on is produced by the serving chain, so an in-process call to
// GetAgentInformation cannot show whether a published build would be admitted:
// an agent linking a core without the contract advertises nothing and is
// rejected before any runtime work.
func TestAgentDiscoveryDeclaresHostContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "service-redis")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, output)
	}

	command := exec.Command(binary)
	command.Env = append(os.Environ(), "CODEFLY_AGENT_TOKEN=discovery-test", "CODEFLY_AGENT_UDS_PATH=")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	endpoint := readHandshakeEndpoint(t, stdout, &stderr)
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, agents.AuthMetadataKey, "discovery-test")

	info, err := agentv0.NewAgentClient(conn).GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		t.Fatalf("GetAgentInformation: %v\nagent stderr:\n%s", err, stderr.String())
	}

	if err = contract.Check(info.GetContract(), contract.ContainerRecoveryScope); err != nil {
		t.Fatalf("host rejects this agent at discovery: %v", err)
	}

	// The declaration travels alongside the advertisement, never instead of it.
	if info.GetReadMe() == "" {
		t.Error("advertised README is empty")
	}
	var advertisesConnection bool
	for _, detail := range info.GetConfigurationDetails() {
		if detail.GetName() != "redis" {
			continue
		}
		for _, field := range detail.GetFields() {
			if field.GetName() == "connection" {
				advertisesConnection = true
			}
		}
	}
	if !advertisesConnection {
		t.Errorf("redis connection configuration is not advertised: %v", info.GetConfigurationDetails())
	}
}
