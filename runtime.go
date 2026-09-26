package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"google.golang.org/protobuf/proto"
)

// redisDockerRuntime retains the exact environment acquired during Init.
// A failed shutdown must leave this handle available for retry.
type redisDockerRuntime interface {
	Init(context.Context) error
	Shutdown(context.Context) error
}

type Runtime struct {
	*services.DefaultRuntime
	*Service

	// internal
	runnerEnvironment redisDockerRuntime
	dockerDestroyMu   sync.Mutex

	// nixRuntime is set instead of runnerEnvironment when the caller requests
	// RuntimeContextNix — redis runs natively from a nix-provisioned binary.
	// It is this invocation's ownership record for that process: whoever holds
	// it is responsible for stopping it, and it is guarded because the
	// supervision goroutine armed by Start reads it while Stop and Destroy
	// clear it.
	nixRuntime   *nixRedis
	nixRuntimeMu sync.Mutex

	redisPort uint16
	// replicaAddresses are the host addresses of the read replicas Init
	// started, in order; empty without read replicas. Readiness covers each.
	replicaAddresses []string
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

// initializeDockerRuntime serializes acquisition and initialization with
// destruction. Retain even a failed initialization's handle for explicit retry.
func (s *Runtime) initializeDockerRuntime(ctx context.Context, runner redisDockerRuntime) error {
	s.dockerDestroyMu.Lock()
	defer s.dockerDestroyMu.Unlock()
	s.runnerEnvironment = runner
	return runner.Init(ctx)
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
			if err := s.resolveTCPEndpoints(ctx, endpoints); err != nil {
				return s.Wool.Wrapf(err, "cannot find TCP endpoint")
			}
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

	replicas, err := s.replicaCount()
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if replicas > 0 && s.ReadEndpoint == nil {
		return s.Runtime.InitError(w.NewError("with-read-replicas is set but no endpoint was resolved for the replicas"))
	}
	var replicaEndpoint *basev0.Endpoint
	if replicas > 0 {
		replicaEndpoint = s.ReadEndpoint
	}

	// Without replicas this runtime serves one Redis process, and every TCP
	// alias reports that process's actual addresses rather than the separate
	// ports the orchestrator proposed before it knew the serving endpoint.
	// With replicas the serving endpoint is the primary and every other alias
	// reports the replicas' addresses.
	mappings, err := redisRuntimeMappings(ctx, req.ProposedNetworkMappings, s.TcpEndpoint, replicaEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.NetworkMappings = mappings

	configuration := req.GetConfiguration()

	serving, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if serving == nil {
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

	// The first replica serves the replica endpoint's assigned port. A local
	// alias has one address, so replicas beyond the first have no endpoint of
	// their own: they take free loopback ports, replicate and are held to the
	// same readiness, but no local consumer is routed to them. In Kubernetes
	// the read Service balances across all of them.
	var replicaPorts []uint16
	s.replicaAddresses = nil
	if replicas > 0 {
		readInstance, errRead := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, replicaEndpoint, resources.NewNativeNetworkAccess())
		if errRead != nil {
			return s.Runtime.InitError(errRead)
		}
		if readInstance.GetPort() == instance.GetPort() {
			return s.Runtime.InitError(w.NewError("endpoint %q and the replicas' endpoint %q resolve to the same port %d; the replicas need their own", s.TcpEndpoint.GetName(), replicaEndpoint.GetName(), instance.GetPort()))
		}
		extra, errPorts := reserveLoopbackPorts(replicas - 1)
		if errPorts != nil {
			return s.Runtime.InitError(errPorts)
		}
		replicaPorts = append([]uint16{uint16(readInstance.GetPort())}, extra...)
		s.replicaAddresses = append(s.replicaAddresses, readInstance.GetAddress())
		for _, port := range extra {
			s.replicaAddresses = append(s.replicaAddresses, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		}
	}

	// Create connection string resources for the network instance
	for _, inst := range serving.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		if replicas > 0 {
			readInstance, errRead := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, replicaEndpoint, inst.GetAccess())
			if errRead != nil {
				return s.Runtime.InitError(errRead)
			}
			s.addReadConnection(conf, readInstance)
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
		stateKey := redisStateKey(s.Location, s.Environment.GetNamingScope())
		nixr, errNix := newNixRedis(ctx, stateKey, uint16(instance.Port), s.redisPassword, newRedisLogWriter(s.Wool))
		if errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		// Take ownership before Init can spawn anything. A handle retained only
		// on success leaves a failed Init's server running with nothing left to
		// reach it; retaining it here means a failed rollback still has an owner
		// that Stop and Destroy can retry.
		s.setNativeRuntime(nixr)
		if errNix = nixr.attachReplicas(ctx, stateKey, replicaPorts); errNix != nil {
			return s.Runtime.InitError(errors.Join(errNix, nixr.Stop(ctx)))
		}
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
		// Replicas run in the primary's container, each on its own internal
		// port replicating the primary over the container's loopback: the
		// docker runner offers no network between containers, and one
		// container per set is also the failure domain the nix runtime has.
		for i, port := range replicaPorts {
			runner.WithPortMapping(ctx, port, redisContainerReplicaPort(i))
		}
		if s.redisPassword != "" {
			runner.WithEnvironmentVariables(ctx,
				resources.Env("REDIS_PASSWORD", s.redisPassword),
			)
		}
		if command := redisDockerCommand(s.redisPassword != "", replicas); command != nil {
			runner.WithCommand(command...)
		}
		w.Debug("init for runner environment: will start container")
		if errDocker = s.initializeDockerRuntime(ctx, runner); errDocker != nil {
			return s.Runtime.InitError(errDocker)
		}
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

// redisRuntimeMappings folds every declared TCP alias onto the process that
// serves it: the serving endpoint's own views, or, when replicas is set, the
// serving endpoint for itself and replicas' views for every other alias.
func redisRuntimeMappings(ctx context.Context, proposed []*basev0.NetworkMapping, serving *basev0.Endpoint, replicas *basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	views, err := redisAccessViews(ctx, proposed, serving, "serving")
	if err != nil {
		return nil, err
	}
	replicaViews := views
	if replicas != nil {
		if replicas.GetName() == serving.GetName() {
			return nil, fmt.Errorf("redis replicas cannot serve the primary's endpoint %q", serving.GetName())
		}
		if replicaViews, err = redisAccessViews(ctx, proposed, replicas, "replica"); err != nil {
			return nil, err
		}
	}
	accepted := make([]*basev0.NetworkMapping, 0, len(proposed))
	for _, mapping := range proposed {
		if mapping.GetEndpoint() == nil {
			return nil, fmt.Errorf("redis network mapping is missing its endpoint")
		}
		copy := proto.Clone(mapping).(*basev0.NetworkMapping)
		if mapping.GetEndpoint().GetApi() == "tcp" {
			if mapping.Endpoint.Module != serving.Module || mapping.Endpoint.Service != serving.Service {
				return nil, fmt.Errorf("redis TCP alias belongs to another service")
			}
			if len(mapping.GetInstances()) == 0 {
				return nil, fmt.Errorf("redis TCP alias has no access views")
			}
			target := replicaViews
			if mapping.Endpoint.Name == serving.GetName() {
				target = views
			}
			for i, instance := range mapping.GetInstances() {
				access := instance.GetAccess().GetKind()
				actual := target[access]
				if access == "" || actual == nil {
					return nil, fmt.Errorf("serving Redis endpoint has no matching access view for %s", mapping.GetEndpoint().GetName())
				}
				copy.Instances[i] = proto.Clone(actual).(*basev0.NetworkInstance)
			}
		}
		accepted = append(accepted, copy)
	}
	return accepted, nil
}

// redisAccessViews indexes the proposed mapping of endpoint by access kind.
func redisAccessViews(ctx context.Context, proposed []*basev0.NetworkMapping, endpoint *basev0.Endpoint, role string) (map[string]*basev0.NetworkInstance, error) {
	mapping, err := resources.FindNetworkMapping(ctx, proposed, endpoint)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return nil, fmt.Errorf("%s Redis network mapping is missing", role)
	}
	views := make(map[string]*basev0.NetworkInstance)
	for _, instance := range mapping.GetInstances() {
		access := instance.GetAccess().GetKind()
		if access == "" || views[access] != nil {
			return nil, fmt.Errorf("%s Redis endpoint has a missing or duplicate access view", role)
		}
		views[access] = instance
	}
	return views, nil
}

// redisContainerReplicaPort is the container-internal port of replica i, which
// its assigned host port maps onto, as the primary's maps onto 6379.
func redisContainerReplicaPort(i int) uint16 { return 6380 + uint16(i) }

// redisDockerCommand runs the primary and, before it, one replica per
// replicas, or returns nil when the image's own command already does: no
// password and no replicas.
//
// Keep the password out of docker inspect's process argv. The fixed shell
// fragment expands the container environment variable inside the container.
func redisDockerCommand(password bool, replicas int) []string {
	if !password && replicas == 0 {
		return nil
	}
	var auth, replicaAuth string
	if password {
		auth = ` --requirepass "$REDIS_PASSWORD"`
		replicaAuth = auth + ` --masterauth "$REDIS_PASSWORD"`
	}
	var script strings.Builder
	for i := range replicas {
		fmt.Fprintf(&script, "redis-server --port %d --replicaof 127.0.0.1 6379 --dbfilename replica-%d.rdb%s & ",
			redisContainerReplicaPort(i), i+1, replicaAuth)
	}
	script.WriteString("exec redis-server" + auth)
	return []string{"sh", "-c", script.String()}
}

// reserveLoopbackPorts returns n loopback ports free at the time of the call.
// They are held together until all are chosen, so the n are distinct.
func reserveLoopbackPorts(n int) ([]uint16, error) {
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make([]uint16, 0, n)
	for range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve a loopback port for a redis replica: %w", err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, uint16(listener.Addr().(*net.TCPAddr).Port))
	}
	return ports, nil
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
		onAttemptFailed: func(probeErr error) {
			s.Wool.Debug("waiting for redis to be ready", wool.ErrField(probeErr))
		},
	}); err != nil {
		return s.Wool.Wrapf(err, "redis is not ready")
	}
	// Every replica, not just the one behind the read endpoint: each is only
	// ready once its link to the primary is up.
	for i, replica := range s.replicaAddresses {
		if err = waitForRedisPong(ctx, redisWaitOptions{
			address:  replica,
			password: s.redisPassword,
			budget:   redisDockerReadinessBudget,
			replica:  true,
			onAttemptFailed: func(probeErr error) {
				s.Wool.Debug("waiting for redis replica to be ready", wool.Field("replica", i+1), wool.ErrField(probeErr))
			},
		}); err != nil {
			return s.Wool.Wrapf(err, "redis replica %d is not ready", i+1)
		}
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
	// Destroy owns only the container generation acquired by this invocation's
	// Init, including partially initialized handles. A fresh environment has no
	// acquired ID, so Shutdown would silently do nothing; resolving the name
	// again could instead destroy a successor owned by a different invocation.
	s.dockerDestroyMu.Lock()
	defer s.dockerDestroyMu.Unlock()
	if s.runnerEnvironment == nil {
		return s.Runtime.DestroyResponse()
	}
	if err := s.runnerEnvironment.Shutdown(ctx); err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}
