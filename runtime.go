package main

import (
	"context"
	"sync"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
)

type Runtime struct {
	*services.DefaultRuntime
	*Service

	// internal
	runnerEnvironment *dockerrun.DockerEnvironment

	// nixRuntime is set instead of runnerEnvironment when the caller requests
	// RuntimeContextNix — redis runs natively from a nix-provisioned binary.
	// It is this invocation's ownership record for that process: whoever holds
	// it is responsible for stopping it, and it is guarded because the
	// supervision goroutine armed by Start reads it while Stop and Destroy
	// clear it.
	nixRuntime   *nixRedis
	nixRuntimeMu sync.Mutex

	redisPort uint16
}

func NewRuntime() *Runtime {
	service := NewService()
	return &Runtime{
		DefaultRuntime: services.NewDefaultRuntime(service.Runtime),
		Service:        service,
	}
}

func (s *Runtime) setNativeRuntime(nixr *nixRedis) {
	s.nixRuntimeMu.Lock()
	defer s.nixRuntimeMu.Unlock()
	s.nixRuntime = nixr
}

func (s *Runtime) nativeRuntime() *nixRedis {
	s.nixRuntimeMu.Lock()
	defer s.nixRuntimeMu.Unlock()
	return s.nixRuntime
}

// releaseNativeRuntime stops the native redis this runtime owns, if any, and
// gives up ownership. A failure keeps the handle so the caller can retry rather
// than losing track of a process that may still be alive.
func (s *Runtime) releaseNativeRuntime(ctx context.Context) error {
	nixr := s.nativeRuntime()
	if nixr == nil {
		return nil
	}
	if err := nixr.Stop(ctx); err != nil {
		return err
	}
	s.setNativeRuntime(nil)
	return nil
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
			endpoint, err := resolveServingTCPEndpoint(ctx, endpoints)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find TCP endpoint")
			}
			s.TcpEndpoint = endpoint
			return nil
		},
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	configuration := req.GetConfiguration()

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	// The redis container publishes its port to the agent host, and the port
	// mapping plus readiness probe run in this host agent process. Always select
	// the native mapping for those: a container mapping such as
	// host.docker.internal shares the same port but is not a portable hostname on
	// the host itself (notably on Linux and several macOS Docker backends), so
	// probing it reports "redis is not ready" even though the container is up.
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, resources.NewNativeNetworkAccess())
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
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}
	s.Wool.Debug("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	// Load password from configuration — needed by both runtimes.
	if err = s.LoadConfiguration(ctx, configuration); err != nil {
		return s.Runtime.InitError(err)
	}

	// Nix runtime: run redis natively from a nix-provisioned binary instead of a
	// Docker container — selected when the caller requests RuntimeContextNix
	// (e.g. a host without Docker). Same port, so WaitForReady is unchanged.
	if rc := req.GetRuntimeContext(); rc != nil && rc.Kind == resources.RuntimeContextNix {
		s.Infof("using nix runtime for redis on port %d", instance.Port)
		// Release whatever a previous Init left running before building its
		// replacement. Adopting a second handle over a live one drops the only
		// reference to that process: it keeps the assigned port, the new server
		// cannot bind, and nothing is left that can stop either of them. It also
		// still holds the exclusive claim on the runtime root, so a same-scope
		// re-Init would wait out claimRuntimeRoot and then be refused as if a
		// foreign invocation owned it.
		//
		// This is the point Init commits to replacing the server, and not a line
		// earlier: a request that fails validation must leave a working redis
		// working. Ownership is retained across such a failure, so the server is
		// still reachable by Stop and Destroy — it is not orphaned by surviving.
		if errRelease := s.releaseNativeRuntime(ctx); errRelease != nil {
			return s.Runtime.InitError(errRelease)
		}
		nixr, errNix := newNixRedis(ctx, redisStateKey(s.Location, s.Environment.GetNamingScope()), uint16(instance.Port), s.redisPassword, newRedisLogWriter(s.Wool))
		if errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		// Take ownership before Init can spawn anything. A handle retained only
		// on success leaves a failed Init's server running with nothing left to
		// reach it; retaining it here means a failed rollback still has an owner
		// that Stop and Destroy can retry.
		s.setNativeRuntime(nixr)
		if errNix = nixr.Init(ctx); errNix != nil {
			return s.Runtime.InitError(errNix)
		}
	} else {
		// Docker: container redis on 6379, mapped to the assigned port. A caller
		// that switches backends between Inits still has to have its native
		// server released — the docker container would otherwise be a second
		// redis contending for the same port.
		if errRelease := s.releaseNativeRuntime(ctx); errRelease != nil {
			return s.Runtime.InitError(errRelease)
		}
		runner, errDocker := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
		if errDocker != nil {
			return s.Runtime.InitError(errDocker)
		}
		runner.WithOutput(newRedisLogWriter(s.Wool))
		runner.WithPortMapping(ctx, uint16(instance.Port), s.redisPort)
		if s.redisPassword != "" {
			runner.WithEnvironmentVariables(ctx,
				resources.Env("REDIS_PASSWORD", s.redisPassword),
			)
			runner.WithCommand(redisDockerCommand()...)
		}
		s.runnerEnvironment = runner
		w.Debug("init for runner environment: will start container")
		if errDocker = s.runnerEnvironment.Init(ctx); errDocker != nil {
			return s.Runtime.InitError(errDocker)
		}
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

func redisDockerCommand() []string {
	// Keep the password out of docker inspect's process argv. The fixed shell
	// fragment expands the container environment variable inside the container.
	return []string{"sh", "-c", `exec redis-server --requirepass "$REDIS_PASSWORD"`}
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Probe over the native (localhost) mapping, matching the port published in
	// Init — host.docker.internal is not reachable from this host agent process.
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot find network instance")
	}

	// instance.Host is already host:port form ("localhost:65350"), while
	// instance.Address is the same string. The previous "%s:%d" with
	// Host doubled the port and produced "localhost:65350:65350" which
	// trips net.Dial's "too many colons" error. Use Address directly.
	address := instance.Address
	s.Wool.Debug("waiting for redis to be ready", wool.Field("address", address))

	if err = waitForRedisPong(ctx, redisWaitOptions{
		address:  address,
		password: s.redisPassword,
		budget:   redisDockerReadinessBudget,
	}); err != nil {
		return s.Wool.Wrapf(err, "redis is not ready")
	}

	s.Wool.Debug("redis is ready!")
	return nil
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
	// Commit STARTED before arming supervision. StartResponse and
	// MarkRunnerExited write the same status under the same lock, so ordering is
	// by wall clock: arming first would let a death already buffered in the exit
	// channel flip the status to ERROR, and this response would then clobber it
	// back to STARTED — masking exactly the death supervision exists to report.
	resp, err := s.Runtime.StartResponse()
	if err != nil {
		return resp, err
	}
	// Docker redis is supervised by the container engine; the native process has
	// no supervisor but this one.
	if nixr := s.nativeRuntime(); nixr != nil {
		s.superviseNative(nixr)
	}
	return resp, nil
}

// superviseNative reports a native redis that dies without being stopped, so
// the orchestrator observes a failed start instead of leaving dependents to
// spin on connection refused.
func (s *Runtime) superviseNative(nixr *nixRedis) {
	nixr.Supervise(func(err error) {
		// A handle the runtime no longer owns belongs to a superseded
		// generation — a re-Init replaced it — and its exit says nothing about
		// the redis serving now.
		if s.nativeRuntime() != nixr {
			return
		}
		if err != nil {
			s.Wool.Error("native redis exited unexpectedly", wool.ErrField(err))
		} else {
			s.Wool.Error("native redis exited unexpectedly (clean exit, not stopped)")
		}
		s.Runtime.MarkRunnerExited(err)
	})
}

// Stop releases the execution resources this invocation owns and retains its
// data. For the native runtime that means terminating the redis process and
// freeing the assigned port; the dataset is written out on shutdown and the
// data directory and config stay in place for the next Init. A runtime that
// never ran Init owns nothing and stops nothing.
//
// keep-running opts out: the server stays up for warm reuse. The Docker runtime
// keeps its container running across Stop as it always has; moving it onto the
// same release contract is a separate policy change, not part of owning the
// native process.
func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if s.Settings.KeepRunning {
		s.Wool.Debug("keep-running is set: leaving redis up for reuse")
		return s.Runtime.StopResponse()
	}

	if s.nativeRuntime() != nil {
		if err := s.releaseNativeRuntime(ctx); err != nil {
			// The handle is kept: the process may still be alive, and dropping
			// the only reference to it is how it becomes an orphan. Destroy
			// retries.
			return s.Runtime.StopError(err)
		}
		s.Wool.Debug("stopped native redis: port released, data retained")
		return s.Runtime.StopResponse()
	}

	s.Wool.Debug("nothing to stop: keep environment alive")

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Nix runtime: terminate the native redis process; there is no container.
	// Stopping an already-stopped handle is a no-op, and a handle from a failed
	// Init still points at whatever that Init managed to spawn, so this is the
	// retry path for both.
	if s.nativeRuntime() != nil {
		if err := s.releaseNativeRuntime(ctx); err != nil {
			return s.Runtime.DestroyError(err)
		}
		return s.Runtime.DestroyResponse()
	}
	// A native invocation has no container to remove, and reaching for one would
	// demand a docker daemon from a host that was very likely chosen for not
	// having one.
	if s.Runtime.IsNixRuntime() {
		return s.Runtime.DestroyResponse()
	}

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
