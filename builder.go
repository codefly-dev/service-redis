package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/agents/communicate"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/wool"
	"path"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
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

	err = s.LoadEndpoints(ctx)
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

	s.Wool.Debug("init", wool.Field("endpoints proposed", configurations.MakeNetworkMappingSummary(req.ProposedNetworkMappings)))

	// Get the write mapping
	writeMapping, err := configurations.FindNetworkMapping(s.write, req.ProposedNetworkMappings)

	if err != nil {
		return s.Builder.InitError(err)
	}

	if s.Settings.ReadReplica {
		s.NetworkMappings = []*basev0.NetworkMapping{writeMapping}
		// Create a network mapping for read to go to write
		replica := &basev0.NetworkMapping{
			Endpoint: s.read,
			Host:     writeMapping.Host,
			Port:     writeMapping.Port,
			Address:  writeMapping.Address,
		}
		s.NetworkMappings = append(s.NetworkMappings, replica)
	}

	s.DependencyEndpoints = req.DependenciesEndpoints

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

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Envs []Env
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {

	return &builderv0.BuildResponse{}, nil
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	base := s.Builder.CreateDeploymentBase(req.Environment, req.Deployment.Namespace, req.BuildContext)
	params := services.DeploymentTemplateInput{
		Image:       image,
		Information: s.Information,
	}
	switch v := req.Deployment.Kind.(type) {
	case *builderv0.Deployment_Kustomize:
		err := s.deployKustomize(ctx, v, base, params)
		if err != nil {
			return s.Builder.DeployError(err)
		}
	}
	return s.Builder.DeployResponse()
}

func (s *Builder) deployKustomize(ctx context.Context, v *builderv0.Deployment_Kustomize, base *services.DeploymentBase, params any) error {
	wrapper := &services.DeploymentWrapper{DeploymentBase: base, Parameters: params}
	destination := path.Join(v.Kustomize.Destination, "applications", s.Configuration.Application, "services", s.Configuration.Name)
	err := s.Templates(ctx, wrapper,
		services.WithDeployment(deploymentFS, "kustomize/base").WithDestination(path.Join(destination, "base")),
		services.WithDeployment(deploymentFS, "kustomize/overlays/environment/write").WithDestination(path.Join(destination, "overlays", base.Environment.Name)))
	if err != nil {
		return err
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
