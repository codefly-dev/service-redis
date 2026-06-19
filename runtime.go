package main

import (
	"context"
	"net"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
)

type Runtime struct {
	services.RuntimeServer
	*Service

	// internal
	runnerEnvironment *dockerrun.DockerEnvironment

	redisPort uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogLoadRequest(req)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	s.Runtime.SetEnvironment(req.Environment)

	requirements.Localize(s.Location)

	// Endpoints
	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "cannot load endpoints")
	}

	s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	s.TcpEndpoint, err = resources.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "cannot find TCP endpoint")
	}

	return s.Runtime.LoadResponse()
}

func CallingContext() *basev0.NetworkAccess {
	return resources.NewNativeNetworkAccess()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Configuration = req.Configuration

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	s.redisPort = 6379

	// Create connection string resources for the network instance
	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, s.Configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}
	s.Wool.Debug("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	// Docker
	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(newRedisLogWriter(s.Wool))
	runner.WithPortMapping(ctx, uint16(instance.Port), s.redisPort)

	// Load password from configuration
	err = s.LoadConfiguration(ctx, s.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if s.redisPassword != "" {
		runner.WithEnvironmentVariables(ctx,
			resources.Env("REDIS_PASSWORD", s.redisPassword),
		)
		runner.WithCommand("redis-server", "--requirepass", s.redisPassword)
	}

	s.runnerEnvironment = runner

	w.Debug("init for runner environment: will start container")
	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot find network instance")
	}

	// instance.Host is already host:port form ("localhost:65350"), while
	// instance.Address is the same string. The previous "%s:%d" with
	// Host doubled the port and produced "localhost:65350:65350" which
	// trips net.Dial's "too many colons" error. Use Address directly.
	address := instance.Address
	s.Wool.Debug("waiting for redis to be ready", wool.Field("address", address))

	maxRetry := 10
	for retry := 0; retry < maxRetry; retry++ {
		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err == nil {
			// Send PING command
			_, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
			if err == nil {
				buf := make([]byte, 64)
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, readErr := conn.Read(buf)
				conn.Close()
				if readErr == nil && n > 0 {
					s.Wool.Debug("redis is ready!")
					return nil
				}
			}
			conn.Close()
		}
		s.Wool.Debug("waiting for redis to be ready", wool.ErrField(err))
		time.Sleep(2 * time.Second)
	}
	return s.Wool.NewError("redis is not ready")
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	err = runner.Shutdown(ctx)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}
