package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
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
	runners              []*runners.Docker
	EnvironmentVariables *configurations.EnvironmentVariableManager

	NetworkMappings []*basev0.NetworkMapping
	Port            int
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
	writeMapping := configurations.FindMapping(req.ProposedNetworkMappings, s.write)

	net, err := configurations.GetMappingInstanceForName(ctx, req.ProposedNetworkMappings, standards.TCP, "write")
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.LogForward("will run on: %s", net.Address)

	for _, providerInfo := range req.ProviderInfos {
		s.EnvironmentVariables.Add(configurations.ProviderInformationAsEnvironmentVariables(providerInfo)...)
	}

	// Docker
	s.Port = 6379

	// runner for the write endpoint
	runner, err := runners.NewDocker(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithCommand("redis-server")
	runner.WithPort(runners.DockerPortMapping{Container: s.Port, Host: net.Port})
	runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	if s.Settings.Silent {
		runner.WithSilence()
	}

	err = runner.Init(ctx, image)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runners = []*runners.Docker{runner}

	if s.Settings.Replicas == 0 {
		s.NetworkMappings = []*basev0.NetworkMapping{writeMapping}
		// Create a network mapping for read to go to write
		replica := &basev0.NetworkMapping{
			Application: s.read.Application,
			Service:     s.read.Service,
			Endpoint:    s.read,
			Addresses:   []string{net.Address},
		}
		s.NetworkMappings = append(s.NetworkMappings, replica)
	} else {
		s.NetworkMappings = req.ProposedNetworkMappings
		// Use the instances
		mps, err := configurations.GetMappingInstancesForName(req.ProposedNetworkMappings, standards.TCP, "read")
		if err != nil {
			return s.Runtime.InitError(err)
		}
		localized := configurations.LocalizeMapping(writeMapping, "host.docker.internal")
		address := localized.Addresses[0]
		host, port, err := configurations.SplitAddress(address)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		for _, mp := range mps {
			// runner for the write endpoint
			replica, err := runners.NewDocker(ctx)
			if err != nil {
				return s.Runtime.InitError(err)
			}

			replica.WithCommand("redis-server", "--replicaof", host, port)
			replica.WithPort(runners.DockerPortMapping{Container: s.Port, Host: mp.Port})
			replica.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)

			// Create a replica identity
			identity := s.Identity.Clone()
			identity.Name = fmt.Sprintf("%s-read", s.Identity.Name)
			out := agents.NewServiceProvider(ctx, identity).Get(ctx)
			replica.WithOut(out)
			if s.Settings.Silent {
				replica.WithSilence()
			}

			err = replica.Init(ctx, image)
			if err != nil {
				return s.Runtime.InitError(err)
			}
			s.runners = append(s.runners, replica)
		}
	}

	return s.Base.Runtime.InitResponse(s.NetworkMappings)
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	//if s.createDataFirst {
	//	// Hack for now
	//	time.Sleep(5 * time.Second)
	//}
	//maxRetry := 5
	//var err error
	//for retry := 0; retry < maxRetry; retry++ {
	//	time.Sleep(5 * time.Second)
	//	db, err := sql.Open("postgres", connection)
	//	if err != nil {
	//		return s.Wool.Wrapf(err, "cannot open database")
	//	}
	//
	//	err = db.Ping()
	//	if err == nil {
	//		return nil
	//	}
	//}
	//return s.Wool.Wrapf(err, "cannot ping database")
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
