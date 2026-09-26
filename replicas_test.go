package main

// replicas_test.go — read replicas, short of a real server: the settings, how
// aliases are routed, the processes the docker runtime starts, the readiness
// protocol a replica is held to, and what a deployment renders. The real
// servers are exercised by TestRealRedisRuntimeReplicas* and the cache suites.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func TestReplicaCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings Settings
		want     int
		wantErr  string
	}{
		{name: "off", settings: Settings{}, want: 0},
		{name: "on defaults to one", settings: Settings{WithReadReplicas: true}, want: 1},
		{name: "on with a count", settings: Settings{WithReadReplicas: true, ReadReplicas: 3}, want: 3},
		{name: "count without the switch", settings: Settings{ReadReplicas: 2}, wantErr: "with-read-replicas is not set"},
		{name: "negative", settings: Settings{WithReadReplicas: true, ReadReplicas: -1}, wantErr: "at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.settings.replicaCount()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("replicaCount = %d, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("replicaCount = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestResolveTCPEndpointsBindsReplicasToTheReadEndpoint(t *testing.T) {
	ctx := context.Background()
	read, write, other := &basev0.Endpoint{Name: "read", Api: "tcp"}, &basev0.Endpoint{Name: "write", Api: "tcp"}, &basev0.Endpoint{Name: "analytics", Api: "tcp"}

	svc := NewService()
	if err := svc.resolveTCPEndpoints(ctx, []*basev0.Endpoint{other, read, write}); err != nil {
		t.Fatal(err)
	}
	if svc.TcpEndpoint != write || svc.ReadEndpoint != nil {
		t.Fatalf("without replicas: serving %v, read %v; want write and none", svc.TcpEndpoint, svc.ReadEndpoint)
	}

	svc.WithReadReplicas = true
	if err := svc.resolveTCPEndpoints(ctx, []*basev0.Endpoint{other, read, write}); err != nil {
		t.Fatal(err)
	}
	if svc.TcpEndpoint != write || svc.ReadEndpoint != read {
		t.Fatalf("with replicas: serving %v, read %v; want write and read", svc.TcpEndpoint, svc.ReadEndpoint)
	}

	// No endpoint named read: the first other TCP endpoint serves the replicas.
	if err := svc.resolveTCPEndpoints(ctx, []*basev0.Endpoint{other, write}); err != nil || svc.ReadEndpoint != other {
		t.Fatalf("replica endpoint = %v, %v; want analytics", svc.ReadEndpoint, err)
	}

	// Replicas with nothing to serve are refused rather than run unreachable.
	if err := svc.resolveTCPEndpoints(ctx, []*basev0.Endpoint{write}); err == nil {
		t.Fatal("with-read-replicas accepted on a service with a single TCP endpoint")
	}
}

func TestRedisRuntimeMappingsRouteAliasesToReplicas(t *testing.T) {
	read, write := proposedRedisMapping("read", 16001), proposedRedisMapping("write", 16002)
	analytics := proposedRedisMapping("analytics", 16003)
	accepted, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read, write, analytics}, write.Endpoint, read.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []*basev0.NetworkMapping{read, write, read} {
		for j, instance := range accepted[i].GetInstances() {
			if !proto.Equal(instance, want.Instances[j]) {
				t.Errorf("%s resolves to %s, want %s's %s", accepted[i].GetEndpoint().GetName(), instance.GetAddress(), want.GetEndpoint().GetName(), want.Instances[j].GetAddress())
			}
		}
	}
	if _, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{read, write}, write.Endpoint, write.Endpoint); err == nil {
		t.Fatal("replicas accepted on the primary's own endpoint")
	}
	if _, err := redisRuntimeMappings(context.Background(), []*basev0.NetworkMapping{write}, write.Endpoint, read.Endpoint); err == nil {
		t.Fatal("replicas accepted without a mapping for their endpoint")
	}
}

