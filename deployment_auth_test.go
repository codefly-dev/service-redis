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
