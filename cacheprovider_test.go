package main

// cacheprovider_test.go — service-redis as a provider of codefly.dev/cache.
//
// The agent emits a "cache" configuration group naming the driver this
// repository ships (./cache). These tests read that group back the way a
// consumer does, open the driver from it, and hold the result to the
// interface's conformance suite against the redis this agent's own Runtime
// starts (runtimeredis_test.go): the pinned image under Docker and the
// nix-provisioned server, single-process and with read replicas.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
	"github.com/codefly-dev/interface-cache/go/cache/cachetest"
	rediscache "github.com/codefly-dev/service-redis/cache"
)

// emitted reads a configuration the agent produced, the way the SDK reads what
// codefly delivered to a consumer. Whether a key is secret is part of the
// interface, so reading a secret as plain configuration (or the reverse) fails.
type emitted struct{ conf *basev0.Configuration }

func (e emitted) Configuration(group, key string) (string, error) { return e.value(group, key, false) }
func (e emitted) Secret(group, key string) (string, error)        { return e.value(group, key, true) }

func (e emitted) value(group, key string, secret bool) (string, error) {
	for _, info := range e.conf.GetInfos() {
		if info.GetName() != group {
			continue
		}
		for _, v := range info.GetConfigurationValues() {
			if v.GetKey() != key {
				continue
			}
			if v.GetSecret() != secret {
				return "", fmt.Errorf("%s.%s: secret=%v, the interface reads it as secret=%v", group, key, v.GetSecret(), secret)
			}
			return v.GetValue(), nil
		}
	}
	return "", fmt.Errorf("%s.%s not emitted", group, key)
}

// connectionConfiguration is what the agent hands consumers for a redis at
// address guarded by password.
func connectionConfiguration(t *testing.T, address, password string) *basev0.Configuration {
	t.Helper()
	builder, _ := newDeploymentTestBuilder(t)
	builder.Password = password
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	// A consumer on the host reaches the server through the native view, as the
	// runtime's mappings provide it.
	instance := resources.NewNetworkInstance(host, uint16(port))
	instance.Access = resources.NewNativeNetworkAccess()
	conf, err := builder.CreateConnectionConfiguration(context.Background(), nil, instance)
	if err != nil {
		t.Fatal(err)
	}
	if conf == nil {
		t.Fatal("CreateConnectionConfiguration returned neither a configuration nor an error")
	}
	return conf
}

func TestCacheConfigurationFollowsTheInterface(t *testing.T) {
	conf := emitted{connectionConfiguration(t, "127.0.0.1:6379", "p@ss word")}

	driver, err := conf.Configuration(cacheiface.Group, cacheiface.KeyDriver)
	if err != nil {
		t.Fatal(err)
	}
	if driver != rediscache.DriverName {
		t.Fatalf("cache.driver = %q, want %q", driver, rediscache.DriverName)
	}
	connection, err := conf.Secret(cacheiface.Group, cacheiface.KeyConnection)
	if err != nil {
		t.Fatal(err)
	}
	redisConnection, err := conf.Secret("redis", "connection")
	if err != nil {
		t.Fatal(err)
	}
	if connection == "" || connection != redisConnection {
		t.Fatalf("cache.connection = %q, want the redis group's %q", connection, redisConnection)
	}
}

