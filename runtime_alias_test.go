package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"google.golang.org/protobuf/proto"
)

func proposedRedisMapping(name string, port uint16) *basev0.NetworkMapping {
	native := resources.NewNetworkInstance("localhost", port)
	native.Access = resources.NewNativeNetworkAccess()
	container := resources.NewNetworkInstance("host.docker.internal", port)
	container.Access = resources.NewContainerNetworkAccess()
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: name, Module: "module", Service: "redis", Api: "tcp"},
		Instances: []*basev0.NetworkInstance{native, container},
	}
}

func TestRedisRuntimeMappingsPreserveViewsAndProposal(t *testing.T) {
	read, write := proposedRedisMapping("read", 16001), proposedRedisMapping("write", 16002)
	// The primary's public view must not widen a private alias's visibility.
	public := resources.NewNetworkInstance("redis.example.com", 16002)
	public.Access = resources.NewPublicNetworkAccess()
	write.Instances = append(write.Instances, public)
	before := proto.Clone(read)
	accepted, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read, write}, write.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(read, before) {
		t.Fatal("caller proposal was mutated")
	}
	if !proto.Equal(accepted[0].Endpoint, read.Endpoint) {
		t.Fatal("alias identity changed")
	}
	if len(accepted[0].Instances) != 2 {
		t.Fatal("alias acquired an undeclared access view")
	}
	for i, instance := range accepted[0].Instances {
		if !proto.Equal(instance, write.Instances[i]) {
			t.Fatal("alias does not use the matching primary access view")
		}
	}
	accepted[0].Instances[0].Port = 16003
	if write.Instances[0].Port != 16002 || accepted[1].Instances[0].Port != 16002 {
		t.Fatal("accepted mappings share mutable instances")
	}
}

func TestRedisRuntimeMappingsRejectMissingAccessView(t *testing.T) {
	read, write := proposedRedisMapping("read", 16001), proposedRedisMapping("write", 16002)
	write.Instances = write.Instances[:1]
	if _, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read, write}, write.Endpoint); err == nil {
		t.Fatal("container alias accepted without a serving container address")
	}
	if _, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read}, write.Endpoint); err == nil {
		t.Fatal("missing primary mapping accepted")
	}
}

func TestRedisRuntimeMappingsSingleEndpointAndOtherAPI(t *testing.T) {
	primary := proposedRedisMapping("tcp", 16001)
	other := proposedRedisMapping("metrics", 16002)
	other.Endpoint.Api = "http"
	accepted, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{primary, other}, primary.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(primary, accepted[0]) || !proto.Equal(other, accepted[1]) {
		t.Fatal("single endpoint or non-TCP mapping changed")
	}
}

func TestRedisRuntimeMappingsRejectMalformedAliases(t *testing.T) {
	for _, scenario := range []string{"nil-serving", "nil-mapping", "nil-endpoint", "nil-instance", "missing-access", "duplicate-primary-access", "no-instances", "foreign-service"} {
		t.Run(scenario, func(t *testing.T) {
			read, write := proposedRedisMapping("read", 16001), proposedRedisMapping("write", 16002)
			serving := write.Endpoint
			switch scenario {
			case "nil-serving":
				serving = nil
			case "nil-mapping":
				read = nil
			case "nil-endpoint":
				read.Endpoint = nil
			case "nil-instance":
				read.Instances[0] = nil
			case "missing-access":
				read.Instances[0].Access = nil
			case "duplicate-primary-access":
				write.Instances = append(write.Instances, proto.Clone(write.Instances[0]).(*basev0.NetworkInstance))
			case "no-instances":
				read.Instances = nil
			case "foreign-service":
				read.Endpoint.Service = "other"
			}
			if _, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read, write}, serving); err == nil {
				t.Fatal("malformed proposal accepted")
			}
		})
	}
}

