package apply

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type phasedGatewayTestAdapter struct {
	*gatewayTestAdapter
}

func (adapter *phasedGatewayTestAdapter) RollbackCleanup(_ context.Context, _ Plan) error {
	*adapter.trace = append(*adapter.trace, "cleanup:"+adapter.name)
	return nil
}

func (adapter *phasedGatewayTestAdapter) RollbackRestore(_ context.Context, _ Plan) error {
	*adapter.trace = append(*adapter.trace, "restore:"+adapter.name)
	return nil
}

func phasedGatewayTestAdapters(clock Clock, trace *[]string, failure string) []GatewayResourceAdapter {
	plain, _ := gatewayTestAdapters(clock, trace, failure)
	adapters := make([]GatewayResourceAdapter, 0, len(plain))
	for _, adapter := range plain {
		adapters = append(adapters, &phasedGatewayTestAdapter{gatewayTestAdapter: adapter.(*gatewayTestAdapter)})
	}
	return adapters
}

func TestGatewayTransactionFailureRunsPhasedCleanupForEveryAppliedResource(t *testing.T) {
	clock := deterministicClock(time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC))
	trace := []string{}
	transaction := &GatewayMultiResourceTransaction{Adapters: phasedGatewayTestAdapters(clock, &trace, "routes"), Now: clock}

	_, err := transaction.Run(context.Background(), Plan{Request: Request{TransactionID: "txn-phased-cleanup", Resource: "/api/v1/runtime/apply"}})
	if err == nil {
		t.Fatal("transaction must fail when a resource fails")
	}
	want := []string{
		"interfaces", "bonds", "wan-groups", "routes",
		"cleanup:routes", "cleanup:wan-groups", "cleanup:bonds", "cleanup:interfaces",
		"restore:interfaces", "restore:bonds", "restore:wan-groups", "restore:routes", "restore:acls", "restore:qos", "restore:nat44", "restore:port-maps",
	}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("rollback trace = %#v, want %#v", trace, want)
	}
}

func TestGatewayTransactionReapplySucceedsAfterFailedRollback(t *testing.T) {
	ctx := context.Background()
	clock := deterministicClock(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	trace := []string{}
	adapters, concrete := gatewayTestAdapters(clock, &trace, "routes")
	first := &GatewayMultiResourceTransaction{Adapters: adapters, Now: clock}
	if _, err := first.Run(ctx, Plan{Request: Request{TransactionID: "txn-reapply-1", Resource: "/api/v1/runtime/apply"}}); err == nil {
		t.Fatal("first transaction must fail")
	}

	// The failed generation rolled back every applied resource; the next apply
	// must start from a clean transaction state instead of stale adapters.
	for _, adapter := range concrete {
		adapter.fail = false
	}
	second := &GatewayMultiResourceTransaction{Adapters: adapters, Now: clock}
	result, err := second.Run(ctx, Plan{Request: Request{TransactionID: "txn-reapply-2", Resource: "/api/v1/runtime/apply"}})
	if err != nil {
		t.Fatalf("re-apply after rollback failed: %v", err)
	}
	wantOrder := []string{"interfaces", "bonds", "wan-groups", "routes", "acls", "qos", "nat44", "port-maps"}
	if !reflect.DeepEqual(result.Order, wantOrder) {
		t.Fatalf("re-apply order = %#v, want %#v", result.Order, wantOrder)
	}
}
