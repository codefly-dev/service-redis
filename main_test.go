package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/agents"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"os"
	"path"
	"testing"
	"time"
)

func TestCreateToRunWithoutReplicas(t *testing.T) {
	testCreateToRun(t, false)
}

func TestCreateToRunWithReplicas(t *testing.T) {
	testCreateToRun(t, true)
}

func testCreateToRun(t *testing.T, withReplica bool) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir := t.TempDir()
	defer func(path string) {
		_ = os.RemoveAll(path)
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	err := service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name))
	service.WithModule("mod")

	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}
	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime()

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)

	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()
	shared.Must(env.Proto())

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})

	require.NoError(t, err)

	runtime.Settings.WithReadReplicas = withReplica

	require.Equal(t, 2, len(runtime.Endpoints))

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 2, len(networkMappings))

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextFree(),
		ProposedNetworkMappings: networkMappings,
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		//_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	// Extract logs

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	// Get the configuration and connect to redis
	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	// extract the connection string
	connWriteString, err := resources.GetConfigurationValue(ctx, configurationOut, "write", "connection")
	require.NoError(t, err)

	connReadString, err := resources.GetConfigurationValue(ctx, configurationOut, "read", "connection")
	require.NoError(t, err)

	if withReplica {
		require.NotEqual(t, connReadString, connWriteString)
	}

	// Connect to the redis
	readClient, err := CreateRedisClient(connReadString)
	require.NoError(t, err)

	out := readClient.Ping(ctx)
	require.NoError(t, out.Err())

	writeClient, err := CreateRedisClient(connWriteString)
	require.NoError(t, err)
	out = writeClient.Ping(ctx)
	require.NoError(t, out.Err())

	// Write something
	err = writeClient.Set(ctx, "key", "value", 0).Err()
	require.NoError(t, err)

	val, err := writeClient.Get(ctx, "key").Result()
	require.NoError(t, err)

	// Read the value from read
	err = shared.Retry(5*time.Second, 10, func() error {
		val, err = readClient.Get(ctx, "key").Result()
		return err
	})
	require.NoError(t, err)
	// Check the value
	require.Equal(t, "value", val)

}
