package main

import (
	"context"
	"net"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// TestWaitForReadyProbesNativeMappingUnderContainerContext pins the readiness
// probe to the native (localhost) instance regardless of the runtime context.
// The agent runs on the host and reaches the published container port via
// localhost; the container mapping (host.docker.internal) resolves inside other
// containers, not this host process. A regression to the runtime-context access
// would dial the unreachable container instance and report "redis is not ready".
func TestWaitForReadyProbesNativeMappingUnderContainerContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = conn.Write([]byte("+PONG\r\n"))
			conn.Close()
		}
	}()

	nativePort := uint16(listener.Addr().(*net.TCPAddr).Port)

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
	container := resources.NewNetworkInstance("host.docker.internal", nativePort+1)
	container.Access = resources.NewContainerNetworkAccess()
	rt.NetworkMappings = []*basev0.NetworkMapping{{
		Endpoint:  rt.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{container, native},
	}}

	if err = rt.WaitForReady(context.Background()); err != nil {
		t.Fatalf("WaitForReady probed the wrong instance: %v", err)
	}
}
