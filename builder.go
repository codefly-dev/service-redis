package main

import (
	"context"
	"embed"
	"fmt"

	"github.com/codefly-dev/core/agents/communicate"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/upgrade"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// runtimeImageRole is the role every image this service owns plays; redis ships
// a single runtime image with no init, migration, or sidecar companions.
const runtimeImageRole = "runtime"

type Builder struct {
	*services.DefaultBuilder
	*Service
}

type deploymentTemplateParameters struct {
	PasswordReference *builderv0.KubernetesSecretKeyReference
	ServicePorts      []servicePort
	Headless          bool
}

// servicePort is one port the rendered Service publishes. Name is empty when the
// Service has a single port, which Kubernetes allows to stay anonymous.
type servicePort struct {
	Name string
	Port uint32
}

func NewBuilder() *Builder {
	service := NewService()
	return &Builder{
		DefaultBuilder: services.NewDefaultBuilder(service.Builder),
		Service:        service,
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: func(ctx context.Context, endpoints []*v0.Endpoint) error {
			endpoint, err := resolveServingTCPEndpoint(ctx, endpoints)
			if err != nil {
				return err
			}
			s.TcpEndpoint = endpoint
			s.Wool.Debug("endpoint", wool.Field("tcp", endpoint))
			return nil
		},
	})
}

// Audit scans the redis docker image for HIGH/CRITICAL CVEs via trivy.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.AuditContainer(ctx, req, image.FullName())
}

// SBOM serves the source inventory by default and digest-bound image evidence
// under image scope. Redis selects its runtime image instead of building one,
// so the reference is known without a build and the agent can enumerate its own
// subjects when the caller supplies none.
func (s *Builder) SBOM(ctx context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if req.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		return s.Builder.SBOMContainer(ctx, image.FullName())
	}
	subjects := req.GetSubjects()
	if len(subjects) == 0 {
		// Enumerating subjects needs the service identity evidence is attributed
		// to, and that only exists once the service is loaded. Reading it
		// unloaded panics, and a recovered panic returns an empty response with
		// no error, which reads as absent evidence rather than a failure.
		if s.Identity == nil {
			return s.Builder.SBOMImageError(fmt.Errorf("image SBOM subjects can only be enumerated after the service is loaded"))
		}
		subjects = s.runtimeImageSubjects()
	}
	return s.Builder.SBOMImages(ctx, subjects)
}

// runtimeImageSubjects names one subject per shipped platform. Digest stays
// unset because the pinned reference addresses a manifest index: the scanner
// resolves each platform to its own child manifest and binds evidence to that
// child digest, which never equals the index digest a subject could carry.
func (s *Builder) runtimeImageSubjects() []*builderv0.ImageSubject {
	subjects := make([]*builderv0.ImageSubject, 0, len(runtimeImage.Platforms))
	for _, platform := range runtimeImage.Platforms {
		subjects = append(subjects, &builderv0.ImageSubject{
			Reference: image.FullName(),
			Platform:  platform,
			Role:      runtimeImageRole,
			Service:   s.Unique(),
		})
	}
	return subjects
}

// Upgrade reports a newer redis tag (within current major unless --major).
func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	res, err := upgrade.Docker(ctx, image.FullName(), upgrade.Options{
		IncludeMajor: req.IncludeMajor,
		DryRun:       req.DryRun,
	})
	if err != nil {
		return s.Builder.UpgradeError(err)
	}
	return s.Builder.UpgradeResponse(res.Changes, res.LockfileDiff)
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	s.Base.SetDockerImage(image)

	parameters := &deploymentTemplateParameters{}
	var restrictedConfiguration *v0.Configuration
	response, err := s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			configuration, prepareErr := s.prepareDeployment(ctx, deployment, parameters)
			if prepareErr != nil {
				return prepareErr
			}
			s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(configuration)))
			if services.IsRestrictedOutputProfile(deployment.Profile) {
				restrictedConfiguration = configuration
				return nil
			}
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
	if err != nil ||
		response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS ||
		restrictedConfiguration == nil {
		return response, err
	}
	response.Configuration = restrictedConfiguration
	return response, nil
}

