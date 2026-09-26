package main

// deployment_auth_test.go — the ephemeral (local-apply) deployment must start
// redis with the password it hands consumers.
//
// It did not: the non-restricted StatefulSet had no command, so the image ran a
// bare redis-server, and the password was never in the Secret. The server was
// open to any client and refused the AUTH every consumer's connection sent.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	goredis "github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

type renderedRedisContainer struct {
	Command []string
	Secret  map[string]string // decoded
}

// renderEphemeral deploys with the ephemeral local-apply profile and returns the
// redis container's command, the decoded Secret and the emitted connection.
func renderEphemeral(t *testing.T, password string) (renderedRedisContainer, string) {
	t.Helper()
	useSuccessfulKubectl(t)
	builder, networkMappings := newDeploymentTestBuilder(t)
	builder.Password = password
	builder.RequirePass = password != ""
	destination := t.TempDir()
	request := restrictedDeploymentRequest(destination, networkMappings, nil, false)
	request.Deployment.GetKubernetes().Profile = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	response, err := builder.Deploy(context.Background(), request)
	if err != nil || response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("Deploy: %v, %v", response.GetState(), err)
	}

	var statefulSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name    string   `yaml:"name"`
						Command []string `yaml:"command"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readDeploymentFile(t, destination, "base", "stateful-set.yaml")), &statefulSet); err != nil {
		t.Fatal(err)
	}
	var rendered renderedRedisContainer
	for _, c := range statefulSet.Spec.Template.Spec.Containers {
		if c.Name == "redis" {
			rendered.Command = c.Command
		}
	}

	var secret struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(readDeploymentFile(t, destination, "overlays", "test", "secret.yaml")), &secret); err != nil {
		t.Fatal(err)
	}
	rendered.Secret = map[string]string{}
	for k, v := range secret.Data {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatalf("secret %s is not base64: %v", k, err)
		}
		rendered.Secret[k] = string(decoded)
	}

	connection := ""
	for _, info := range response.GetConfiguration().GetInfos() {
		for _, v := range info.GetConfigurationValues() {
			if info.GetName() == "redis" && v.GetKey() == "connection" {
				connection = v.GetValue()
			}
		}
	}
	return rendered, connection
}

func TestEphemeralDeploymentStartsRedisWithItsPassword(t *testing.T) {
	const password = "ephemeral-auth-7Qm2"
	rendered, connection := renderEphemeral(t, password)

	if got := rendered.Secret["REDIS_PASSWORD"]; got != password {
		t.Fatalf("Secret REDIS_PASSWORD = %q, want the configured password", got)
	}
	if got := rendered.Secret["REDISCLI_AUTH"]; got != password {
		t.Fatalf("Secret REDISCLI_AUTH = %q, want the configured password (probes authenticate with it)", got)
	}
	if !strings.Contains(strings.Join(rendered.Command, " "), `--requirepass "$REDIS_PASSWORD"`) {
		t.Fatalf("redis container command %q does not start the server with the password", rendered.Command)
	}
	if strings.Contains(strings.Join(rendered.Command, " "), password) {
		t.Fatal("the password leaked into the container command")
	}
	if !strings.Contains(connection, password) {
		t.Fatalf("emitted connection %q does not carry the password the server now requires", connection)
	}
}

func TestEphemeralDeploymentWithoutPasswordStaysOpenByConfiguration(t *testing.T) {
	rendered, _ := renderEphemeral(t, "")
	if _, ok := rendered.Secret["REDIS_PASSWORD"]; ok {
		t.Fatal("Secret carries REDIS_PASSWORD although no password is configured")
	}
	if len(rendered.Command) == 0 {
		t.Fatal("redis container has no command")
	}
}

// The rendered container, run for real: the pinned image with exactly the
// rendered command and the rendered Secret as its environment.
func TestRealRedisEphemeralDeploymentAuthenticates(t *testing.T) {
	requireDocker(t)
	const password = "ephemeral-real-9Xk4"
	rendered, connection := renderEphemeral(t, password)
	if len(rendered.Command) == 0 {
		t.Fatal("redis container has no command to run")
	}

	name := fmt.Sprintf("service-redis-ephemeral-%d", time.Now().UnixNano())
	args := []string{"run", "--detach", "--rm", "--name", name, "--publish", "127.0.0.1::6379"}
	for k, v := range rendered.Secret {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, "--entrypoint", rendered.Command[0], image.FullName())
	args = append(args, rendered.Command[1:]...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", name).Run() })
	published, err := exec.CommandContext(ctx, "docker", "port", name, "6379/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	address := strings.TrimSpace(strings.SplitN(string(published), "\n", 2)[0])
	if err := waitForRedisPong(ctx, redisWaitOptions{address: address, password: password, budget: redisDockerReadinessBudget}); err != nil {
		t.Fatalf("server never accepted the configured password: %v", err)
	}

	// Anyone without the password is refused.
	anonymous := goredis.NewClient(&goredis.Options{Addr: address})
	defer anonymous.Close()
	if err := anonymous.Set(ctx, "k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("unauthenticated SET = %v, want NOAUTH", err)
	}

	// The consumer's emitted connection works against it.
	options, err := goredis.ParseURL(strings.Replace(connection, "redis.example.com:6379", address, 1))
	if err != nil {
		t.Fatal(err)
	}
	consumer := goredis.NewClient(options)
	defer consumer.Close()
	if err := consumer.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("consumer using the emitted connection: %v", err)
	}

	// The probes' redis-cli ping authenticates through REDISCLI_AUTH.
	out, err := exec.CommandContext(ctx, "docker", "exec", name, "redis-cli", "ping").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "PONG" {
		t.Fatalf("probe redis-cli ping = %q, %v; want PONG", out, err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("timed out")
	}
}

// renderEphemeralReplicas deploys a read/write service with one replica under
// the ephemeral profile. The write endpoint is published on 6379, so the
// address a replica is rendered to replicate is where the primary container
// itself listens.
func renderEphemeralReplicas(t *testing.T, password string) (primary, replica renderedStatefulSet, secret map[string]string) {
	t.Helper()
	useSuccessfulKubectl(t)
	builder, _ := newDeploymentTestBuilder(t)
	builder.Password, builder.RequirePass = password, password != ""
	builder.WithReadReplicas = true
	read := deploymentAliasMapping(builder, "read", 16001)
	write := deploymentAliasMapping(builder, "write", 6379)
	if err := builder.resolveTCPEndpoints(context.Background(), []*basev0.Endpoint{read.Endpoint, write.Endpoint}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	request := restrictedDeploymentRequest(destination, []*basev0.NetworkMapping{read, write}, nil, false)
	request.Deployment.GetKubernetes().Profile = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	response, err := builder.Deploy(context.Background(), request)
	if err != nil || response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("Deploy: %v, %v", response.GetState(), err)
	}
	sets := parseStatefulSets(t, destination)
	if len(sets) != 2 {
		t.Fatalf("rendered %d StatefulSets, want the primary and the replicas", len(sets))
	}
	var rendered struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(readDeploymentFile(t, destination, "overlays", "test", "secret.yaml")), &rendered); err != nil {
		t.Fatal(err)
	}
	secret = map[string]string{}
	for k, v := range rendered.Data {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatalf("secret %s is not base64: %v", k, err)
		}
		secret[k] = string(decoded)
	}
	return sets[0], sets[1], secret
}

func TestEphemeralReplicasUseTheSecret(t *testing.T) {
	const password = "ephemeral-replica-3Rk8"
	_, replica, secret := renderEphemeralReplicas(t, password)
	container := replica.Spec.Template.Spec.Containers[0]
	command := strings.Join(container.Command, " ")
	for _, want := range []string{`--requirepass "$REDIS_PASSWORD"`, `--masterauth "$REDIS_PASSWORD"`, `--replicaof redis.codefly-test.svc.cluster.local 6379`} {
		if !strings.Contains(command, want) {
			t.Errorf("replica command %q does not contain %q", command, want)
		}
	}
	if strings.Contains(command, password) {
		t.Fatal("the password leaked into the replica command")
	}
	if secret["REDIS_PASSWORD"] != password || secret["REDISCLI_AUTH"] != password {
		t.Fatalf("Secret = %v, want REDIS_PASSWORD and REDISCLI_AUTH set to the password", secret)
	}
}

// The rendered primary and replica, run for real: the pinned image with
// exactly the rendered commands and the rendered Secret as their environment,
// on a network where the primary answers at the host the replica was rendered
// to replicate, as the Service makes it answer in the cluster.
func TestRealRedisEphemeralReplicasAuthenticate(t *testing.T) {
	requireDocker(t)
	const password = "ephemeral-replica-real-6Tq1"
	primarySet, replicaSet, secret := renderEphemeralReplicas(t, password)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	network := fmt.Sprintf("service-redis-replicas-%d", time.Now().UnixNano())
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", network).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	run := func(set renderedStatefulSet, extra ...string) string {
		container := set.Spec.Template.Spec.Containers[0]
		name := fmt.Sprintf("service-redis-%s-%d", set.Metadata.Name, time.Now().UnixNano())
		args := append([]string{"run", "--detach", "--rm", "--name", name, "--network", network}, extra...)
		for k, v := range secret {
			args = append(args, "--env", k+"="+v)
		}
		args = append(args, "--entrypoint", container.Command[0], image.FullName())
		args = append(args, container.Command[1:]...)
		if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker run %s: %v\n%s", set.Metadata.Name, err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", name).Run() })
		return name
	}
	primary := run(primarySet, "--network-alias", "redis.codefly-test.svc.cluster.local", "--publish", "127.0.0.1::6379")
	replica := run(replicaSet, "--publish", "127.0.0.1::6379")
	address := func(name string) string {
		published, err := exec.CommandContext(ctx, "docker", "port", name, "6379/tcp").Output()
		if err != nil {
			t.Fatalf("docker port: %v", err)
		}
		return strings.TrimSpace(strings.SplitN(string(published), "\n", 2)[0])
	}
	primaryAddress, replicaAddress := address(primary), address(replica)

	// The replica is ready only by the rendered probe, run as rendered: it
	// authenticates through REDISCLI_AUTH and requires the link to be up.
	probe := replicaSet.Spec.Template.Spec.Containers[0].ReadinessProbe.Exec.Command
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(500 * time.Millisecond) {
		if exec.CommandContext(ctx, "docker", append([]string{"exec", replica}, probe...)...).Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", replica).CombinedOutput()
			t.Fatalf("the rendered replica never passed its rendered readiness probe %q\n%s", probe, out)
		}
	}
	if err := waitForRedisPong(ctx, redisWaitOptions{address: replicaAddress, password: password, replica: true, budget: redisDockerReadinessBudget}); err != nil {
		t.Fatalf("replica not linked with the configured password: %v", err)
	}

	anonymous := goredis.NewClient(&goredis.Options{Addr: replicaAddress})
	defer anonymous.Close()
	if err := anonymous.Get(ctx, "k").Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("unauthenticated GET on the replica = %v, want NOAUTH", err)
	}
	onReplica := goredis.NewClient(&goredis.Options{Addr: replicaAddress, Password: password})
	defer onReplica.Close()
	if err := onReplica.Set(ctx, "k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Fatalf("SET on the replica = %v, want READONLY", err)
	}
	onPrimary := goredis.NewClient(&goredis.Options{Addr: primaryAddress, Password: password})
	defer onPrimary.Close()
	if err := onPrimary.Set(ctx, "k", "from-the-primary", 0).Err(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if v, _ := onReplica.Get(ctx, "k").Result(); v == "from-the-primary" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the primary's write never reached the replica")
		}
	}
}