func TestRedisDockerCommandStartsReplicasBeforeThePrimary(t *testing.T) {
	if command := redisDockerCommand(false, 0); command != nil {
		t.Fatalf("no password and no replicas = %q; want the image's own command", command)
	}
	script := strings.Join(redisDockerCommand(true, 2), " ")
	for _, want := range []string{
		`redis-server --port 6380 --replicaof 127.0.0.1 6379 --dbfilename replica-1.rdb --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD" & `,
		`redis-server --port 6381 --replicaof 127.0.0.1 6379 --dbfilename replica-2.rdb --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD" & `,
		`exec redis-server --requirepass "$REDIS_PASSWORD"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("docker command %q does not contain %q", script, want)
		}
	}
	open := strings.Join(redisDockerCommand(false, 1), " ")
	if !strings.Contains(open, "--replicaof 127.0.0.1 6379") || strings.Contains(open, "REDIS_PASSWORD") {
		t.Fatalf("passwordless docker command = %q", open)
	}
}

func TestReserveLoopbackPortsAreDistinct(t *testing.T) {
	ports, err := reserveLoopbackPorts(8)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint16]bool{}
	for _, port := range ports {
		if port == 0 || seen[port] {
			t.Fatalf("ports = %v", ports)
		}
		seen[port] = true
	}
}

func replicationInfo(fields ...string) string {
	body := "# Replication\r\n" + strings.Join(fields, "\r\n")
	return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body)
}

func TestProbeRedisReplicaRequiresTheLinkUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		info      string
		wantOK    bool
		retryable bool
	}{
		{name: "link up", info: replicationInfo("role:slave", "master_host:127.0.0.1", "master_link_status:up"), wantOK: true},
		{name: "still syncing", info: replicationInfo("role:slave", "master_link_status:down", "master_sync_in_progress:1"), retryable: true},
		{name: "a primary", info: replicationInfo("role:master", "connected_slaves:0")},
		{name: "refused", info: "-NOPERM this user has no permissions to run the 'info' command\r\n"},
		{name: "not a bulk string", info: "+OK\r\n"},
		{name: "oversized", info: fmt.Sprintf("$%d\r\n", redisProbeMaxBulkBytes+1)},
		{name: "not terminated", info: "$4\r\nrole:\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serveAuthRedis(t, &authRedis{info: tc.info})
			err := probeRedisReplica(context.Background(), server.address(), "")
			if tc.wantOK {
				if err != nil {
					t.Fatalf("probeRedisReplica: %v", err)
				}
				return
			}
			var probeErr *redisProbeError
			if !errors.As(err, &probeErr) {
				t.Fatalf("probeRedisReplica = %v, want a *redisProbeError", err)
			}
			if probeErr.retryable != tc.retryable {
				t.Fatalf("retryable = %t, want %t (%v)", probeErr.retryable, tc.retryable, err)
			}
		})
	}
}

type renderedStatefulSet struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas int `yaml:"replicas"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []struct {
					Command []string `yaml:"command"`
					Ports   []struct {
						Name string `yaml:"name"`
					} `yaml:"ports"`
					ReadinessProbe struct {
						Exec struct {
							Command []string `yaml:"command"`
						} `yaml:"exec"`
					} `yaml:"readinessProbe"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type renderedReplicaService struct {
	Spec struct {
		Selector map[string]string `yaml:"selector"`
		Ports    []struct {
			Port       uint32 `yaml:"port"`
			TargetPort string `yaml:"targetPort"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

func parseStatefulSets(t *testing.T, destination string) []renderedStatefulSet {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(readDeploymentFile(t, destination, "base", "stateful-set.yaml")))
	var sets []renderedStatefulSet
	for {
		var set renderedStatefulSet
		if err := decoder.Decode(&set); err != nil {
			if err.Error() == "EOF" {
				return sets
			}
			t.Fatal(err)
		}
		sets = append(sets, set)
	}
}

// replicaDeployment deploys a read/write service with replicas and returns the
// response and where it rendered. restricted selects the restricted profile,
// with a password Secret reference.
func replicaDeployment(t *testing.T, replicas int, restricted bool) (*Builder, *builderv0.DeploymentResponse, string) {
	t.Helper()
	useSuccessfulKubectl(t)
	builder, _ := newDeploymentTestBuilder(t)
	read := deploymentAliasMapping(builder, "read", 16001)
	write := deploymentAliasMapping(builder, "write", 16002)
	builder.WithReadReplicas, builder.ReadReplicas = true, replicas
	if err := builder.resolveTCPEndpoints(context.Background(), []*basev0.Endpoint{read.Endpoint, write.Endpoint}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	request := restrictedDeploymentRequest(destination, []*basev0.NetworkMapping{read, write}, nil, false)
	if restricted {
		builder.RequirePass = true
		key := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "redis", "REDIS_PASSWORD")
		request.GetDeployment().GetKubernetes().SecretReferences = map[string]*builderv0.KubernetesSecretKeyReference{
			key: {Name: "redis-credentials", Key: "password"},
		}
	} else {
		request.GetDeployment().GetKubernetes().Profile = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
		request.Configuration = &basev0.Configuration{Infos: []*basev0.ConfigurationInformation{{
			Name:                "redis",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "REDIS_PASSWORD", Value: "deploy-secret", Secret: true}},
		}}}
	}
	response, err := builder.Deploy(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}
	return builder, response, destination
}

