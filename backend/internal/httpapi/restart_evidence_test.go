package httpapi

import (
	"context"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// A committed generation stays authoritative across a restart: the persisted
// receipt/readback is commit evidence whose age says nothing about liveness.
// When the service runtime can prove the components are healthy, the gateway
// must report committed/running instead of degrading on the readback age.
func TestReconcileRuntimeKeepsCommittedGenerationWhenLiveServicesAreHealthy(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-reconcile-live-health?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := fixedClock()()
	transactionID := "txn-restart-live-health"
	result := RuntimeApplyResult{
		Status:        RuntimeStatusCommitted,
		RuntimeState:  RuntimeStateRunning,
		TransactionID: transactionID,
		Receipt:       apply.ApplyReceipt{TransactionID: transactionID, Capability: "/api/v1/config/apply", Status: apply.ReceiptApplied, AppliedAt: now.Add(-30 * time.Minute)},
		Readback:      apply.Readback{TransactionID: transactionID, Capability: "/api/v1/config/apply", Timestamp: now.Add(-30 * time.Minute), Fresh: true},
		AppliedAt:     now.Add(-30 * time.Minute),
	}
	payload, hash, err := persistence.MarshalPayload(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveApply(ctx, persistence.ApplyRecord{Snapshot: persistence.RuntimeSnapshot{ID: "snapshot-" + transactionID, SourceTransactionID: transactionID, Payload: payload, PayloadHash: hash, CreatedAt: result.AppliedAt}}); err != nil {
		t.Fatal(err)
	}
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.VPP:      {Service: serviceRuntime.VPP, Available: true},
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
	}}
	server := New(WithStore(store), WithClock(fixedClock()), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	server.reconcileRuntime(ctx)

	got := server.runtimeEvidence()
	if got.Status != RuntimeStatusCommitted || got.RuntimeState != RuntimeStateRunning {
		t.Fatalf("restart evidence = %#v, want committed/running with live healthy services", got)
	}
	if got.ReconciliationReceipt.Status != apply.ReceiptApplied || got.ReconciliationReceipt.TransactionID != transactionID {
		t.Fatalf("reconciliation receipt = %#v", got.ReconciliationReceipt)
	}
	components := server.reprobeRuntimeComponents(ctx, got.Components)
	if state := runtimeState(components); state != RuntimeStateRunning {
		t.Fatalf("component state = %q (%#v), want running", state, components)
	}
}

// Without a live service-runtime probe an aged readback still fails closed:
// the gateway must not claim a running state it cannot verify.
func TestReconcileRuntimeKeepsStaleFailureWithoutLiveProbe(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-reconcile-no-live-probe?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := fixedClock()()
	transactionID := "txn-restart-no-live-probe"
	result := RuntimeApplyResult{
		Status:        RuntimeStatusCommitted,
		RuntimeState:  RuntimeStateRunning,
		TransactionID: transactionID,
		Receipt:       apply.ApplyReceipt{TransactionID: transactionID, Capability: "/api/v1/config/apply", Status: apply.ReceiptApplied, AppliedAt: now.Add(-30 * time.Minute)},
		Readback:      apply.Readback{TransactionID: transactionID, Capability: "/api/v1/config/apply", Timestamp: now.Add(-30 * time.Minute), Fresh: true},
		AppliedAt:     now.Add(-30 * time.Minute),
	}
	payload, hash, err := persistence.MarshalPayload(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveApply(ctx, persistence.ApplyRecord{Snapshot: persistence.RuntimeSnapshot{ID: "snapshot-" + transactionID, SourceTransactionID: transactionID, Payload: payload, PayloadHash: hash, CreatedAt: result.AppliedAt}}); err != nil {
		t.Fatal(err)
	}
	server := New(WithStore(store), WithClock(fixedClock()))
	server.reconcileRuntime(ctx)

	got := server.runtimeEvidence()
	if got.Status == RuntimeStatusCommitted || got.RuntimeState == RuntimeStateRunning {
		t.Fatalf("unverifiable restart evidence = %#v, want degraded", got)
	}
	if got.ReconciliationReceipt.Cause != "stale persisted runtime readback" {
		t.Fatalf("reconciliation cause = %q", got.ReconciliationReceipt.Cause)
	}
}
