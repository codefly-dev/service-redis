package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// The wire contract the host admits. These are literals on purpose. Reading
// them from the linked core would compare that core against itself and hold for
// every version, including one the host has already moved past — which is the
// drift that gets an agent rejected at discovery. Changing them means the host
// protocol moved, so the published CLI has to be confirmed to have moved too.
const (
	hostProtocolVersion        = 1
	hostStartupProtocolVersion = 2
	hostRecoveryCapability     = "container-recovery-scope/v1"
)

// readHandshakeEndpoint consumes the "<startup protocol>|<endpoint>" line the
// agent prints once it is listening, then drains the rest of its stdout so a
// chatty agent can never fill the pipe and block.
func readHandshakeEndpoint(t *testing.T, stdout io.Reader, stderr *syncBuffer) string {
	t.Helper()

	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		buffered := bufio.NewReader(stdout)
		line, err := buffered.ReadString('\n')
		lines <- result{line: line, err: err}
		_, _ = io.Copy(io.Discard, buffered)
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
		if spoken, err := strconv.Atoi(version); err != nil || spoken != hostStartupProtocolVersion {
			t.Fatalf("handshake startup protocol %q, want %d", version, hostStartupProtocolVersion)
		}
		return endpoint
	case <-time.After(30 * time.Second):
		t.Fatalf("agent did not emit a handshake\nagent stderr:\n%s", stderr.String())
		return ""
	}
}

// TestAgentDiscoveryDeclaresHostContract drives the agent the way the host does
// — a real process, the stdout handshake, then authenticated discovery — and
// checks what comes back against the contract the host admits. The declaration
// the host gates on is produced by the serving chain, so an in-process call to
// GetAgentInformation cannot show whether a published build would be admitted:
// an agent linking a core without the contract advertises nothing and is
// rejected before any runtime work.
func TestAgentDiscoveryDeclaresHostContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches the agent binary")
	}

	binary := filepath.Join(t.TempDir(), "service-redis")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, output)
	}

	// An *os.File stdout is handed to the child directly, so Wait neither waits
	// on nor closes the read end while the goroutine above is still using it.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	command := exec.Command(binary)
	command.Env = append(os.Environ(), "CODEFLY_AGENT_TOKEN=discovery-test", "CODEFLY_AGENT_UDS_PATH=")
	command.Stdout = writer
	var stderr syncBuffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	// The child holds its own descriptor; closing this copy is what ends the
	// read above once the agent exits.
	_ = writer.Close()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = reader.Close()
	})

	endpoint := readHandshakeEndpoint(t, reader, &stderr)
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

	declared := info.GetContract()
	if err = contract.Check(declared, contract.ContainerRecoveryScope); err != nil {
		t.Fatalf("host rejects this agent at discovery: %v", err)
	}
	if got := declared.GetProtocolVersion(); got != hostProtocolVersion {
		t.Errorf("declares CLI-agent protocol version %d, want %d; confirm the published CLI moved with this agent", got, hostProtocolVersion)
	}
	if got := declared.GetStartupProtocolVersion(); got != hostStartupProtocolVersion {
		t.Errorf("declares startup protocol version %d, want %d; confirm the published CLI moved with this agent", got, hostStartupProtocolVersion)
	}
	if !slices.Contains(declared.GetCapabilities(), hostRecoveryCapability) {
		t.Errorf("declares capabilities %v, want %q among them", declared.GetCapabilities(), hostRecoveryCapability)
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
