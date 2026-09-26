package main

// runtime_replicas_test.go — read replicas as the Runtime runs them, over both
// backends: the write endpoint is the primary, the read endpoint a replica,
// every replica is linked to the primary, and a replica refuses writes.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	cacheiface "github.com/codefly-dev/interface-cache/go/cache"
)

func TestRealRedisRuntimeReplicasOverDocker(t *testing.T) {
	testRuntimeReplicas(t, dockerBackend, 2, "replicas-docker-5TqWn8")
}

func TestRealRedisRuntimeReplicasOverNix(t *testing.T) {
	testRuntimeReplicas(t, nixBackend, 2, "replicas-nix-8KcVr2")
}

// Without a password the docker servers are started by a shell rather than the
// image's own command; a replica must still take clients through its published
// port and replicate an unauthenticated primary.
func TestRealRedisRuntimeReplicasWithoutPasswordOverDocker(t *testing.T) {
	testRuntimeReplicas(t, dockerBackend, 1, "")
}

func testRuntimeReplicas(t *testing.T, backend redisBackend, replicas int, password string) {
	r := startRuntimeRedis(t, backend, replicas, password)
	ctx := context.Background()
	conf := r.consumer(t)

	primaryConnection, err := conf.Secret("redis", "connection")
	if err != nil {
		t.Fatal(err)
	}
	readConnection, err := conf.Secret("redis", readConnectionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := connectionAddress(t, primaryConnection), r.address(t, r.write); got != want {
		t.Errorf("redis.connection points at %s, want the write endpoint %s", got, want)
	}
	if got, want := connectionAddress(t, readConnection), r.address(t, r.read); got != want {
		t.Errorf("redis.read-connection points at %s, want the read endpoint %s", got, want)
	}
	if cache, err := conf.Secret(cacheiface.Group, cacheiface.KeyConnection); err != nil || cache != primaryConnection {
		t.Errorf("cache.connection = %q, %v; every cache operation belongs on the primary %q", cache, err, primaryConnection)
	}

	primary := redisClient(t, primaryConnection)
	info := replicationInfoOf(t, primary)
	if info["role"] != "master" || info["connected_slaves"] != strconv.Itoa(replicas) {
		t.Fatalf("write endpoint: role %q with %q replicas, want master with %d", info["role"], info["connected_slaves"], replicas)
	}

	// Every replica, including those no endpoint routes to, is linked and read-only.
	if len(r.rt.replicaAddresses) != replicas || r.rt.replicaAddresses[0] != r.address(t, r.read) {
		t.Fatalf("replicas at %v, want %d with the first behind the read endpoint %s", r.rt.replicaAddresses, replicas, r.address(t, r.read))
	}
	for i, address := range r.rt.replicaAddresses {
		replica := redisClient(t, strings.Replace(readConnection, connectionAddress(t, readConnection), address, 1))
		info := replicationInfoOf(t, replica)
		if info["role"] != "slave" || info["master_link_status"] != "up" {
			t.Errorf("replica %d at %s: role %q, link %q; want a replica linked to the primary", i+1, address, info["role"], info["master_link_status"])
		}
		if err := replica.Set(ctx, "written-to-a-replica", "x", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
			t.Errorf("SET on replica %d = %v, want READONLY", i+1, err)
		}
	}

	// A write to the primary reaches every replica.
	if err := primary.Set(ctx, "replicated", "from-the-primary", 0).Err(); err != nil {
		t.Fatal(err)
	}
	for i, address := range r.rt.replicaAddresses {
		replica := redisClient(t, strings.Replace(readConnection, connectionAddress(t, readConnection), address, 1))
		deadline := time.Now().Add(5 * time.Second)
		for {
			value, err := replica.Get(ctx, "replicated").Result()
			if err == nil && value == "from-the-primary" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("replica %d never received the primary's write: %q, %v", i+1, value, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Destroy releases every process of the set, not just the primary.
	addresses := append([]string{r.address(t, r.write)}, r.rt.replicaAddresses...)
	destroyed, err := r.rt.Destroy(ctx, &runtimev0.DestroyRequest{})
	if err != nil || destroyed.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
		t.Fatalf("Destroy: %v, %v", destroyed, err)
	}
	for _, address := range addresses {
		requireRefused(t, address)
	}
}
