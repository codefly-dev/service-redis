package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// newReadinessRuntime builds a runtime whose only reachable TCP instance is the
// native (localhost) one at nativePort, mirroring how the agent sees a published
// container port.
func newReadinessRuntime(t *testing.T, nativePort uint16) *Runtime {
	t.Helper()

	// A guaranteed-free port for the unreachable container instance: bind then
	// release it, so nothing listens there. Derived independently of nativePort
	// to avoid a uint16 wrap (nativePort+1 overflows to 0 when nativePort=65535).
	deadListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (dead port): %v", err)
	}
	deadPort := uint16(deadListener.Addr().(*net.TCPAddr).Port)
	deadListener.Close()

	rt := NewRuntime()
	if err = rt.HeadlessLoad(context.Background(), &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "redis",
		Version:   "1.2.3",
	}); err != nil {
		t.Fatalf("HeadlessLoad: %v", err)
	}
	rt.Runtime.WithContext(resources.NewRuntimeContextContainer())

	rt.TcpEndpoint = &basev0.Endpoint{Name: "tcp", Module: "module", Service: "redis", Api: "tcp"}

	// The container instance points at a port nothing listens on, so a probe
	// that selected it would fail — only the native instance is reachable.
	native := resources.NewNetworkInstance("localhost", nativePort)
	native.Access = resources.NewNativeNetworkAccess()
	container := resources.NewNetworkInstance("host.docker.internal", deadPort)
	container.Access = resources.NewContainerNetworkAccess()
	rt.NetworkMappings = []*basev0.NetworkMapping{{
		Endpoint:  rt.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{container, native},
	}}
	return rt
}

// TestWaitForReadyProbesNativeMappingUnderContainerContext pins the readiness
// probe to the native (localhost) instance regardless of the runtime context.
// The agent runs on the host and reaches the published container port via
// localhost; the container mapping (host.docker.internal) resolves inside other
// containers, not this host process. A regression to the runtime-context access
// would dial the unreachable container instance and report "redis is not ready".
func TestWaitForReadyProbesNativeMappingUnderContainerContext(t *testing.T) {
	server := newScriptedRedis(t, "+PONG\r\n")
	port := uint16(server.listener.Addr().(*net.TCPAddr).Port)

	rt := newReadinessRuntime(t, port)

	if err := rt.WaitForReady(context.Background()); err != nil {
		t.Fatalf("WaitForReady probed the wrong instance: %v", err)
	}
}

// TestWaitForReadyRejectsRepliesThatAreNotAnAuthenticatedPong pins readiness to
// a parsed PONG. Accepting any nonempty reply instead reports ready for a
// passworded redis refusing the probe, a redis still loading its dataset, and an
// unrelated HTTP service.
func TestWaitForReadyRejectsRepliesThatAreNotAnAuthenticatedPong(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{name: "noauth", reply: "-NOAUTH Authentication required.\r\n"},
		{name: "loading", reply: "-LOADING Redis is loading the dataset in memory\r\n"},
		{name: "http", reply: "HTTP/1.1 503 Service Unavailable\r\n\r\n"},
		{name: "empty", reply: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newScriptedRedis(t, tc.reply)
			rt := newReadinessRuntime(t, uint16(server.listener.Addr().(*net.TCPAddr).Port))

			// A budget shorter than the runtime's own bounds the retryable rows;
			// the non-retryable ones fail before it matters.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			if err := rt.WaitForReady(ctx); err == nil {
				t.Fatalf("WaitForReady accepted %q as ready", tc.reply)
			}
		})
	}
}

// TestWaitForReadyAuthenticatesWithTheConfiguredPassword covers the projected
// credentials actually being used: the same server is ready with the configured
// password and refuses without it.
func TestWaitForReadyAuthenticatesWithTheConfiguredPassword(t *testing.T) {
	const password = "projected-secret"
	server := newAuthRedis(t, password)
	port := uint16(server.listener.Addr().(*net.TCPAddr).Port)

	rt := newReadinessRuntime(t, port)
	rt.redisPassword = password
	if err := rt.WaitForReady(context.Background()); err != nil {
		t.Fatalf("WaitForReady with the configured password: %v", err)
	}

	unauthenticated := newReadinessRuntime(t, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := unauthenticated.WaitForReady(ctx)
	if err == nil {
		t.Fatal("WaitForReady reported ready without the configured password")
	}
	if !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("error is not an authentication diagnostic: %v", err)
	}
}
