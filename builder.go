package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/configurations/standards"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
	"path"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

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

	s.Wool.Debug("init", wool.Field("endpoints proposed", configurations.MakeNetworkMappingSummary(req.ProposedNetworkMappings)))

	// Get the write mapping
	writeMapping := configurations.FindMapping(req.ProposedNetworkMappings, s.write)

	net, err := configurations.GetMappingInstanceForName(ctx, req.ProposedNetworkMappings, standards.TCP, "write")
	if err != nil {
		return s.Builder.InitError(err)
	}

	if s.Settings.Replicas == 0 {
		s.NetworkMappings = []*basev0.NetworkMapping{writeMapping}
		// Create a network mapping for read to go to write
		replica := &basev0.NetworkMapping{
			Application: s.read.Application,
			Service:     s.read.Service,
			Endpoint:    s.read,
			Addresses:   []string{net.Address},
		}
		s.NetworkMappings = append(s.NetworkMappings, replica)
	}

	s.DependencyEndpoints = req.DependenciesEndpoints

	return s.Builder.InitResponse(s.NetworkMappings, nil)
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

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	base := s.Builder.CreateDeploymentBase(req.Environment, req.BuildContext)

	params := DeploymentParameter{
		Image:       image,
		Information: s.Information,
		Deployment:  Deployment{Replicas: s.Settings.Replicas},
	}

	switch v := req.Deployment.Kind.(type) {
	case *builderv0.Deployment_Kustomize:
		err := s.deployKustomize(ctx, v, base, params)
		if err != nil {
			return nil, err
		}
	}

	return s.Builder.DeployResponse()
}

func (s *Builder) deployKustomize(ctx context.Context, v *builderv0.Deployment_Kustomize, base *services.DeploymentBase, params any) error {

	if s.Settings.Replicas == 0 {
		wrapper := &services.DeploymentWrapper{DeploymentBase: base, Parameters: params}
		destination := path.Join(v.Kustomize.Destination, s.Configuration.Unique())
		err := s.Templates(ctx, wrapper,
			services.WithDeployment(deploymentFS, "kustomize/base").WithDestination(path.Join(destination, "base")),
			services.WithDeployment(deploymentFS, "kustomize/overlays/environment/main").WithDestination(path.Join(destination, "overlays", base.Environment.Name)))
		if err != nil {
			return err
		}
	}
	return nil
}

const Replicas = "replicas"

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewIntInput(&agentv0.Message{Name: Replicas, Message: "Read replicas?", Description: "Split write and reads"}, 1),
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

	s.Settings.Replicas, err = session.GetIntString(Replicas)
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
