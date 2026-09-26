package main

// runtimeredis_test.go — redis as this agent's own Runtime runs it.
//
// startRuntimeRedis drives the Runtime contract end to end (HeadlessLoad → Init
// with proposed read/write mappings → Start, and Destroy at cleanup) over the
// docker or the nix backend, with or without read replicas. What a consumer
// gets is whatever Init emitted; nothing here rebuilds it.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	goredis "github.com/redis/go-redis/v9"
)

type redisBackend string

const (
	dockerBackend redisBackend = "docker"
	nixBackend    redisBackend = "nix"
)

var redisBackends = []redisBackend{dockerBackend, nixBackend}

// runtimeRedis is a redis started through the Runtime contract.
type runtimeRedis struct {
	rt          *Runtime
	init        *runtimev0.InitResponse
	read, write *basev0.Endpoint
	password    string
}

func requireBackend(t *testing.T, backend redisBackend) {
	t.Helper()
	switch backend {
	case dockerBackend:
		requireDocker(t)
		// The source-test runner may be a grandchild of the CLI's agent, whose
		// inherited recovery marker belongs to that launcher. Own this
		// fixture's containers through Core's normal scope API.
		recoveryRoot := t.TempDir()
		scope, err := dockerrun.NewContainerRecoveryScope(recoveryRoot, recoveryRoot, t.Name())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(recoveryscope.EnvironmentVariable, os.Getenv(recoveryscope.EnvironmentVariable))
		if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
			t.Fatal(err)
		}
	case nixBackend:
		if !runners.CheckNixInstalled() {
			requireInfrastructure(t, "nix is not installed")
		}
	}
}

// startRuntimeRedis runs redis through the Runtime on backend with a read and a
// write endpoint, and with replicas read replicas when replicas > 0.
func startRuntimeRedis(t *testing.T, backend redisBackend, replicas int, password string) *runtimeRedis {
	t.Helper()
	requireBackend(t, backend)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ports, err := reserveLoopbackPorts(2)
	if err != nil {
		t.Fatal(err)
	}
	read, write := proposedRedisMapping("read", ports[0]), proposedRedisMapping("write", ports[1])
	unique := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	rt := NewRuntime()
	if err = rt.HeadlessLoad(ctx, &basev0.ServiceIdentity{
		Workspace: "redis-runtime-" + unique, Module: "module", Name: "redis", Version: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	// Its own naming scope gives the native runtime its own state roots.
	rt.Runtime.SetEnvironment(&basev0.Environment{Name: "local", NamingScope: "test-" + unique})
	rt.WithReadReplicas, rt.ReadReplicas = replicas > 0, replicas
	if err = rt.resolveTCPEndpoints(ctx, []*basev0.Endpoint{read.Endpoint, write.Endpoint}); err != nil {
		t.Fatal(err)
	}
	rt.Password, rt.RequirePass = password, password != ""

	runtimeContext := resources.NewRuntimeContextContainer()
	if backend == nixBackend {
		runtimeContext = resources.NewRuntimeContextNix()
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if response, err := rt.Destroy(cleanup, &runtimev0.DestroyRequest{}); err != nil || response.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
			t.Errorf("Destroy: %v, %v", response, err)
		}
		if backend == nixBackend {
			stateKey := redisStateKey(rt.Location, rt.Environment.GetNamingScope())
			keys := []string{stateKey}
			for i := range replicas {
				keys = append(keys, redisReplicaStateKey(stateKey, i+1))
			}
			for _, key := range keys {
				if root, err := redisRuntimeRoot(key); err == nil {
					_ = os.RemoveAll(root)
				}
			}
		}
	})

	init, err := rt.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: []*basev0.NetworkMapping{read, write},
	})
	if err != nil || init.GetStatus().GetState() != runtimev0.InitStatus_READY {
		t.Fatalf("Init over %s: %v, %v", backend, init.GetStatus(), err)
	}
	started, err := rt.Start(ctx, &runtimev0.StartRequest{})
	if err != nil || started.GetStatus().GetState() != runtimev0.StartStatus_STARTED {
		t.Fatalf("Start over %s: %v, %v", backend, started.GetStatus(), err)
	}
	return &runtimeRedis{rt: rt, init: init, read: read.Endpoint, write: write.Endpoint, password: password}
}

// address is where a consumer on this host reaches endpoint, as Init mapped it.
func (r *runtimeRedis) address(t *testing.T, endpoint *basev0.Endpoint) string {
	t.Helper()
	instance, err := resources.FindNetworkInstanceInNetworkMappings(context.Background(), r.init.GetNetworkMappings(), endpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		t.Fatal(err)
	}
	return instance.GetAddress()
}

// consumer is the configuration Init emitted for a consumer on this host.
func (r *runtimeRedis) consumer(t *testing.T) emitted {
	t.Helper()
	for _, conf := range r.init.GetRuntimeConfigurations() {
		if conf.GetRuntimeContext().GetKind() == resources.RuntimeContextNative {
			return emitted{conf}
		}
	}
	t.Fatalf("Init emitted no configuration for a native consumer: %v", r.init.GetRuntimeConfigurations())
	return emitted{}
}

// client opens a go-redis client on a connection string Init emitted.
func redisClient(t *testing.T, connection string) *goredis.Client {
	t.Helper()
	opts, err := goredis.ParseURL(connection)
	if err != nil {
		t.Fatal(err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// replicationInfoOf reads INFO replication as field:value pairs.
func replicationInfoOf(t *testing.T, client *goredis.Client) map[string]string {
	t.Helper()
	raw, err := client.Info(context.Background(), "replication").Result()
	if err != nil {
		t.Fatalf("INFO replication on %s: %v", client.Options().Addr, err)
	}
	info := map[string]string{}
	for _, line := range strings.Split(raw, "\r\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			info[key] = value
		}
	}
	return info
}

// connectionAddress is the host:port a connection string points at.
func connectionAddress(t *testing.T, connection string) string {
	t.Helper()
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func requireRefused(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after Destroy", address)
	}
}