// A consumer that imports the driver opens it from the emitted group alone.
// No server is contacted until the first command.
func TestCacheOpensFromEmittedConfiguration(t *testing.T) {
	layer, err := cacheiface.Open(context.Background(), emitted{connectionConfiguration(t, "127.0.0.1:6379", "pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := layer.(*rediscache.Layer); !ok {
		t.Fatalf("cache.Open = %T, want *rediscache.Layer", layer)
	}
}

// The whole codefly.dev/cache suite, against redis as this agent's Runtime runs
// it, reached only through the configuration Init emitted: single-process and
// with read replicas, over both backends.
func TestRealRedisCacheOverDocker(t *testing.T) { testRuntimeCache(t, dockerBackend, 0) }
func TestRealRedisCacheOverNix(t *testing.T)    { testRuntimeCache(t, nixBackend, 0) }

func TestRealRedisCacheReplicasOverDocker(t *testing.T) { testRuntimeCache(t, dockerBackend, 2) }
func TestRealRedisCacheReplicasOverNix(t *testing.T)    { testRuntimeCache(t, nixBackend, 2) }

func testRuntimeCache(t *testing.T, backend redisBackend, replicas int) {
	password := fmt.Sprintf("cache-%s-%d-7PzQm2", backend, replicas)
	r := startRuntimeRedis(t, backend, replicas, password)
	runCacheInterface(t, r.consumer(t), password)
	if replicas > 0 {
		t.Run("Replicas", func(t *testing.T) { runReplicaCache(t, r) })
	}
}

// connectionHarness gives each namespace its own client on connection, as
// separate processes would have.
func connectionHarness(connection string, opts ...rediscache.Option) cachetest.Harness {
	return cachetest.Harness{New: func(t *testing.T, namespace string) cacheiface.Layer {
		return rediscache.New(redisClient(t, connection), append([]rediscache.Option{rediscache.WithPrefix(namespace)}, opts...)...)
	}}
}

// runCacheSuite is the interface's conformance, plus what a stack over the
// layer owes a consumer: a write elsewhere evicts its in-process copy.
func runCacheSuite(t *testing.T, harness cachetest.Harness) {
	t.Run("Conformance", func(t *testing.T) { cachetest.Run(t, harness) })
	t.Run("FillOnceAcrossProcesses", func(t *testing.T) { cachetest.RunStack(t, harness) })

	t.Run("RemoteWriteEvictsMemory", func(t *testing.T) {
		ctx := context.Background()
		ns := cachetest.Namespace(t)
		p := cacheiface.NewPartition("tenant-" + ns)
		stack := func() *cacheiface.Stack {
			s, err := cacheiface.New(ctx,
				cacheiface.WithTier(cacheiface.NewMemory(), time.Hour),
				cacheiface.WithTier(harness.New(t, ns), time.Hour),
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			return s
		}
		a, b := stack(), stack()
		if err := a.Set(ctx, p, "k", []byte("one")); err != nil {
			t.Fatal(err)
		}
		if v, err := b.Get(ctx, p, "k"); err != nil || string(v) != "one" {
			t.Fatalf("b.Get = %q, %v", v, err)
		}
		if err := a.Set(ctx, p, "k", []byte("two")); err != nil {
			t.Fatal(err)
		}
		eventuallyGets(t, b, p, "k", "two", 3*time.Second)
	})
}

// eventuallyGets waits for s to serve want for key.
func eventuallyGets(t *testing.T, s *cacheiface.Stack, p cacheiface.Partition, key, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		v, err := s.Get(context.Background(), p, key)
		if err == nil && string(v) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still serving %q (%v) %s after the write of %q", v, err, within, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runCacheInterface holds the redis conf describes to codefly.dev/cache,
// reached only through that configuration.
func runCacheInterface(t *testing.T, conf emitted, password string) {
	connection, err := conf.Secret(cacheiface.Group, cacheiface.KeyConnection)
	if err != nil {
		t.Fatal(err)
	}
	runCacheSuite(t, connectionHarness(connection))

	t.Run("OpenThroughTheInterface", func(t *testing.T) {
		ctx := context.Background()
		layer, err := cacheiface.Open(ctx, conf)
		if err != nil {
			t.Fatal(err)
		}
		key := cachetest.Namespace(t)
		if err := layer.Set(ctx, key, cacheiface.Entry{Value: []byte("v")}, time.Minute); err != nil {
			t.Fatalf("layer opened from the emitted configuration cannot write: %v", err)
		}
		if _, err := layer.Get(ctx, key); err != nil {
			t.Fatalf("layer opened from the emitted configuration cannot read: %v", err)
		}
	})

	t.Run("WrongPasswordIsRefused", func(t *testing.T) {
		u, err := url.Parse(connection)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("", "not-"+password)
		layer, err := rediscache.Open(context.Background(), cacheiface.Config{Driver: rediscache.DriverName, Connection: u.String()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := layer.Get(context.Background(), "k"); err == nil || errors.Is(err, cacheiface.ErrMiss) {
			t.Fatalf("Get with a wrong password = %v, want an authentication error", err)
		}
	})
}

// A stack whose redis is unreachable still serves from the origin. The URL
// tunes the client down so the breaker, not retries, bounds the cost.
func TestCacheDegradesWhenRedisIsUnreachable(t *testing.T) {
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close() // nothing listens there now

	layer, err := rediscache.Open(ctx, cacheiface.Config{
		Driver:     rediscache.DriverName,
		Connection: "redis://" + address + "?dial_timeout=100ms&max_retries=-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := cacheiface.SourceFunc(func(context.Context, string, string) (cacheiface.Entry, error) {
		return cacheiface.Entry{Value: []byte("from origin")}, nil
	})
	s, err := cacheiface.New(ctx, cacheiface.WithTier(layer, time.Minute), cacheiface.WithOrigin(origin))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 10 {
		v, err := s.Get(ctx, cacheiface.NewPartition("tenant"), "k")
		if err != nil || string(v) != "from origin" {
			t.Fatalf("Get with redis down = %q, %v; want the origin's value", v, err)
		}
	}
}
