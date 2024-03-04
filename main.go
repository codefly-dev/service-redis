package main

import (
	"context"
	"embed"
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

	Watch    bool `yaml:"watch"`
	Silent   bool `yaml:"silent"`
	Replicas int  `yaml:"replicas"`
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
		ReadMe:    readme,
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
	write := &configurations.Endpoint{Name: "write", Visibility: configurations.VisibilityApplication}
	write.Application = s.Configuration.Application
	write.Service = s.Configuration.Name
	var err error
	s.write, err = configurations.NewTCPAPI(ctx, write)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot  create write endpoint")
	}
	s.Endpoints = []*basev0.Endpoint{s.write}
	// Create the write endpoint
	read := &configurations.Endpoint{Name: "read", Visibility: configurations.VisibilityApplication}
	read.Application = s.Configuration.Application
	read.Service = s.Configuration.Name
	s.read, err = configurations.NewTCPAPI(ctx, read)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot  create read endpoint")
	}
	s.read.Replicas = int32(s.Replicas)
	s.Endpoints = append(s.Endpoints, s.read)
	return nil
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
