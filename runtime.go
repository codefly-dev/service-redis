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
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/wool"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	runners []*runners.Docker
	port    int32
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.Localize(s.Location)

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("initializing", wool.Field("networkMappings", configurations.MakeNetworkMappingSummary(req.ProposedNetworkMappings)))

	// Get the write mapping
	writeMapping, err := configurations.FindNetworkMapping(s.write, req.ProposedNetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	for _, providerInfo := range req.ProviderInfos {
		s.EnvironmentVariables.Add(configurations.ProviderInformationAsEnvironmentVariables(providerInfo)...)
	}

	// Docker
	s.port = 6379

	// runner for the write endpoint
	runner, err := runners.NewDocker(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithCommand("redis-server")
	runner.WithPort(runners.DockerPortMapping{Container: s.port, Host: writeMapping.Port})
	runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	runner.WithName(s.Global())

	if s.Settings.Persist {
		runner.WithPersistence()
	}

	if s.Settings.Silent {
		runner.WithSilence()
	}

	err = runner.Init(ctx, image)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runners = []*runners.Docker{runner}

	readMapping, err := configurations.FindNetworkMapping(s.read, req.ProposedNetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if !s.Settings.ReadReplica {
		// Point to the write
		readMapping = &basev0.NetworkMapping{
			Endpoint: s.read,
			Host:     writeMapping.Host,
			Port:     writeMapping.Port,
			Address:  writeMapping.Address,
		}
	} else {
		s.NetworkMappings = req.ProposedNetworkMappings
		// Use the instances
		write := configurations.LocalizeMapping(writeMapping, "host.docker.internal")
		// runner for the write endpoint
		replicaRunner, err := runners.NewDocker(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}

		s.Wool.Focus("replicaRunner", wool.Field("port", write.Port), wool.Field("host", write.Host))
		replicaRunner.WithCommand("redis-server", "--replicaof", write.Host, fmt.Sprintf("%d", write.Port))
		replicaRunner.WithPort(runners.DockerPortMapping{Container: s.port, Host: readMapping.Port})
		replicaRunner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
		replicaRunner.WithName(fmt.Sprintf("%s-read", s.Global()))
		if s.Settings.Persist {
			replicaRunner.WithPersistence()
		}

		// Create a replicaRunner identity
		identity := s.Identity.Clone()
		identity.Name = fmt.Sprintf("%s-read", s.Identity.Name)
		out := agents.NewServiceProvider(ctx, identity).Get(ctx)
		replicaRunner.WithOut(out)
		if s.Settings.Silent {
			replicaRunner.WithSilence()
		}
		s.LogForward("read endpoint will run on: %s", readMapping.Address)

		err = replicaRunner.Init(ctx, image)
		if err != nil {
			return s.Runtime.InitError(err)
		}

		s.runners = append(s.runners, replicaRunner)
	}

	s.NetworkMappings = []*basev0.NetworkMapping{writeMapping, readMapping}

	s.LogForward("write endpoint, will run on: %s", writeMapping.Address)
	s.LogForward("read endpoint, will run on: %s", readMapping.Address)

	connections := &basev0.ProviderInformation{
		Name: "redis", Origin: s.Service.Configuration.Unique(),
		Data: map[string]string{
			"read":  s.CreateConnectionString(ctx, readMapping.Address),
			"write": s.CreateConnectionString(ctx, writeMapping.Address),
		},
	}

	s.ServiceProviderInfos = []*basev0.ProviderInformation{connections}

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

	runningContext := s.Wool.Inject(context.Background())

	for _, runner := range s.runners {
		err := runner.Start(runningContext)
		if err != nil {
			return s.Runtime.StartError(err)
		}
		err = s.WaitForReady(ctx)
		if err != nil {
			return s.Runtime.StartError(err)
		}

	}
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	s.Wool.Debug("stopping service")
	for _, runner := range s.runners {
		err := runner.Stop()
		if err != nil {
			return s.Runtime.StopError(err)
		}

	}
	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.Runtime.DesiredInit()
	return nil
}
