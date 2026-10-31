package orchestratorapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

// reconcileTestRuntime records every data-plane mutation and lets a test make
// the reconcile readback fail on demand.
type reconcileTestRuntime struct {
	mu           sync.Mutex
	applications []transparentApplication
	reconciled   []transparentApplication
	applyErr     error
	reconcileErr error
	applyEntered chan string
	applyRelease chan struct{}
}

func (runtime *reconcileTestRuntime) ApplyTransparent(_ context.Context, requestID string, topology orchestrator.Topology, _ *orchestrator.Policy) error {
	if runtime.applyEntered != nil {
		runtime.applyEntered <- requestID
		<-runtime.applyRelease
	}
	if runtime.applyErr != nil {
		return runtime.applyErr
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.applications = append(runtime.applications, transparentApplication{topology: topology.View()})
	return nil
}

func (runtime *reconcileTestRuntime) ReconcileTransparent(_ context.Context, _ string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	if runtime.reconcileErr != nil {
		return runtime.reconcileErr
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	application := transparentApplication{topology: topology.View()}
	if policy != nil {
		view := policy.View()
		application.policy = &view
	}
	runtime.reconciled = append(runtime.reconciled, application)
	return nil
}

func (runtime *reconcileTestRuntime) DisableTransparent(context.Context, string) error   { return nil }
func (runtime *reconcileTestRuntime) CommitTransparentTransaction(context.Context) error { return nil }

func (runtime *reconcileTestRuntime) ApplyServiceChain(context.Context, string, orchestrator.ServiceChain, []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	return vpp.ServiceChainApplyResult{}, nil
}

func (runtime *reconcileTestRuntime) reconciledCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.reconciled)
}

func TestTransparentMutationReconcilesPersistedPolicyAfterCommit(t *testing.T) {
	runtime := &reconcileTestRuntime{}
	_, server := newTestHTTPServer(t, runtime)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	policy := []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[{"id":"g-default","position":10,"rules":[{"id":"all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"direct"}}]}],"default":{"kind":"drop"}}`)
	response := sendRequest(t, server, http.MethodPut, PolicyPath, policy)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("policy: %d %s", response.StatusCode, response.Body)
	}
	if runtime.reconciledCount() == 0 {
		t.Fatal("policy write did not run a post-persist reconcile")
	}
	reconciled := runtime.reconciled[len(runtime.reconciled)-1]
	if reconciled.policy == nil || reconciled.policy.Default.Kind != orchestrator.ActionDrop {
		t.Fatalf("reconcile did not run against the persisted drop policy: %#v", reconciled.policy)
	}
}

func TestTransparentMutationFailsWhenDataPlaneDoesNotConverge(t *testing.T) {
	runtime := &reconcileTestRuntime{}
	repository, server := newTestHTTPServer(t, runtime)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	runtime.reconcileErr = errors.New("configured generation abc default direct, VPP state running generation def default drop")
	policy := []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[],"default":{"kind":"direct"}}`)
	response := sendRequest(t, server, http.MethodPut, PolicyPath, policy)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("policy status = %d, want 500 when the readback disagrees: %s", response.StatusCode, response.Body)
	}
	// The record is durable: the periodic reconciler owns convergence.
	if _, _, err := repository.PolicySnapshot(context.Background()); err != nil {
		t.Fatalf("policy not persisted after readback failure: %v", err)
	}
}

func TestTransparentTopologyAndPolicyWritesAreSerialised(t *testing.T) {
	runtime := &reconcileTestRuntime{applyEntered: make(chan string, 4), applyRelease: make(chan struct{})}
	_, server := newTestHTTPServer(t, runtime)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sendRequest(t, server, http.MethodPut, TopologyPath, topology)
	}()
	select {
	case <-runtime.applyEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("topology apply never started")
	}
	policy := []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[],"default":{"kind":"direct"}}`)
	policyDone := make(chan struct{})
	go func() {
		defer close(policyDone)
		sendRequest(t, server, http.MethodPut, PolicyPath, policy)
	}()
	select {
	case requestID := <-runtime.applyEntered:
		t.Fatalf("policy apply %s started while the topology mutation was still in flight", requestID)
	case <-time.After(300 * time.Millisecond):
	}
	close(runtime.applyRelease)
	for name, channel := range map[string]chan struct{}{"topology": done, "policy": policyDone} {
		select {
		case <-channel:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s mutation did not finish", name)
		}
	}
}
