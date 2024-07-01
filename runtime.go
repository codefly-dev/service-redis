package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/helpers/code"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	"github.com/go-redis/redis/v8"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/hashicorp/go-multierror"
	_ "github.com/lib/pq"
	"time"
)

type Runtime struct {
	*Service

	// internal
	runners   []runners.RunnerEnvironment
	redisPort uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.SetEnvironment(req.Environment)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	requirements.Localize(s.Location)

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading endpoints")
	}

	s.write, err = resources.FindTCPEndpointWithName(ctx, "write", s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding write endpoint")
	}

	s.read, err = resources.FindTCPEndpointWithName(ctx, "read", s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding read endpoint")

	}

	return s.Runtime.LoadResponse()
}

func (s *Runtime) CreateConnectionConfigurationInformation(ctx context.Context, endpoint *basev0.Endpoint, instance *basev0.NetworkInstance) *basev0.ConfigurationInformation {

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	connection := fmt.Sprintf("redis://%s:%d", instance.Hostname, instance.Port)

	return &basev0.ConfigurationInformation{
		Name: endpoint.Name,
		ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: "connection", Value: connection, Secret: true},
		},
	}
}

func (s *Runtime) CreateConnectionsConfiguration(runtimeContext *basev0.RuntimeContext, infos []*basev0.ConfigurationInformation) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Base.Service.Unique(),
		RuntimeContext: runtimeContext,
		Configurations: infos,
	}
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	writeMapping, err := resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.write)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	writeInstanceContainer, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.ProposedNetworkMappings, s.write, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Infof("write endpoint will run on localhost:%d", writeInstanceContainer.Port)

	s.redisPort = 6379

	// runner for the write endpoint
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithPortMapping(ctx, uint16(writeInstanceContainer.Port), s.redisPort)
	runner.WithOutput(s.Logger)

	err = runner.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runners = []runners.RunnerEnvironment{runner}

	readMapping, err := resources.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.read)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	readInstanceContainer, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.ProposedNetworkMappings, s.read, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if !s.Settings.WithReadReplicas {
		// Point to the write
		readMapping = &basev0.NetworkMapping{
			Endpoint:  s.read,
			Instances: writeMapping.Instances,
		}
		s.Infof("read endpoint will run on localhost:%d", writeInstanceContainer.Port)

	} else {
		// Use the instances
		s.Wool.Debug("replicaRunner", wool.Field("port", writeInstanceContainer.Port), wool.Field("host", writeInstanceContainer.Hostname))
		name := fmt.Sprintf("%s-read", s.UniqueWithWorkspace())
		replicaRunner, err := runners.NewDockerHeadlessEnvironment(ctx, image, name)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		replicaRunner.WithCommand("redis-server", "--replicaof", writeInstanceContainer.Hostname, fmt.Sprintf("%d", writeInstanceContainer.Port))
		replicaRunner.WithPortMapping(ctx, uint16(readInstanceContainer.Port), s.redisPort)
		runner.WithOutput(s.Logger)

		// Create a replicaRunner identity
		identity := s.Identity.Clone()
		identity.Name = fmt.Sprintf("%s-read", s.Identity.Name)
		out := agents.NewServiceProvider(ctx, identity).Get(ctx)
		replicaRunner.WithOutput(out)
		s.Infof("read endpoint will run on localhost:%d", readInstanceContainer.Port)

		err = replicaRunner.Init(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}

		s.runners = append(s.runners, replicaRunner)
	}

	s.NetworkMappings = []*basev0.NetworkMapping{writeMapping, readMapping}

	informations := make(map[string][]*basev0.ConfigurationInformation)

	for _, inst := range writeMapping.Instances {
		informations[resources.RuntimeContextFromInstance(inst).Kind] = append(informations[resources.RuntimeContextFromInstance(inst).Kind], s.CreateConnectionConfigurationInformation(ctx, s.write, inst))
	}
	for _, inst := range readMapping.Instances {
		informations[resources.RuntimeContextFromInstance(inst).Kind] = append(informations[resources.RuntimeContextFromInstance(inst).Kind], s.CreateConnectionConfigurationInformation(ctx, s.read, inst))
	}
	for _, runtimeContext := range []*basev0.RuntimeContext{resources.NewRuntimeContextNative(), resources.NewRuntimeContextContainer()} {
		conf := s.CreateConnectionsConfiguration(runtimeContext, informations[runtimeContext.Kind])
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	return s.Runtime.InitResponse()
}

func CreateRedisClient(conn string) (*redis.Client, error) {
	opt, err := redis.ParseURL(conn)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(opt), nil
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Get the configuration and connect to postgres
	configuration, err := resources.ExtractConfiguration(s.Runtime.RuntimeConfigurations, resources.NewRuntimeContextNative())
	if err != nil {
		return err
	}

	// extract the connection string
	connWriteString, err := resources.FindConfigurationValue(configuration, "write", "connection")
	if err != nil {
		return err
	}
	write, err := CreateRedisClient(connWriteString)
	if err != nil {
		return err
	}

	connReadString, err := resources.FindConfigurationValue(configuration, "read", "connection")
	if err != nil {
		return err
	}
	read, err := CreateRedisClient(connReadString)
	if err != nil {
		return err
	}
	retries := 5
	for i := 0; i < retries; i++ {
		err = write.Ping(ctx).Err()
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	for i := 0; i < retries; i++ {
		err = read.Ping(ctx).Err()
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	var agg error
	for _, runner := range s.runners {
		err := runner.Stop(ctx)
		if err != nil {
			agg = multierror.Append(agg, err)
		}
	}
	if agg != nil {
		return s.Runtime.DestroyError(agg)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.Runtime.DesiredInit()
	return nil
}
