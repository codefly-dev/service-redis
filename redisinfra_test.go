package main

// redisinfra_test.go — readiness against a real redis server.
//
// The socket fixtures in redisprobe_test.go pin the protocol contract; they do
// not prove redis itself agrees with it. These tests run the actual server —
// the pinned runtime image under Docker, and the nix-provisioned binary through
// the native runtime — with real credentials.
//
// They skip when the backend is unavailable. Set REDIS_INFRA_TESTS=required
// (the CI infrastructure job does) to turn a skip into a failure so the job
// cannot silently pass without exercising a real server.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

func requireInfrastructure(t *testing.T, missing string) {
	t.Helper()
	if os.Getenv("REDIS_INFRA_TESTS") == "required" {
		t.Fatalf("infrastructure tests are required but %s", missing)
	}
	t.Skipf("skipping: %s", missing)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		requireInfrastructure(t, "docker is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		requireInfrastructure(t, fmt.Sprintf("docker is not usable: %v (%s)", err, strings.TrimSpace(string(out))))
	}
}

// startRealRedis runs the pinned runtime image the same way Runtime.Init does —
// same image, same password plumbing, same command — and returns the published
// host address.
func startRealRedis(t *testing.T, password string) string {
	t.Helper()

	name := fmt.Sprintf("service-redis-infra-%d-%d", os.Getpid(), time.Now().UnixNano())
	args := []string{"run", "--detach", "--rm", "--name", name, "--publish", "127.0.0.1::6379"}
	if password != "" {
		args = append(args, "--env", "REDIS_PASSWORD="+password)
	}
	args = append(args, image.FullName())
	if password != "" {
		args = append(args, redisDockerCommand()...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", name).Run()
	})

	published, err := exec.CommandContext(ctx, "docker", "port", name, "6379/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	address := strings.TrimSpace(strings.SplitN(string(published), "\n", 2)[0])
	if address == "" {
		t.Fatal("docker published no host port for 6379/tcp")
	}
	return address
}

func TestRealRedisReadinessOverDocker(t *testing.T) {
	requireDocker(t)

	// Distinct credentials per row: a probe that ignores the password would pass
	// every row, including the cross-checks below.
	const passwordA = "infra-alpha-1LtCz3"
	const passwordB = "infra-bravo-9QsRn7"

	withAuth := startRealRedis(t, passwordA)
	otherAuth := startRealRedis(t, passwordB)
	withoutAuth := startRealRedis(t, "")

	// Probing straight after `docker run` returns exercises the retry path
	// against a real server that is not serving yet.
	for _, ready := range []struct {
		address  string
		password string
	}{
		{address: withAuth, password: passwordA},
		{address: otherAuth, password: passwordB},
		{address: withoutAuth},
	} {
		if err := waitForRedisPong(context.Background(), redisWaitOptions{
			address:  ready.address,
			password: ready.password,
			budget:   redisDockerReadinessBudget,
		}); err != nil {
			t.Fatalf("waitForRedisPong(%s): %v", ready.address, err)
		}
	}

	for _, tc := range []struct {
		name     string
		address  string
		password string
	}{
		{name: "wrong password", address: withAuth, password: passwordB},
		{name: "missing password", address: withAuth},
		{name: "password against an open server", address: withoutAuth, password: passwordA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := probeRedis(context.Background(), tc.address, tc.password)
			if err == nil {
				t.Fatal("probeRedis reported a real redis ready with the wrong credentials")
			}
			var probeErr *redisProbeError
			if !errors.As(err, &probeErr) {
				t.Fatalf("error is not a *redisProbeError: %v", err)
			}
			if probeErr.retryable {
				t.Fatalf("credential mismatch reported as retryable: %v", err)
			}
			if tc.password != "" && strings.Contains(err.Error(), tc.password) {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestRealRedisReadinessOverNix(t *testing.T) {
	if !runners.CheckNixInstalled() {
		requireInfrastructure(t, "nix is not installed")
	}

	const password = "infra-nix-4WhKv8"

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()

	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	server, err := newNixRedis(ctx, t.TempDir(), port, password, io.Discard)
	if err != nil {
		t.Fatalf("newNixRedis: %v", err)
	}
	// Init ends in waitReady, so a server that never authenticates fails here.
	if err = server.Init(ctx); err != nil {
		t.Fatalf("nix redis Init: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	if err = probeRedis(ctx, address, "not-"+password); err == nil {
		t.Fatal("probeRedis reported the nix redis ready with the wrong password")
	}
	if err = probeRedis(ctx, address, password); err != nil {
		t.Fatalf("probeRedis with the configured password: %v", err)
	}
}
