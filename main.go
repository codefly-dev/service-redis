package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("migrations", "migrations").WithPathSelect(shared.NewSelect("*.sql")),
)

type Settings struct {
	Debug bool `yaml:"debug"` // Developer only

	Watch       bool `yaml:"watch"`
	Silent      bool `yaml:"silent"`
	ReadReplica bool `yaml:"read-replica"`
	Persist     bool `yaml:"persist"`
}

var image = &configurations.DockerImage{Name: "redis", Tag: "latest"}

type Service struct {
	*services.Base

	// Settings
	*Settings

	write *basev0.Endpoint
	read  *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_DOCKER},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ProviderInfos: []*agentv0.ProviderInfoDetail{
			{
				Name: "redis", Description: "connection string",
				Fields: []*agentv0.ProviderInfoField{
					{Name: "write", Description: "connection string for write endpoint"},
					{Name: "read", Description: "connection string for read endpoint"},
				},
			},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(configurations.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadEndpoints(ctx context.Context) error {
	// Create the write endpoint
	write := s.Configuration.BaseEndpoint("write")
	var err error
	s.write, err = configurations.NewTCPAPI(ctx, write)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot  create write endpoint")
	}
	s.Endpoints = []*basev0.Endpoint{s.write}

	// Create the read endpoint
	read := s.Configuration.BaseEndpoint("read")
	s.read, err = configurations.NewTCPAPI(ctx, read)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot  create read endpoint")
	}
	s.Endpoints = append(s.Endpoints, s.read)
	return nil
}

func (s *Service) CreateConnectionString(ctx context.Context, address string) string {
	password, _ := s.EnvironmentVariables.GetServiceProvider(ctx, s.Unique(), "redis", "REDIS_PASSWORD")
	if password == "" {
		return fmt.Sprintf("redis://%s", address)
	} else {
		return fmt.Sprintf("redis://:%s@%s", password, address)
	}
}

func main() {
	agents.Register(
		services.NewServiceAgent(agent.Of(configurations.ServiceAgent), NewService()),
		services.NewBuilderAgent(agent.Of(configurations.RuntimeServiceAgent), NewBuilder()),
		services.NewRuntimeAgent(agent.Of(configurations.BuilderServiceAgent), NewRuntime()))
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
