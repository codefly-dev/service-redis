package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/wool"
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

	writeMapping = configurations.PrefixNetworkMapping(writeMapping, "write")

	readMapping, err := configurations.FindNetworkMapping(s.read, req.ProposedNetworkMappings)
	if err != nil {
		return s.Builder.InitError(err)
	}

	readMapping = configurations.PrefixNetworkMapping(readMapping, "read")

	s.NetworkMappings = []*basev0.NetworkMapping{writeMapping, readMapping}

	s.DependencyEndpoints = req.DependenciesEndpoints

	// This is the credential exposed to dependencies
	s.ServiceProviderInfos = []*basev0.ProviderInformation{
		{Name: "redis",
			Origin: s.Service.Configuration.Unique(),
			Data: map[string]string{
				"read":  s.CreateConnectionString(ctx, readMapping.Address),
				"write": s.CreateConnectionString(ctx, writeMapping.Address),
			},
		},
	}
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
	SecretMap    services.EnvironmentMap
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	// Only expose the "connections"

	info := &basev0.ProviderInformation{
		Name:   "redis",
		Origin: s.Configuration.Unique(),
	}
	var envs []string

	writeKey := configurations.ProviderInformationEnvKey(info, "write")
	writeMapping, err := configurations.FindNetworkMapping(s.write, s.NetworkMappings)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	envs = append(envs, fmt.Sprintf("%s=%s", writeKey, s.CreateConnectionString(ctx, writeMapping.Address)))

	readKey := configurations.ProviderInformationEnvKey(info, "read")
	readMapping, err := configurations.FindNetworkMapping(s.read, s.NetworkMappings)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	envs = append(envs, fmt.Sprintf("%s=%s", readKey, s.CreateConnectionString(ctx, readMapping.Address)))

	secret, err := services.EnvsAsSecretData(envs...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	base := s.Builder.CreateDeploymentBase(req.Environment, req.Deployment.Namespace, req.BuildContext)

	readSelector := "read"
	if !s.Settings.ReadReplica {
		readSelector = "write"
	}

	params := Parameters{
		SecretMap:    secret,
		ReadSelector: readSelector,
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
