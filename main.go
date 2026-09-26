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
	// WithReadReplicas runs read replicas beside the primary: the serving
	// (write) endpoint resolves to the primary, every other TCP endpoint to the
	// replicas. Without it every alias is served by one process.
	WithReadReplicas bool `yaml:"with-read-replicas"`
	// ReadReplicas is how many replicas run. It defaults to 1 and is only
	// meaningful with with-read-replicas.
	ReadReplicas int `yaml:"read-replicas"`
}

// replicaCount is the number of read replicas the settings ask for, 0 when
// there are none. A count without with-read-replicas is refused rather than
// guessed at: either reading of it silently changes what the service runs.
func (s *Settings) replicaCount() (int, error) {
	switch {
	case s.ReadReplicas < 0:
		return 0, fmt.Errorf("read-replicas is %d; it must be at least 1", s.ReadReplicas)
	case !s.WithReadReplicas && s.ReadReplicas > 0:
		return 0, fmt.Errorf("read-replicas is %d but with-read-replicas is not set", s.ReadReplicas)
	case !s.WithReadReplicas:
		return 0, nil
	case s.ReadReplicas == 0:
		return 1, nil
	}
	return s.ReadReplicas, nil
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
	// ReadEndpoint is the TCP endpoint the replicas serve when the service has
	// read replicas, and nil otherwise. Every TCP alias other than TcpEndpoint
	// resolves to the same replicas.
	ReadEndpoint *basev0.Endpoint
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
					{Name: readConnectionKey, Description: "connection string of the read replicas; only with with-read-replicas"},
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

// resolveServingTCPEndpoint selects the TCP endpoint the primary serves. A
// read/write topology declares several tcp endpoints (e.g. read + write), which
// Core's FindTCPEndpoint rejects as ambiguous, so prefer the write endpoint when
// present and otherwise fall back to the first declared TCP endpoint. Without
// read replicas every other alias is served by the same process.
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

// resolveReplicaTCPEndpoint selects the TCP endpoint read replicas serve: "read"
// when declared, otherwise the first TCP endpoint that is not the serving one.
// Replicas with no endpoint of their own would be unreachable, so that is an
// error rather than a topology that quietly serves nothing.
func resolveReplicaTCPEndpoint(ctx context.Context, endpoints []*basev0.Endpoint, serving *basev0.Endpoint) (*basev0.Endpoint, error) {
	var candidates []*basev0.Endpoint
	for _, endpoint := range resources.FindEndpointsByAPI(ctx, standards.TCP, endpoints) {
		if endpoint.Name == serving.GetName() {
			continue
		}
		if endpoint.Name == "read" {
			return endpoint, nil
		}
		candidates = append(candidates, endpoint)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("with-read-replicas needs a TCP endpoint for the replicas besides %q (declare a read endpoint)", serving.GetName())
	}
	return candidates[0], nil
}

// resolveTCPEndpoints binds the serving endpoint and, when the settings ask for
// replicas, the endpoint they serve. Load runs it for the runtime and the
// builder alike, so both render the same topology.
func (s *Service) resolveTCPEndpoints(ctx context.Context, endpoints []*basev0.Endpoint) error {
	serving, err := resolveServingTCPEndpoint(ctx, endpoints)
	if err != nil {
		return err
	}
	s.TcpEndpoint = serving
	s.ReadEndpoint = nil
	replicas, err := s.replicaCount()
	if err != nil || replicas == 0 {
		return err
	}
	s.ReadEndpoint, err = resolveReplicaTCPEndpoint(ctx, endpoints, serving)
	return err
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

// readConnectionKey carries the replicas' connection in the redis group. The
// cache group has no such key: every cache operation goes to the primary.
const readConnectionKey = "read-connection"

// addReadConnection records the replicas' connection string for readInstance in
// the redis group of conf, which CreateConnectionConfiguration built for the
// primary's matching access view.
func (s *Service) addReadConnection(conf *basev0.Configuration, readInstance *basev0.NetworkInstance) {
	value := &basev0.ConfigurationValue{Key: readConnectionKey, Secret: true}
	if readInstance != nil {
		value.Value = s.createConnectionString(context.Background(), readInstance.Address)
	}
	for _, info := range conf.GetInfos() {
		if info.GetName() == "redis" {
			info.ConfigurationValues = append(info.ConfigurationValues, value)
			return
		}
	}
}

// restrictedConnectionConfiguration carries value-free secret references only.
// withReplicas adds the replicas' reference beside the primary's.
func (s *Service) restrictedConnectionConfiguration(instance *basev0.NetworkInstance, withReplicas bool) *basev0.Configuration {
	conf := &basev0.Configuration{
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
	if withReplicas {
		s.addReadConnection(conf, nil)
	}
	return conf
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
