package main

// cache_targets_test.go — codefly.dev/cache against servers this agent does not
// run, but whose support the driver claims: Valkey, and Redis Cluster.
//
// The agent's Runtime only ever starts redis, single-process or with replicas,
// so these servers cannot come from it. They are started here, pinned by
// digest, and gated like every other real-server test: skipped without docker,
// failed without it under REDIS_INFRA_TESTS=required.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
	"github.com/codefly-dev/interface-cache/go/cache/cachetest"
	rediscache "github.com/codefly-dev/service-redis/cache"
	goredis "github.com/redis/go-redis/v9"
)

// valkeyImage is the Valkey the driver is checked against.
const valkeyImage = "docker.io/valkey/valkey:9.1.2-alpine@sha256:48332870af354a799964c0012ae1194a0bf2bf894eb508f945810596dc2d8d11"

// dockerRun starts a detached container removed at cleanup, and returns its name.
func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	name := fmt.Sprintf("service-redis-target-%d-%d", os.Getpid(), time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := append([]string{"run", "--detach", "--rm", "--name", name}, args...)
	if out, err := exec.CommandContext(ctx, "docker", run...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), time.Minute)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, "docker", "rm", "--force", name).Run()
	})
	return name
}

func TestRealRedisCacheOverValkey(t *testing.T) {
	requireDocker(t)
	const password = "cache-valkey-2HmXq6"
	ports, err := reserveLoopbackPorts(1)
	if err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
	dockerRun(t, "--publish", address+":6379", "--env", "VALKEY_PASSWORD="+password,
		valkeyImage, "sh", "-c", `exec valkey-server --requirepass "$VALKEY_PASSWORD"`)
	if err := waitForRedisPong(context.Background(), redisWaitOptions{address: address, password: password, budget: redisDockerReadinessBudget}); err != nil {
		t.Fatalf("valkey never became ready: %v", err)
	}
	// Valkey is reached exactly as a consumer of this agent reaches redis: the
	// connection string the agent would emit, opened through the interface.
	conf := emitted{connectionConfiguration(t, address, password)}
	server := redisClient(t, mustSecret(t, conf, cacheiface.Group, cacheiface.KeyConnection))
	if info, err := server.Info(context.Background(), "server").Result(); err != nil || !strings.Contains(info, "server_name:valkey") {
		t.Fatalf("the server under test is not Valkey: %v", err)
	}
	runCacheInterface(t, conf, password)
}

func mustSecret(t *testing.T, conf emitted, group, key string) string {
	t.Helper()
	value, err := conf.Secret(group, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// clusterNodes is the size of the local cluster: the smallest one with every
// slot covered and keys spread over more than one primary.
const clusterNodes = 3

// TestRealRedisCacheOverCluster runs the suite against a Redis Cluster of the
// pinned runtime image. Each node listens inside the container on the same
// port it is published on and announces 127.0.0.1, so the MOVED redirects the
// cluster answers with name addresses this host can reach. Each node's cluster
// bus gets a port of its own: by default it is the node's port plus 10000,
// which does not exist for a port above 55535, and loopback ports handed out
// by the OS routinely are.
func TestRealRedisCacheOverCluster(t *testing.T) {
	requireDocker(t)
	const password = "cache-cluster-4NdLp1"
	ports, err := reserveLoopbackPorts(2 * clusterNodes)
	if err != nil {
		t.Fatal(err)
	}
	ports, busPorts := ports[:clusterNodes], ports[clusterNodes:]
	var script strings.Builder
	var addresses, publish []string
	for i, port := range ports {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
		addresses = append(addresses, address)
		publish = append(publish, "--publish", address+":"+strconv.Itoa(int(port)))
		fmt.Fprintf(&script, `redis-server --port %d --cluster-port %d --cluster-enabled yes --cluster-config-file node-%d.conf --cluster-announce-ip 127.0.0.1 --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD" --dbfilename node-%d.rdb & `, port, busPorts[i], i, i)
	}
	script.WriteString("wait")
	args := append(publish, "--env", "REDIS_PASSWORD="+password, image.FullName(), "sh", "-c", script.String())
	name := dockerRun(t, args...)
	for _, address := range addresses {
		if err := waitForRedisPong(context.Background(), redisWaitOptions{address: address, password: password, budget: redisDockerReadinessBudget}); err != nil {
			t.Fatalf("cluster node %s never became ready: %v", address, err)
		}
	}
	create := append([]string{"exec", "--env", "REDISCLI_AUTH=" + password, name, "redis-cli", "--cluster", "create"}, addresses...)
	create = append(create, "--cluster-yes")
	if out, err := exec.Command("docker", create...).CombinedOutput(); err != nil {
		t.Fatalf("redis-cli --cluster create: %v\n%s", err, out)
	}

	newClient := func(t *testing.T) *goredis.ClusterClient {
		client := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: addresses, Password: password})
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	// Creating a cluster returns before every node agrees on the slot map.
	ctx := context.Background()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		info, err := newClient(t).ClusterInfo(ctx).Result()
		if err == nil && strings.Contains(info, "cluster_state:ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster never reached cluster_state:ok: %q, %v", info, err)
		}
	}
	client := newClient(t)
	shards, err := client.ClusterShards(ctx).Result()
	if err != nil || len(shards) != clusterNodes {
		t.Fatalf("cluster has %d shards (%v), want %d primaries", len(shards), err, clusterNodes)
	}

	// Every key the driver writes for one cache key must share a slot, or its
	// two-key scripts fail with CROSSSLOT; spread across nodes, the suite
	// below would hit that on the first Set.
	runCacheSuite(t, cachetest.Harness{New: func(t *testing.T, namespace string) cacheiface.Layer {
		return rediscache.New(newClient(t), rediscache.WithPrefix(namespace))
	}})

	// And the keys do spread: a cluster holding everything on one node would
	// pass the suite without proving anything about slots.
	t.Run("KeysSpreadAcrossPrimaries", func(t *testing.T) {
		layer := rediscache.New(client, rediscache.WithPrefix(cachetest.Namespace(t)))
		for i := range 64 {
			mustCache(t, layer.Set(ctx, fmt.Sprint("spread-", i), cacheiface.Entry{Value: []byte("v")}, time.Minute))
		}
		holding := 0
		mustCache(t, client.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error {
			n, err := node.DBSize(ctx).Result()
			if n > 0 {
				holding++
			}
			return err
		}))
		if holding < 2 {
			t.Fatalf("keys landed on %d of %d primaries", holding, clusterNodes)
		}
	})
}
