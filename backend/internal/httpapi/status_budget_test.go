package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

type slowHealthController struct {
	delay     time.Duration
	available atomic.Bool
	calls     atomic.Int32
}

func (controller *slowHealthController) Receipt(_ context.Context, request RuntimeEvidenceRequest) (apply.ApplyReceipt, error) {
	return apply.ApplyReceipt{TransactionID: request.TransactionID, Capability: request.Capability, Status: apply.ReceiptApplied, AppliedAt: fixedClock()()}, nil
}

func (controller *slowHealthController) Readback(_ context.Context, request RuntimeEvidenceRequest) (apply.Readback, error) {
	return apply.Readback{TransactionID: request.TransactionID, Capability: request.Capability, Timestamp: fixedClock()(), Fresh: true}, nil
}

func (controller *slowHealthController) ReloadOrRestart(context.Context, serviceRuntime.ServiceName, []serviceRuntime.RenderedArtifact) error {
	return nil
}

func (controller *slowHealthController) Status(ctx context.Context, service serviceRuntime.ServiceName) (serviceRuntime.Health, error) {
	controller.calls.Add(1)
	select {
	case <-time.After(controller.delay):
	case <-ctx.Done():
		return serviceRuntime.Health{Service: service, Available: false, TimedOut: true, Reason: "health probe timed out"}, nil
	}
	return serviceRuntime.Health{Service: service, Available: controller.available.Load()}, nil
}

func (*slowHealthController) Rollback(context.Context, serviceRuntime.ServiceName, []serviceRuntime.RenderedArtifact) error {
	return nil
}

func (*slowHealthController) Logs(context.Context, serviceRuntime.ServiceName, int) (string, error) {
	return "", nil
}

// Status polls must re-probe live component health without rebuilding the
// whole runtime plan, and the probes must stay parallel and bounded.
func TestRuntimeStatusRepliesWithinBudgetFromCachedComponents(t *testing.T) {
	controller := &slowHealthController{delay: 300 * time.Millisecond}
	controller.available.Store(true)
	server := New(WithClock(fixedClock()), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	now := fixedClock()()
	transactionID := "txn-status-budget"
	server.setRuntimeEvidence(RuntimeApplyResult{
		Status:        RuntimeStatusCommitted,
		RuntimeState:  RuntimeStateRunning,
		TransactionID: transactionID,
		Receipt:       apply.ApplyReceipt{TransactionID: transactionID, Capability: "/api/v1/config/apply", Status: apply.ReceiptApplied, AppliedAt: now},
		Readback:      apply.Readback{TransactionID: transactionID, Capability: "/api/v1/config/apply", Timestamp: now, Fresh: true},
		AppliedAt:     now,
		Components: []RuntimeComponentState{
			{Name: string(serviceRuntime.SmartDNS), State: "running", Available: true},
			{Name: string(serviceRuntime.Dnsmasq), State: "running", Available: true},
			{Name: string(serviceRuntime.Xray), State: "running", Available: true},
			{Name: string(serviceRuntime.PPPoE), State: "running", Available: true},
		},
	})

	started := time.Now()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/runtime/status", nil))
	elapsed := time.Since(started)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("status took %s from cached components with a 300ms health probe", elapsed)
	}
	if controller.calls.Load() == 0 {
		t.Fatal("status served cached state without re-probing live health")
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("healthy status body = %s", body)
	}
}
