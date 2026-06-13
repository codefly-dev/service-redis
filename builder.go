package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents/communicate"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
	"github.com/codefly-dev/core/agents/services/upgrade"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type Builder struct {
	services.BuilderServer
	*Service
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	s.Wool.Debug("base loaded", wool.Field("identity", s.Identity))

	if req.DisableCatch {
		s.Wool.DisableCatch()
	}

	requirements.Localize(s.Location)

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return nil, err
		}
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.TcpEndpoint, err = resources.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.Wool.Debug("endpoint", wool.Field("tcp", s.TcpEndpoint))

	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return s.Builder.SyncResponse()
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	// Redis doesn't need a custom build step — the official image is used as-is
	return s.Builder.BuildResponse()
}

// Audit scans the redis docker image for HIGH/CRITICAL CVEs via trivy.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	res, err := audit.Docker(ctx, image.FullName())
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditResponse(res.Findings, res.Outdated, res.Tool, res.Language)
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

	s.Builder.LogDeployRequest(req, s.Wool.Debug)

	s.EnvironmentVariables.SetRunning()

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.NetworkMappings, s.TcpEndpoint, resources.NewPublicNetworkAccess())
	if err != nil {
		return s.Builder.DeployError(err)
	}

	conf, err := s.CreateConnectionConfiguration(ctx, req.Configuration, instance)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, conf)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	s.Configuration = conf

	s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(conf)))

	configs, err := s.EnvironmentVariables.Configurations()
	if err != nil {
		return s.Builder.DeployError(err)
	}
	cm, err := services.EnvsAsConfigMapData(configs...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap: cm,
		SecretMap: secrets,
	}
	var k *builderv0.KubernetesDeployment
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}
	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}
	return s.Builder.DeployResponse()
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
