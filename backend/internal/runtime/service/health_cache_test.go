package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingHealthController struct {
	calls atomic.Int32
}

func (controller *countingHealthController) ReloadOrRestart(context.Context, ServiceName, []RenderedArtifact) error {
	return nil
}

func (controller *countingHealthController) Status(context.Context, ServiceName) (Health, error) {
	controller.calls.Add(1)
	time.Sleep(10 * time.Millisecond)
	return Health{Service: SmartDNS, Available: true}, nil
}

func (controller *countingHealthController) Rollback(context.Context, ServiceName, []RenderedArtifact) error {
	return nil
}

func TestRuntimeHealthCheckCoalescesConcurrentProbesAndCaches(t *testing.T) {
	controller := &countingHealthController{}
	runtime := &Runtime{Controller: controller}

	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			health, err := runtime.HealthCheck(context.Background(), SmartDNS)
			if err != nil || len(health) != 1 || !health[0].Available {
				t.Errorf("health = %#v, err = %v", health, err)
			}
		}()
	}
	wait.Wait()
	if got := controller.calls.Load(); got != 1 {
		t.Fatalf("probe calls = %d, want 1 coalesced call", got)
	}

	if _, err := runtime.HealthCheck(context.Background(), SmartDNS); err != nil {
		t.Fatal(err)
	}
	if got := controller.calls.Load(); got != 1 {
		t.Fatalf("probe calls after cache hit = %d, want 1", got)
	}

	runtime.invalidateHealth()
	if _, err := runtime.HealthCheck(context.Background(), SmartDNS); err != nil {
		t.Fatal(err)
	}
	if got := controller.calls.Load(); got != 2 {
		t.Fatalf("probe calls after invalidation = %d, want 2", got)
	}
}
