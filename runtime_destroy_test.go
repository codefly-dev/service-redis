package main

import (
	"context"
	"errors"
	"testing"
	"time"

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

type blockingDockerInitialization struct {
	entered  chan struct{}
	finish   chan struct{}
	shutdown chan struct{}
	initErr  error
}

func (d *blockingDockerInitialization) Init(context.Context) error {
	close(d.entered)
	<-d.finish
	return d.initErr
}
func (d *blockingDockerInitialization) Shutdown(context.Context) error {
	close(d.shutdown)
	return nil
}

func TestDockerDestroyWaitsForAcquisitionAndInitialization(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "failed-init"}[fail], func(t *testing.T) {
			rt := NewRuntime()
			acquired := &blockingDockerInitialization{entered: make(chan struct{}), finish: make(chan struct{}), shutdown: make(chan struct{})}
			if fail {
				acquired.initErr = errors.New("startup failed after acquisition")
			}
			initialized := make(chan error, 1)
			go func() { initialized <- rt.initializeDockerRuntime(context.Background(), acquired) }()
			<-acquired.entered
			destroyed := make(chan error, 1)
			go func() {
				response, err := rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
				if err == nil && response.GetStatus().GetState() != runtimev0.DestroyStatus_SUCCESS {
					err = errors.New("Destroy failed")
				}
				destroyed <- err
			}()
			select {
			case <-acquired.shutdown:
				close(acquired.finish)
				t.Fatal("Destroy reached a half-initialized acquired handle")
			case <-time.After(50 * time.Millisecond):
			}
			close(acquired.finish)
			if err := <-initialized; !errors.Is(err, acquired.initErr) {
				t.Fatal(err)
			}
			if err := <-destroyed; err != nil {
				t.Fatal(err)
			}
			select {
			case <-acquired.shutdown:
			default:
				t.Fatal("acquired handle was not destroyed")
			}
			if rt.runnerEnvironment != acquired {
				t.Fatal("initialization lost its acquired handle")
			}
		})
	}
}
