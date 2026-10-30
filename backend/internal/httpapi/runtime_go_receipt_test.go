package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/applystate"
	"ly-route/backend/internal/runtime/vpp"
)

func TestGoTransactionReceiptBindsAndAdvancesWithoutVPPApply(t *testing.T) {
	server, _ := newApplyStateTestServer(t)
	begin, err := server.beginRuntimeApplyGeneration("hash-go", "txn-go")
	if err != nil {
		t.Fatal(err)
	}
	records := []vpp.CommandRecord{
		{Operation: "vpp.route-policy", Resource: "route-office", Command: "set ly-route pre-nat-route add id 64999 priority 10 source 192.0.2.0/24 destination 0.0.0.0/0", Status: "applied", Attempts: 1},
		{Operation: "vpp.route-policy.snapshot", Command: "show ly-route pre-nat-route", Status: "readback", Attempts: 1},
	}
	receipt := buildGoVPPReceipt("txn-go", "hash-go", begin.State.DesiredGeneration, records, applystate.StatusApplied, false, nil)
	if err := server.publishGoVPPReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	state, err := server.commitRuntimeApplyStateWithReceipt("txn-go", "hash-go", begin, apply.Result{}, nil, nil, false, &receipt)
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.Status != applystate.StatusApplied {
		t.Fatalf("state = %#v", state)
	}
	persisted, err := applystate.LoadReceipt(server.vppReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TransactionID != "txn-go" || persisted.ExecutorTransactionID == "" || len(persisted.Steps) != 2 {
		t.Fatalf("receipt = %#v", persisted)
	}
	if persisted.Steps[0].Command == "" || persisted.Steps[0].Status != "applied" || persisted.Steps[1].Status != "readback" {
		t.Fatalf("receipt steps = %#v", persisted.Steps)
	}
}

func TestGoTransactionReceiptFailedStepIsNotCommitted(t *testing.T) {
	server, _ := newApplyStateTestServer(t)
	begin, err := server.beginRuntimeApplyGeneration("hash-go", "txn-go")
	if err != nil {
		t.Fatal(err)
	}
	records := []vpp.CommandRecord{{
		Operation: "vpp.route-policy", Command: "set ly-route pre-nat-route add id 64999", Status: "failed", Attempts: 2,
		Error: "vppctl command failed",
	}}
	receipt := buildGoVPPReceipt("txn-go", "hash-go", begin.State.DesiredGeneration, records, applystate.StatusApplied, false, nil)
	if receipt.Status != applystate.StatusFailed || receipt.AppliedGeneration != 0 || !receipt.Partial {
		t.Fatalf("failed record receipt = %#v", receipt)
	}
	if _, err := server.commitRuntimeApplyStateWithReceipt("txn-go", "hash-go", begin, apply.Result{}, nil, nil, false, &receipt); err == nil {
		t.Fatal("failed receipt was committed")
	}
	state, err := server.applyGenerationState.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 0 {
		t.Fatalf("failed receipt advanced generation: %#v", state)
	}
}

func TestUnchangedComponentsAreNotDegradedByAnOlderReceipt(t *testing.T) {
	server := New(WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithClock(fixedClock()))
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	server.setRuntimeEvidence(RuntimeApplyResult{
		Status: "degraded", RuntimeState: "degraded", TransactionID: "runtime-current",
		Receipt:   apply.ApplyReceipt{TransactionID: "runtime-current", Capability: "/api/v1/runtime/apply", Status: apply.ReceiptApplied, AppliedAt: now},
		Readback:  apply.Readback{TransactionID: "runtime-current", Capability: "/api/v1/runtime/apply", Timestamp: now, Fresh: true},
		Applied:   []string{"xray"},
		AppliedAt: now,
	})
	components := []RuntimeComponentState{
		{Name: "smartdns", State: "running", Available: true},
		{Name: "dnsmasq", State: "running", Available: true},
		{Name: "vpp", State: "running", Available: true},
	}
	components = server.applyRuntimeEvidence(components)
	components = server.attachServiceEvidence(context.Background(), components, nil)
	for _, component := range components {
		if component.State != "running" || !component.Available {
			t.Fatalf("%s = %#v, want running", component.Name, component)
		}
		if !component.Unchanged {
			t.Fatalf("%s was not marked unchanged: %#v", component.Name, component)
		}
	}
}

func TestGoTransactionReceiptPathIsStable(t *testing.T) {
	server, _ := newApplyStateTestServer(t)
	if server.vppReceiptPath == "" {
		t.Fatal("vpp receipt path is empty")
	}
}

func TestGoVPPReceiptIsNotPartialWithoutConcreteReason(t *testing.T) {
	records := []vpp.CommandRecord{
		{Command: "set interface state lan0 up", Status: "applied", Attempts: 1},
		{Command: "show interface", Status: "readback", Attempts: 1},
	}
	// A caller hint of "something degraded" without a named cause must not
	// produce a partial transaction: all steps applied and no failure exists.
	receipt := buildGoVPPReceipt("txn-ok", "hash-go", 1, records, applystate.StatusApplied, true, nil)
	if receipt.Partial || len(receipt.DegradedReasons) != 0 {
		t.Fatalf("receipt invented a degraded reason: %#v", receipt)
	}
	if err := receipt.Validate(applystate.Expectation{TransactionID: "txn-ok", DesiredGeneration: 1, DesiredHash: "hash-go"}); err != nil {
		t.Fatalf("healthy receipt did not validate: %v", err)
	}
}

func TestGoVPPReceiptFailedStepCarriesConcreteReason(t *testing.T) {
	records := []vpp.CommandRecord{{
		Command: "set ly-route pre-nat-route add id 1", Status: "failed", Attempts: 1,
		Error: "vppctl returned exit code 1",
	}}
	receipt := buildGoVPPReceipt("txn-bad", "hash-go", 1, records, applystate.StatusApplied, true, nil)
	if !receipt.Partial || len(receipt.DegradedReasons) == 0 {
		t.Fatalf("failed step did not mark partial: %#v", receipt)
	}
	if !strings.Contains(receipt.DegradedReasons[0], "pre-nat-route") || !strings.Contains(receipt.DegradedReasons[0], "exit code 1") {
		t.Fatalf("failed step reason is not concrete: %#v", receipt.DegradedReasons)
	}
}
