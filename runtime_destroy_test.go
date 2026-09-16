package main

import (
	"context"
	"errors"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

type failingDockerShutdown struct{ calls int }

func (*failingDockerShutdown) Init(context.Context) error { return nil }
func (d *failingDockerShutdown) Shutdown(context.Context) error {
	d.calls++
	if d.calls == 1 {
		return errors.New("daemon removal failed")
	}
	return nil
}

func TestDockerDestroyRetainsAcquiredHandleOnFailure(t *testing.T) {
	rt := NewRuntime()
	acquired := &failingDockerShutdown{}
	rt.runnerEnvironment = acquired
	failed, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	if err == nil && failed.GetStatus().GetState() == runtimev0.DestroyStatus_SUCCESS {
		t.Fatal("failed removal reported success")
	}
	if rt.runnerEnvironment != acquired || acquired.calls != 1 {
		t.Fatal("failed removal lost the acquired generation")
	}
	retried, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	if err != nil || retried.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS || acquired.calls != 2 {
		t.Fatalf("retry did not shut down the same acquired generation: %v, %v, calls=%d", retried, err, acquired.calls)
	}
}

func TestDockerDestroyWithoutAcquiredHandleIsNoOp(t *testing.T) {
	rt := NewRuntime()
	response, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	if err != nil || response.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
		t.Fatalf("uninitialized Destroy: %v, %v", response, err)
	}
}
