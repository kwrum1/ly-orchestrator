package orchestratorapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

// serialRuntime records whether two transparent applies ever overlap. The
// handler's mutationMu must serialise a policy create and a policy delete that
// arrives while the create is still applying; overlapping candidate/commit
// sequences are exactly the class of race that crashed the gateway dataplane.
type serialRuntime struct {
	started  chan struct{}
	once     sync.Once
	calls    int32
	active   int32
	overlaps int32
	applies  int32
	commits  int32
}

func (runtime *serialRuntime) ApplyTransparent(_ context.Context, _ string, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	if atomic.AddInt32(&runtime.active, 1) > 1 {
		atomic.AddInt32(&runtime.overlaps, 1)
	}
	// The topology PUT is the first apply; the policy create is the second.
	// Signal only when the policy apply is actually in flight.
	if atomic.AddInt32(&runtime.calls, 1) == 2 {
		runtime.once.Do(func() { close(runtime.started) })
	}
	time.Sleep(120 * time.Millisecond)
	atomic.AddInt32(&runtime.applies, 1)
	atomic.AddInt32(&runtime.active, -1)
	return nil
}

func (runtime *serialRuntime) DisableTransparent(context.Context, string) error { return nil }

func (runtime *serialRuntime) CommitTransparentTransaction(context.Context) error {
	atomic.AddInt32(&runtime.commits, 1)
	return nil
}

func (*serialRuntime) ApplyServiceChain(context.Context, string, orchestrator.ServiceChain, []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error) {
	return vpp.ServiceChainApplyResult{}, nil
}

func TestConcurrentPolicyCreateAndDeleteAreSerialized(t *testing.T) {
	runtime := &serialRuntime{started: make(chan struct{})}
	repository, server := newTestHTTPServer(t, runtime)
	topology, err := os.ReadFile("testdata/topology-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if response := sendRequest(t, server, http.MethodPut, TopologyPath, topology); response.StatusCode != http.StatusOK {
		t.Fatalf("topology: %d %s", response.StatusCode, response.Body)
	}
	policy := []byte(`{"schema_version":1,"ip_objects":[],"policy_groups":[{"id":"default","position":10,"rules":[{"id":"all","sequence":10,"match":{"sources":["any"],"destinations":["any"],"protocol":"any"},"action":{"kind":"via","group":"inline-east"}}]}],"default":{"kind":"direct"}}`)

	var createStatus, deleteStatus int
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		createStatus = sendRequest(t, server, http.MethodPut, PolicyPath, policy).StatusCode
	}()
	<-runtime.started
	go func() {
		defer wait.Done()
		deleteStatus = sendRequest(t, server, http.MethodDelete, PolicyPath, nil).StatusCode
	}()
	wait.Wait()

	if createStatus != http.StatusOK {
		t.Fatalf("create policy status = %d, want 200", createStatus)
	}
	if deleteStatus != http.StatusNoContent {
		t.Fatalf("delete policy status = %d, want 204", deleteStatus)
	}
	if overlaps := atomic.LoadInt32(&runtime.overlaps); overlaps != 0 {
		t.Fatalf("transparent applies overlapped %d time(s); create/delete raced the dataplane", overlaps)
	}
	if _, _, err := repository.PolicySnapshot(context.Background()); !errors.Is(err, orchestrator.ErrPolicyNotFound) {
		t.Fatalf("policy remained after delete: %v", err)
	}
}
