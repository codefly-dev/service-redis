package main

import (
	"context"
	"embed"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

type Settings struct {
	Password    string `yaml:"password"`
	RequirePass bool   `yaml:"require-pass"`
}

var image = &resources.DockerImage{Name: "redis", Tag: "8-alpine"}

type Service struct {
	*services.Base

	// Settings
	*Settings

	redisPassword string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		// Advertise the nix runtime (implemented in nixredis.go via
		// RuntimeContextNix) so the CLI's per-service Docker-free gate
		// (flow.resolveDockerFallback → Runner.SupportsNix) lets this service
		// fall back to a nix-provisioned native redis when Docker is
		// unreachable. Without it the run hard-stops with "requires Docker"
		// even though the nix path works.
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_NIX},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{
			{
				Name: "redis", Description: "redis connection details",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "connection string"},
				},
			},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	// Configuration is optional — if no REDIS_PASSWORD is provided, run
	// without auth. This is the sensible default for local dev + test
	// environments; production deployments set the password via the
	// standard configuration flow.
	if conf == nil {
		s.redisPassword = ""
		return nil
	}
	pw, err := resources.GetConfigurationValue(ctx, conf, "redis", "REDIS_PASSWORD")
	if err != nil {
		// Missing key is fine — empty password means no auth. Only
		// surface genuine errors (malformed config, etc.) — but
		// GetConfigurationValue returns an error for "not found" too, so
		// treat any err as "no password configured".
		s.redisPassword = ""
		return nil
	}
	s.redisPassword = pw
	return nil
}

func (s *Service) createConnectionString(_ context.Context, address string) string {
	if s.redisPassword != "" {
		return fmt.Sprintf("redis://:%s@%s", s.redisPassword, address)
	}
	return fmt.Sprintf("redis://%s", address)
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.LoadConfiguration(ctx, conf)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot load configuration")
	}

	connection := s.createConnectionString(ctx, instance.Address)

	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "redis",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
}

func main() {
	svc := NewService()
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
