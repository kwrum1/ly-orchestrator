package apply

import (
	"context"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/proxy"
)

// TestExecutorAcceptsEmptyProxyEgressAsDeletedGeneration covers the F2 read
// path: deleting the last proxy egress stores a soft-deleted tombstone and the
// next apply receives an empty egress. That is a valid no-proxy generation, not
// an invalid egress, and must still persist a commit snapshot.
func TestExecutorAcceptsEmptyProxyEgressAsDeletedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx)
	clock := deterministicClock(time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC))
	executor := Executor{
		Store: store,
		Now:   clock,
		Receipt: func(_ context.Context, plan Plan) (ApplyReceipt, error) {
			return ApplyReceipt{TransactionID: plan.Request.TransactionID, Status: ReceiptApplied, Capability: plan.Request.Resource, AppliedAt: clock()}, nil
		},
		Readback: func(_ context.Context, plan Plan) (Readback, error) {
			return Readback{TransactionID: plan.Request.TransactionID, Capability: plan.Request.Resource, Timestamp: clock(), Fresh: true}, nil
		},
	}
	request := validRequest("txn-no-proxy")
	request.ProxyEgress = proxy.Egress{}

	result, err := executor.Run(ctx, request)
	if err != nil {
		t.Fatalf("empty proxy egress apply failed: %v", err)
	}
	if result.Receipt.Status != ReceiptApplied || result.Plan.CompiledProxy.ID != "" || len(result.Plan.CompiledProxy.VPPSteering) != 0 {
		t.Fatalf("no-proxy result = receipt=%#v compiled=%#v", result.Receipt, result.Plan.CompiledProxy)
	}
}
