package main

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

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
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"

	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
	rediscache "github.com/codefly-dev/service-redis/cache"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

type Settings struct {
	Password    string `yaml:"password"`
	RequirePass bool   `yaml:"require-pass"`
	// KeepRunning opts into warm reuse: Stop leaves the server up instead of
	// releasing it. Stop otherwise ends execution, which for the native runtime
	// means terminating the process and freeing the assigned port.
	KeepRunning bool `yaml:"keep-running"`
}

// The managed image is the official redis Alpine image rebuilt with patched
// OpenSSL (libcrypto3/libssl3 >= 3.5.8-r0). The nix runtime provisions redis
// from nix/flake.nix, keeping both runtimes at parity.
var runtimeImage = shared.Must(parseRuntimeImageLock(runtimeImageLockJSON))

var image = runtimeImage.DockerImage

type runtimeImageLock struct {
	Name      string   `json:"name"`
	Tag       string   `json:"tag"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
}

// managedRuntimeImage is the pinned image together with the platforms it ships.
// The digest addresses a manifest index, so the platform list is what says how
// many images actually ship behind it, and image SBOM evidence owes one subject
// to each of them.
type managedRuntimeImage struct {
	*resources.DockerImage
	Platforms []string
}

func parseRuntimeImageLock(content []byte) (*managedRuntimeImage, error) {
	var lock runtimeImageLock
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, fmt.Errorf("parse runtime image lock: %w", err)
	}
	if lock.Name == "" {
		return nil, fmt.Errorf("runtime image name is required")
	}
	if lock.Tag == "" {
		return nil, fmt.Errorf("runtime image tag is required")
	}
	if lock.Digest == "" {
		return nil, fmt.Errorf("runtime image digest is required")
	}
	algorithm, encoded, found := strings.Cut(lock.Digest, ":")
	decoded, err := hex.DecodeString(encoded)
	if !found || algorithm != "sha256" || err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("runtime image digest must be a sha256 digest")
	}
	if len(lock.Platforms) == 0 {
		return nil, fmt.Errorf("runtime image platforms are required")
	}
	seen := map[string]bool{}
	for _, platform := range lock.Platforms {
		osName, architecture, ok := strings.Cut(platform, "/")
		if !ok || osName == "" || architecture == "" {
			return nil, fmt.Errorf("runtime image platform %q must be in os/arch form", platform)
		}
		if seen[platform] {
			return nil, fmt.Errorf("runtime image platform %q is duplicated", platform)
		}
		seen[platform] = true
	}
	return &managedRuntimeImage{
		DockerImage: &resources.DockerImage{
			Name:   lock.Name,
			Tag:    lock.Tag,
			Digest: lock.Digest,
		},
		Platforms: lock.Platforms,
	}, nil
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
			{
				Name: cacheiface.Group, Description: "codefly.dev/cache provider: open with cache.Open and the driver github.com/codefly-dev/service-redis/cache",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: cacheiface.KeyDriver, Description: "driver name, always " + rediscache.DriverName},
					{Name: cacheiface.KeyConnection, Description: "connection string"},
				},
			},
		},
	}.Build(), nil
}

// resolveServingTCPEndpoint selects the single TCP endpoint the redis agent
// binds its runtime and deployment to. A read-replica topology declares several
// tcp endpoints (e.g. read + write). The local runtime serves them on one
// instance and returns the selected endpoint's actual addresses for each alias.
// Core's FindTCPEndpoint rejects that ambiguity, so prefer the write/primary endpoint
// when present and otherwise fall back to the first declared TCP endpoint.
func resolveServingTCPEndpoint(ctx context.Context, endpoints []*basev0.Endpoint) (*basev0.Endpoint, error) {
	tcp := resources.FindEndpointsByAPI(ctx, standards.TCP, endpoints)
	switch len(tcp) {
	case 0:
		return nil, fmt.Errorf("no tcp endpoint found")
	case 1:
		return tcp[0], nil
	}
	for _, endpoint := range tcp {
		if endpoint.Name == "write" {
			return endpoint, nil
		}
	}
	return tcp[0], nil
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
			cacheConfiguration(connection),
		},
	}
	return outputConf, nil
}

func (s *Service) restrictedConnectionConfiguration(instance *basev0.NetworkInstance) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{
				Name: "redis",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Secret: true},
				},
			},
			cacheConfiguration(""),
		},
	}
}

// cacheConfiguration is what makes service-redis a provider of
// codefly.dev/cache: the group the interface definition fixes, naming the
// driver this repository ships (./cache) and the same connection as the redis
// group. An empty connection is the value-free secret reference restricted
// deployments return.
func cacheConfiguration(connection string) *basev0.ConfigurationInformation {
	return &basev0.ConfigurationInformation{
		Name: cacheiface.Group,
		ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: cacheiface.KeyDriver, Value: rediscache.DriverName},
			{Key: cacheiface.KeyConnection, Value: connection, Secret: true},
		},
	}
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

//go:embed runtime-image.json
var runtimeImageLockJSON []byte

//go:embed templates/agent
var readmeFS embed.FS