// A consumer must be able to use every returned endpoint, not just the
// write endpoint the agent's own readiness check happens to probe.
func TestRealRedisRuntimeReadWriteEndpoints(t *testing.T) {
	requireDocker(t)
	// The source-test runner is a grandchild of the CLI's agent. Its inherited
	// recovery marker belongs to that launcher, not this fixture. Establish
	// ownership for our isolated resources using Core's normal scope API.
	recoveryRoot := t.TempDir()
	recoveryScope, err := dockerrun.NewContainerRecoveryScope(recoveryRoot, recoveryRoot, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(recoveryscope.EnvironmentVariable, os.Getenv(recoveryscope.EnvironmentVariable))
	if err := dockerrun.SetContainerRecoveryScope(recoveryScope); err != nil {
		t.Fatal(err)
	}
	ports := make([]uint16, 2)
	listeners := make([]net.Listener, 0, 2)
	for i := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports[i] = uint16(listener.Addr().(*net.TCPAddr).Port)
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	read, write := proposedRedisMapping("read", ports[0]), proposedRedisMapping("write", ports[1])
	rt := NewRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := rt.HeadlessLoad(ctx, &basev0.ServiceIdentity{
		Workspace: fmt.Sprintf("redis-alias-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Module:    "module", Name: "redis", Version: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	rt.Runtime.SetEnvironment(&basev0.Environment{Name: "local"})
	rt.TcpEndpoint = write.Endpoint
	rt.Password = "local-alias-test-password"
	rt.RequirePass = true
	var ownedID string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		response, err := rt.Destroy(cleanup, &runtimev0.DestroyRequest{})
		if err != nil || response.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
			t.Errorf("destroy owned redis: %v, %v", response, err)
		}
		// Repeated explicit cleanup must remain safe after the acquired ID is gone.
		repeated, repeatErr := rt.Destroy(cleanup, &runtimev0.DestroyRequest{})
		if repeatErr != nil || repeated.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
			t.Errorf("repeat Destroy: %v, %v", repeated, repeatErr)
		}
		if ownedID != "" {
			remaining, inspectErr := exec.CommandContext(cleanup, "docker", "ps", "-aq", "--filter", "id="+ownedID).Output()
			if inspectErr != nil {
				t.Errorf("inspect owned fixture after Destroy: %v", inspectErr)
			} else if strings.TrimSpace(string(remaining)) != "" {
				t.Errorf("Destroy reported success but retained owned container %s", ownedID)
				// Bound cleanup of a failing regression to this fixture's exact ID.
				if err := exec.CommandContext(cleanup, "docker", "rm", "--force", ownedID).Run(); err != nil {
					t.Errorf("remove failed fixture %s: %v", ownedID, err)
				}
			}
		}
	})
	response, err := rt.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextContainer(),
		ProposedNetworkMappings: []*basev0.NetworkMapping{read, write},
	})
	if err != nil || response.GetStatus().GetState() != runtimev0.InitStatus_READY {
		t.Fatalf("Init: %v, %v", response, err)
	}
	ownedID, err = rt.runnerEnvironment.(*dockerrun.DockerEnvironment).ContainerID()
	if err != nil || ownedID == "" {
		t.Fatalf("acquired container identity: %q, %v", ownedID, err)
	}
	started, err := rt.Start(ctx, &runtimev0.StartRequest{})
	if err != nil || started.GetStatus().GetState() != runtimev0.StartStatus_STARTED {
		t.Fatalf("Start: %v, %v", started, err)
	}
	if len(response.GetNetworkMappings()) != 2 {
		t.Fatal("Init did not return both endpoint mappings")
	}
	// Write through the write alias, then read the same value through both.
	for i, endpoint := range []*basev0.Endpoint{write.Endpoint, read.Endpoint} {
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, response.GetNetworkMappings(), endpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			t.Fatal(err)
		}
		if err := probeRedis(ctx, instance.GetAddress(), rt.Password); err != nil {
			t.Errorf("consumer endpoint %s cannot authenticate and PING: %v", endpoint.Name, err)
		}
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", instance.GetAddress())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		reader := bufio.NewReader(conn)
		if err = redisExchange(ctx, conn, reader, "authenticate", instance.GetAddress(), "OK", "AUTH", rt.Password); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err = redisExchange(ctx, conn, reader, "write", instance.GetAddress(), "OK", "SET", "alias-test", "shared-value"); err != nil {
				t.Fatal(err)
			}
		}
		if err = writeRedisCommand(ctx, conn, "GET", "alias-test"); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"$12", "shared-value"} {
			got, err := readRedisLine(reader)
			if err != nil || got != want {
				t.Fatalf("%s GET: got %q, want %q: %v", endpoint.Name, got, want, err)
			}
		}
	}
	if read.Instances[0].GetPort() != uint32(ports[0]) {
		t.Fatal("Init changed the caller's proposal")
	}
	stopped, err := rt.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil || stopped.GetStatus().GetState() != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("Stop: %v, %v", stopped, err)
	}
	retained, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", ownedID).Output()
	if err != nil || strings.TrimSpace(string(retained)) != ownedID {
		t.Fatalf("ordinary Stop did not preserve its container: %q, %v", retained, err)
	}
}
