package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"path"
)

type Builder struct {
	*Service
	NetworkMappings []*basev0.NetworkMapping
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	gettingStarted, err := templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return nil, err
	}

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](s.createCommunicate()))
	if err != nil {
		return s.Builder.LoadError(err)
	}

	return s.Builder.LoadResponse(gettingStarted)
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

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

	return &builderv0.BuildResponse{}, nil
}

type Parameters struct {
	ReadSelector string
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	var k *builderv0.KubernetesDeployment
	var err error
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}

	namespace := k.Namespace
	readService := fmt.Sprintf("redis://read-%s.%s.svc.cluster.local:6379", s.Base.Service.Name, namespace)
	writeService := fmt.Sprintf("redis://write-%s.%s.svc.cluster.local:6379", s.Base.Service.Name, namespace)

	conf := &basev0.Configuration{
		Origin: s.Base.Service.Unique(),
		Scope:  basev0.NetworkScope_Container,
		Configurations: []*basev0.ConfigurationInformation{
			{
				Name: "read",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: readService, Secret: true},
				},
			},
			{
				Name: "write",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: writeService, Secret: true},
				},
			},
		},
	}

	err = s.EnvironmentVariables.AddConfigurations(conf)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	s.Configuration = conf

	cm, err := services.EnvsAsConfigMapData(s.EnvironmentVariables.Configurations()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	readSelector := "read"
	if !s.Settings.ReadReplica {
		readSelector = "write"
	}

	params := services.DeploymentParameters{
		ConfigMap:  cm,
		SecretMap:  secrets,
		Parameters: Parameters{ReadSelector: readSelector},
	}

	base := s.Builder.CreateKubernetesBase(req.Environment, k.Namespace, k.BuildContext)
	err = s.deployKustomize(ctx, k, base, params)

	if err != nil {
		return s.Builder.DeployError(err)
	}
	return s.Builder.DeployResponse()
}

func (s *Builder) deployKustomize(ctx context.Context, k *builderv0.KubernetesDeployment, base *services.DeploymentBase, params any) error {
	wrapper := &services.DeploymentWrapper{DeploymentBase: base, Parameters: params}
	destination := path.Join(k.Destination, "applications", s.Base.Service.Application, "services", s.Base.Service.Name)
	err := s.Templates(ctx, wrapper,
		services.WithDeployment(deploymentFS, "kustomize/base").WithDestination(path.Join(destination, "base")),
		services.WithDeployment(deploymentFS, "kustomize/overlays/environment/write").WithDestination(path.Join(destination, "overlays", base.Environment.Name)))
	if err != nil {
		return err
	}

	if s.Settings.ReadReplica {
		err := s.Templates(ctx, wrapper, services.WithDeployment(deploymentFS, "kustomize/overlays/environment/replicas").WithDestination(path.Join(destination, "overlays", base.Environment.Name)))
		if err != nil {
			return err
		}
	}
	return nil
}

const ReadReplica = "read-replica"

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: ReadReplica, Message: "Read replicas?", Description: "Split write and reads"}, true),
	)
}

type create struct {
	DatabaseName string
	TableName    string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.ReadReplica, err = session.Confirm(ReadReplica)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	err = s.Templates(ctx, create{}, services.WithBuilder(builderFS))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	return s.Base.Builder.CreateResponse(ctx, s.Settings)
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
