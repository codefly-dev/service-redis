package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"
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

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return nil, err
		}
		if req.CreationMode.Communicate {
			// communication on CreateResponse
			err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](s.createCommunicate()))
			if err != nil {
				return s.Builder.LoadError(err)
			}
		}
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	return s.Builder.LoadResponse()
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

	return s.Builder.BuildResponse()
}

type Parameters struct {
	ReadSelector string
	ReadReplica  bool
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
		Origin: s.Base.Unique(),
		Infos: []*basev0.ConfigurationInformation{
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
	if !s.Settings.WithReadReplicas {
		readSelector = "write"
	}

	params := services.DeploymentParameters{
		ConfigMap:  cm,
		SecretMap:  secrets,
		Parameters: Parameters{ReadSelector: readSelector, ReadReplica: s.Settings.WithReadReplicas},
	}

	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	return s.Builder.DeployResponse()
}

func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: WithReadReplicas, Message: "Read replicas?", Description: "Split write and reads"}, true),
	}
}

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(s.Options()...)
}

type create struct {
	DatabaseName string
	TableName    string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	if s.Builder.CreationMode.Communicate {
		s.Wool.Debug("using communicate mode")

		session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.WithReadReplicas, err = session.Confirm(WithReadReplicas)
		if err != nil {
			return s.Builder.CreateError(err)
		}

	} else {
		options := s.Options()
		var err error
		s.Settings.WithReadReplicas, err = communicate.GetDefaultConfirm(options, WithReadReplicas)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	}
	err := s.Templates(ctx, create{}, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {

	write := s.Base.BaseEndpoint(standards.TCP)
	write.Name = "write"
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load TCP api")
	}
	s.write, err = resources.NewAPI(ctx, write, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create read tcp endpoint")
	}
	read := s.Base.BaseEndpoint(standards.TCP)
	read.Name = "read"
	s.read, err = resources.NewAPI(ctx, read, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create read tcp endpoint")
	}
	s.Endpoints = []*basev0.Endpoint{s.write, s.read}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