func (s *Builder) prepareDeployment(
	ctx context.Context,
	deployment *services.KustomizeDeploymentContext,
	parameters *deploymentTemplateParameters,
) (*v0.Configuration, error) {
	req := deployment.Request
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.GetNetworkMappings(), s.TcpEndpoint, resources.NewContainerNetworkAccess())
	if err != nil {
		return nil, err
	}
	parameters.ServicePorts, err = s.servicePorts(ctx, req.GetNetworkMappings())
	if err != nil {
		return nil, err
	}
	// A headless Service has no kube-proxy in front of it: clients resolve it
	// straight to pod IPs and dial the port themselves, so port cannot differ
	// from targetPort. Aliases are exactly that fan-in, so they need a ClusterIP.
	parameters.Headless = len(parameters.ServicePorts) == 1
	if services.IsRestrictedOutputProfile(deployment.Profile) {
		passwordKey := resources.ServiceSecretConfigurationKeyFromUnique(s.Unique(), "redis", "REDIS_PASSWORD")
		passwordReference := deployment.Kubernetes.GetSecretReferences()[passwordKey]
		if passwordReference == nil {
			if s.RequirePass || s.Password != "" {
				return nil, fmt.Errorf("redis authentication requires a typed Kubernetes Secret reference for %s", passwordKey)
			}
		} else {
			if passwordReference.GetOptional() {
				return nil, fmt.Errorf("redis password Secret reference must not be optional")
			}
			parameters.PasswordReference = passwordReference
		}
		return s.restrictedConnectionConfiguration(instance), nil
	}
	configuration, err := s.CreateConnectionConfiguration(ctx, req.GetConfiguration(), instance)
	if err != nil {
		return nil, err
	}
	return configuration, nil
}

// servicePorts lists the ports the rendered Service publishes: one per TCP
// endpoint this service declares. Core hands each sibling alias of an API its
// own derived port, but redis answers all of them from a single process on
// 6379, so every advertised port has to be published and folded back onto it.
func (s *Builder) servicePorts(ctx context.Context, mappings []*v0.NetworkMapping) ([]servicePort, error) {
	var ports []servicePort
	for _, mapping := range mappings {
		endpoint := mapping.GetEndpoint()
		if endpoint.GetApi() != standards.TCP ||
			endpoint.GetModule() != s.TcpEndpoint.GetModule() ||
			endpoint.GetService() != s.TcpEndpoint.GetService() {
			continue
		}
		// An external endpoint is reached through its DNS entry from outside the
		// cluster, never through this Service, and core gives it a public
		// instance with no container view at all. Mirror that exclusion.
		if resources.IsExternalEndpoint(endpoint) {
			continue
		}
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, endpoint, resources.NewContainerNetworkAccess())
		if err != nil {
			return nil, err
		}
		// Naming a port after its number keeps it a valid IANA_SVC_NAME whatever
		// the endpoint is called: endpoint names are author-supplied and may be
		// longer than the 15 characters Kubernetes allows, or carry characters it
		// rejects, and nothing here renders a manifest the API server would take.
		ports = append(ports, servicePort{Name: fmt.Sprintf("redis-%d", instance.GetPort()), Port: instance.GetPort()})
	}
	// A multi-port Service must name every port; a single-port one need not, and
	// leaving it anonymous keeps single-endpoint Services rendering as they do today.
	if len(ports) == 1 {
		ports[0].Name = ""
	}
	return ports, nil
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	err := s.Templates(ctx, s.Information, services.WithFactory(factoryFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateErrorf(err, "cannot create endpoints")
	}

	s.Wool.Debug("created endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load tcp api")
	}
	endpoint := s.Base.BaseEndpoint(standards.TCP)
	endpoint.Visibility = resources.VisibilityExternal
	s.TcpEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create tcp endpoint")
	}
	s.Endpoints = []*v0.Endpoint{s.TcpEndpoint}
	return nil
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	_, err := asker.RunSequence(nil)
	return err
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
