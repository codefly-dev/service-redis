package main

import (
	"context"
	"embed"
	"fmt"
	"net/url"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
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

var image = &resources.DockerImage{
	Name:   "redis",
	Tag:    "8.8.0-alpine",
	Digest: "sha256:9d317178eceac8454a2284a9e6df2466b93c745529947f0cd42a0fa9609d7005",
}

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

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Nix:    true,
			Docker: true,
		},
		ReadMe: readme,
		Config: []*agentv0.ConfigurationValueDetail{
			{
				Name: "redis", Description: "redis connection details",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "connection string"},
				},
			},
		},
	}.Build(), nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	// Runtime configuration has highest precedence; service.codefly.yaml's
	// password is the local/default fallback. Previously Password and
	// RequirePass parsed successfully but were never read in production.
	pw := ""
	if conf != nil {
		configured, err := resources.GetConfigurationValue(ctx, conf, "redis", "REDIS_PASSWORD")
		if err != nil {
			return err
		}
		pw = configured
	}
	if pw == "" {
		pw = s.Password
	}
	if s.RequirePass && pw == "" {
		return fmt.Errorf("redis require-pass is enabled but no password is configured")
	}
	s.redisPassword = pw
	return nil
}

func (s *Service) createConnectionString(_ context.Context, address string) string {
	if s.redisPassword != "" {
		return (&url.URL{Scheme: "redis", Host: address, User: url.UserPassword("", s.redisPassword)}).String()
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
