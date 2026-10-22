package apply

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func graphHookAdapters(clock Clock, trace *[]string, fail string) []GatewayResourceAdapter {
	names := []string{"interfaces", "service-networks", "routes"}
	adapters := make([]GatewayResourceAdapter, 0, len(names))
	for _, name := range names {
		adapter := &gatewayTestAdapter{name: name, clock: clock, trace: trace, fail: name == fail}
		adapters = append(adapters, &phasedGatewayTestAdapter{gatewayTestAdapter: adapter})
	}
	return adapters
}

func TestGatewayGraphAppliesPPPoEBeforeServiceNetworksAndServicesLast(t *testing.T) {
	clock := deterministicClock(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC))
	var trace []string
	hooks := &ApplyHooks{
		EnsurePPPoE: func(context.Context, Plan) error {
			trace = append(trace, "hook:pppoe")
			return nil
		},
		VerifyPPPoE: func(context.Context, Plan) error {
			trace = append(trace, "wait:pppoe")
			return nil
		},
		ApplyServices: func(context.Context, Plan) error {
			trace = append(trace, "hook:services")
			return nil
		},
		VerifyServiceNetworks: func(context.Context, Plan) error {
			trace = append(trace, "wait:nat-role")
			return nil
		},
	}
	transaction := &GatewayMultiResourceTransaction{Adapters: graphHookAdapters(clock, &trace, ""), Now: clock}
	plan := Plan{Request: Request{TransactionID: "txn-graph-hooks", Resource: "/api/v1/runtime/apply"}, Hooks: hooks}
	if _, err := transaction.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	want := []string{"interfaces", "hook:pppoe", "wait:pppoe", "service-networks", "wait:nat-role", "routes", "hook:services"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("graph trace = %v, want %v", trace, want)
	}
}

func TestGatewayGraphRollsBackServicesPPPoEAndNetworksInReverse(t *testing.T) {
	clock := deterministicClock(time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC))
	var trace []string
	hooks := &ApplyHooks{
		EnsurePPPoE:   func(context.Context, Plan) error { trace = append(trace, "hook:pppoe"); return nil },
		VerifyPPPoE:   func(context.Context, Plan) error { return nil },
		RollbackPPPoE: func(context.Context, Plan) error { trace = append(trace, "rollback:pppoe"); return nil },
		ApplyServices: func(context.Context, Plan) error {
			trace = append(trace, "hook:services")
			return errors.New("Kea handoff failed")
		},
		RollbackServices: func(context.Context, Plan) error {
			trace = append(trace, "rollback:services")
			return nil
		},
	}
	transaction := &GatewayMultiResourceTransaction{Adapters: graphHookAdapters(clock, &trace, ""), Now: clock}
	plan := Plan{Request: Request{TransactionID: "txn-graph-rollback", Resource: "/api/v1/runtime/apply"}, Hooks: hooks}
	if _, err := transaction.Run(context.Background(), plan); err == nil {
		t.Fatal("services failure must roll back the generation")
	}
	want := []string{
		"interfaces", "hook:pppoe", "service-networks", "routes", "hook:services",
		"rollback:services", "cleanup:routes", "cleanup:service-networks", "rollback:pppoe", "cleanup:interfaces",
		"restore:interfaces", "restore:service-networks", "restore:routes",
	}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("rollback trace = %v, want %v", trace, want)
	}
}