func TestDeploymentRendersPrimaryAndReplicas(t *testing.T) {
	_, response, destination := replicaDeployment(t, 3, true)
	if v := response.GetDeployment().GetKubernetes().GetValidation(); v.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("restricted output with replicas fails static validation: %v", v.GetViolations())
	}

	sets := parseStatefulSets(t, destination)
	if len(sets) != 2 {
		t.Fatalf("rendered %d StatefulSets, want the primary and the replicas", len(sets))
	}
	primary, replicas := sets[0], sets[1]
	if primary.Metadata.Name != "redis" || primary.Spec.Replicas != 1 {
		t.Errorf("primary = %s × %d, want redis × 1", primary.Metadata.Name, primary.Spec.Replicas)
	}
	if replicas.Metadata.Name != "redis-replica" || replicas.Spec.Replicas != 3 {
		t.Errorf("replicas = %s × %d, want redis-replica × 3", replicas.Metadata.Name, replicas.Spec.Replicas)
	}
	if got := primary.Spec.Template.Spec.Containers[0].Ports[0].Name; got != "primary" {
		t.Errorf("primary container port is named %q, want primary", got)
	}
	replica := replicas.Spec.Template.Spec.Containers[0]
	if got := replica.Ports[0].Name; got != "replica" {
		t.Errorf("replica container port is named %q, want replica", got)
	}
	// Replicas reach the primary through its endpoint as core resolved it, and
	// authenticate to it with the same Secret.
	command := strings.Join(replica.Command, " ")
	for _, want := range []string{
		`--replicaof redis.codefly-test.svc.cluster.local 16002`,
		`--masterauth "$REDIS_PASSWORD"`,
		`--requirepass "$REDIS_PASSWORD"`,
	} {
		if !strings.Contains(command, want) {
			t.Errorf("replica command %q does not contain %q", command, want)
		}
	}
	if probe := strings.Join(replica.ReadinessProbe.Exec.Command, " "); !strings.Contains(probe, "master_link_status:up") {
		t.Errorf("replica readiness = %q, want it to require the link to the primary", probe)
	}
	for _, set := range sets {
		if set.Spec.Template.Metadata.Labels["redis.codefly.dev/set"] != "redis" {
			t.Errorf("%s pods carry no set label for the Service to select", set.Metadata.Name)
		}
	}

	var service renderedReplicaService
	if err := yaml.Unmarshal([]byte(readDeploymentFile(t, destination, "base", "service.yaml")), &service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.Selector["redis.codefly.dev/set"] != "redis" || len(service.Spec.Selector) != 1 {
		t.Errorf("Service selector = %v, want the whole set", service.Spec.Selector)
	}
	targets := map[uint32]string{}
	for _, port := range service.Spec.Ports {
		targets[port.Port] = port.TargetPort
	}
	if targets[16002] != "primary" || targets[16001] != "replica" {
		t.Errorf("Service targets = %v, want write (16002) → primary and read (16001) → replica", targets)
	}

	// The restricted output names both connections, and carries neither value.
	values := map[string]*basev0.ConfigurationValue{}
	for _, v := range response.GetConfiguration().GetInfos()[0].GetConfigurationValues() {
		values[v.GetKey()] = v
	}
	for _, key := range []string{"connection", readConnectionKey} {
		if v := values[key]; v == nil || !v.GetSecret() || v.GetValue() != "" {
			t.Errorf("restricted redis.%s = %+v, want a value-free secret reference", key, v)
		}
	}
}

func TestDeploymentReplicasHandConsumersTheReadConnection(t *testing.T) {
	_, response, destination := replicaDeployment(t, 1, false)
	conf := emitted{response.GetConfiguration()}
	write, err := conf.Secret("redis", "connection")
	if err != nil {
		t.Fatal(err)
	}
	read, err := conf.Secret("redis", readConnectionKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(write, "@redis.codefly-test.svc.cluster.local:16002") || !strings.HasSuffix(read, "@redis.codefly-test.svc.cluster.local:16001") {
		t.Fatalf("connection = %q, read-connection = %q; want the write and read endpoints", write, read)
	}
	cache, err := conf.Secret("cache", "connection")
	if err != nil || cache != write {
		t.Fatalf("cache.connection = %q, %v; want the primary's %q", cache, err, write)
	}
	replica := parseStatefulSets(t, destination)[1].Spec.Template.Spec.Containers[0]
	if command := strings.Join(replica.Command, " "); !strings.Contains(command, "--replicaof redis.codefly-test.svc.cluster.local 16002") {
		t.Fatalf("replica command = %q", command)
	}
}

// A Service port routes to one set of pods, so read and write cannot share one.
func TestDeploymentReplicasNeedTheirOwnPort(t *testing.T) {
	useSuccessfulKubectl(t)
	builder, _ := newDeploymentTestBuilder(t)
	read := deploymentAliasMapping(builder, "read", 6379)
	write := deploymentAliasMapping(builder, "write", 6379)
	builder.WithReadReplicas = true
	if err := builder.resolveTCPEndpoints(context.Background(), []*basev0.Endpoint{read.Endpoint, write.Endpoint}); err != nil {
		t.Fatal(err)
	}
	response, err := builder.Deploy(context.Background(), restrictedDeploymentRequest(t.TempDir(), []*basev0.NetworkMapping{read, write}, nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_ERROR || !strings.Contains(response.GetState().GetMessage(), "each needs its own port") {
		t.Fatalf("deployment = %s (%s); want the shared port refused", response.GetState().GetState(), response.GetState().GetMessage())
	}
}
