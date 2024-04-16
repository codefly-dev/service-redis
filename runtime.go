package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/hashicorp/go-multierror"
	_ "github.com/lib/pq"
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

	s.Runtime.SetScope(req)
	if !s.Runtime.Container() {
		return s.Base.Runtime.LoadError(fmt.Errorf("not implemented: cannot load service in scope %s", req.Scope))
	}

	s.Runtime.Scope = req.Scope

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.Localize(s.Location)

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.write, err = configurations.FindTCPEndpointWithName(ctx, "write", s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.read, err = configurations.FindTCPEndpointWithName(ctx, "read", s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)

	}

	s.EnvironmentVariables.SetEnvironment(req.Environment)
	s.EnvironmentVariables.SetNetworkScope(req.Scope)

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) CreateConnectionConfiguration(ctx context.Context, endpoint *basev0.Endpoint, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	connection := fmt.Sprintf("redis://%s:%d", instance.Hostname, instance.Port)

	conf := &basev0.Configuration{
		Origin: s.Base.Service.Unique(),
		Scope:  instance.Scope,
		Configurations: []*basev0.ConfigurationInformation{
			{Name: endpoint.Name,
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return conf, nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	writeMapping, err := configurations.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.write)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	writeInstance, err := s.Runtime.NetworkInstance(ctx, req.ProposedNetworkMappings, s.write)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.LogForward("write endpoint will run on localhost:%d", writeInstance.Port)

	s.redisPort = 6379

	// runner for the write endpoint
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithProject())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithPortMapping(ctx, uint16(writeInstance.Port), s.redisPort)
	runner.WithOutput(s.Logger)

	err = runner.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runners = []runners.RunnerEnvironment{runner}

	readMapping, err := configurations.FindNetworkMapping(ctx, req.ProposedNetworkMappings, s.read)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	readInstance, err := s.Runtime.NetworkInstance(ctx, req.ProposedNetworkMappings, s.read)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if !s.Settings.ReadReplica {
		// Point to the write
		readMapping = &basev0.NetworkMapping{
			Endpoint:  s.read,
			Instances: writeMapping.Instances,
		}
		s.LogForward("read endpoint will run on localhost:%d", writeInstance.Port)

	} else {
		// Use the instances
		s.Wool.Debug("replicaRunner", wool.Field("port", writeInstance.Port), wool.Field("host", writeInstance.Hostname))
		name := fmt.Sprintf("%s-read", s.UniqueWithProject())
		replicaRunner, err := runners.NewDockerHeadlessEnvironment(ctx, image, name)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		replicaRunner.WithCommand("redis-server", "--replicaof", writeInstance.Hostname, fmt.Sprintf("%d", writeInstance.Port))
		replicaRunner.WithPortMapping(ctx, uint16(readInstance.Port), s.redisPort)
		runner.WithOutput(s.Logger)

		// Create a replicaRunner identity
		identity := s.Identity.Clone()
		identity.Name = fmt.Sprintf("%s-read", s.Identity.Name)
		out := agents.NewServiceProvider(ctx, identity).Get(ctx)
		replicaRunner.WithOutput(out)
		s.LogForward("read endpoint will run on localhost:%d", readInstance.Port)

		err = replicaRunner.Init(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}

		s.runners = append(s.runners, replicaRunner)
	}

	s.NetworkMappings = []*basev0.NetworkMapping{writeMapping, readMapping}

	// Create connection string configurations for the network instance
	for _, inst := range writeMapping.Instances {
		conf, err := s.CreateConnectionConfiguration(ctx, s.write, inst)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.Runtime.ExportedConfigurations = append(s.Runtime.ExportedConfigurations, conf)
	}
	for _, inst := range readMapping.Instances {
		conf, err := s.CreateConnectionConfiguration(ctx, s.read, inst)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.Runtime.ExportedConfigurations = append(s.Runtime.ExportedConfigurations, conf)
	}

	return s.Base.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	if s.Settings.Persist {
		s.Wool.Debug("persisting service")
		return s.Runtime.StopResponse()
	}
	s.Wool.Debug("stopping service")
	var agg error
	for _, runner := range s.runners {
		err := runner.Stop(ctx)
		if err != nil {
			agg = multierror.Append(agg, err)
		}
	}
	err := s.Base.Stop()
	if err != nil {
		agg = multierror.Append(agg, err)
	}
	if agg != nil {
		return s.Base.Runtime.StopError(agg)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	//TODO implement me
	panic("implement me")
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.Runtime.DesiredInit()
	return nil
}
