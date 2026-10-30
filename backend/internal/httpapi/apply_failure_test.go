package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/vpp"
)

// A failed apply must not replace the committed generation. The failure is
// exposed separately so /api/v1/runtime/status keeps reporting the previous
// apply=committed generation.
func TestFailedApplyKeepsCommittedGeneration(t *testing.T) {
	server := &Server{}
	committed := RuntimeApplyResult{Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning, TransactionID: "txn-committed", SnapshotHash: "hash-committed", AppliedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	server.runtimeMu.Lock()
	server.lastRuntime = &committed
	server.runtimeMu.Unlock()
	failed := degradedRuntimeResult("txn-failed", "routes apply failed", time.Date(2026, 10, 27, 9, 5, 0, 0, time.UTC))
	failed.Status = "apply_failed"
	server.publishFailedRuntimeResult(nil, failed)

	if got := server.runtimeEvidence(); got.TransactionID != "txn-committed" || got.Status != RuntimeStatusCommitted {
		t.Fatalf("committed generation was replaced by the failed apply: %#v", got)
	}
	if got := server.lastFailedRuntimeApply(); got == nil || got.TransactionID != "txn-failed" {
		t.Fatalf("failed apply was not recorded separately: %#v", got)
	}
}

// A rejected or timed-out apply must never replace the committed generation.
// Every non-committed status is recorded separately as last_failed_apply.
func TestNonCommittedApplyDoesNotReplaceCommittedGeneration(t *testing.T) {
	server := &Server{}
	committed := RuntimeApplyResult{Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning, TransactionID: "txn-committed", SnapshotHash: "hash-committed", AppliedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	server.setRuntimeEvidence(committed)

	for _, status := range []string{"compile_failed", "apply_failed", "af_fallback_required", "dataplane_degraded", "unavailable"} {
		failed := degradedRuntimeResult("txn-"+status, "injected "+status, time.Date(2026, 10, 27, 9, 5, 0, 0, time.UTC))
		failed.Status = status
		server.publishRuntimeResult(nil, failed)
		if got := server.runtimeEvidence(); got.TransactionID != "txn-committed" {
			t.Fatalf("%s replaced the committed generation: %#v", status, got)
		}
		if got := server.lastFailedRuntimeApply(); got == nil || got.Status != status {
			t.Fatalf("%s was not recorded as last_failed_apply: %#v", status, got)
		}
	}

	// A committed generation (even degraded) still replaces last_apply.
	next := RuntimeApplyResult{Status: "degraded", RuntimeState: "degraded", TransactionID: "txn-degraded", AppliedAt: time.Date(2026, 10, 27, 9, 10, 0, 0, time.UTC)}
	server.publishRuntimeResult(nil, next)
	if got := server.runtimeEvidence(); got.TransactionID != "txn-degraded" {
		t.Fatalf("committed degraded generation was not published: %#v", got)
	}
}

// The failure response must report the generation that is still running, the
// rolled-back state, and the actionable error envelope.
func TestApplyFailureResponseReportsCommittedGeneration(t *testing.T) {
	server := &Server{}
	committed := RuntimeApplyResult{Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning, TransactionID: "txn-committed", SnapshotHash: "hash-committed", AppliedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	// Record the committed generation directly: this test is about the failure
	// response, not about re-validating a receipt.
	server.runtimeMu.Lock()
	server.lastRuntime = &committed
	server.runtimeMu.Unlock()
	result := RuntimeApplyResult{
		Status:          "apply_failed",
		RuntimeState:    "degraded",
		TransactionID:   "txn-failed",
		RollbackReceipt: apply.RollbackReceipt{TransactionID: "txn-failed", Status: apply.ReceiptRolledBack},
	}
	response := server.applyFailureResponse(result, "runtime transaction: gateway multi-resource transaction failed: resource routes: apply node routes run failed: pre-NAT classifier is missing", []string{"lylan-ens256 192.168.10.1/24"})

	if response["rolled_back"] != true {
		t.Fatalf("rolled_back = %v, want true", response["rolled_back"])
	}
	committedSummary, ok := response["committed"].(map[string]any)
	if !ok || committedSummary["transaction_id"] != "txn-committed" {
		t.Fatalf("committed generation not reported: %#v", response["committed"])
	}
	envelope, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %#v", response)
	}
	if envelope["object"] != "routes" || envelope["step"] != "apply" {
		t.Fatalf("envelope object/step = %v/%v, want routes/apply", envelope["object"], envelope["step"])
	}
	if suggestion, _ := envelope["suggestion"].(string); !strings.Contains(suggestion, "next_hop") {
		t.Fatalf("suggestion is not actionable: %q", suggestion)
	}
	if reason, _ := response["reason"].(string); !strings.Contains(reason, "was restored") {
		t.Fatalf("reason does not say the previous generation was restored: %q", reason)
	}

	failedRollback := RuntimeApplyResult{Status: "apply_failed", RuntimeState: "degraded", TransactionID: "txn-failed", RollbackReceipt: apply.RollbackReceipt{Status: apply.ReceiptFailed, RollbackError: "lcp missing"}}
	response = server.applyFailureResponse(failedRollback, "resource interfaces: LCP restore failed", nil)
	if response["rolled_back"] != false {
		t.Fatalf("failed rollback reported rolled_back=%v", response["rolled_back"])
	}
	if reason, _ := response["reason"].(string); !strings.Contains(reason, "rollback reported errors") {
		t.Fatalf("failed rollback reason is misleading: %q", reason)
	}
}

// A DHCP service bound to an explicit WAN port must fail the plan instead of
// being silently skipped: no dataplane can serve DHCP from a WAN port.
func TestDHCPServerOnExplicitWANFailsPlan(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "dhcp_server",
		ResourceID:   "uiacc-dhcp-wan",
		Payload:      []byte(`{"id":"uiacc-dhcp-wan","interface_id":"ens224","subnet":"192.168.10.0/24","pools":["192.168.10.100-192.168.10.110"],"enabled":true}`),
		PayloadHash:  "fixture-dhcp-wan",
	}); err != nil {
		t.Fatalf("seed dhcp: %v", err)
	}
	assignments := []vpp.AddressAssignment{
		{ID: "wan-ens224", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", CIDR: "100.64.0.2/24", Role: "wan"},
		{ID: "lan-ens256", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"},
	}
	if _, _, _, err := server.currentDHCPServers(ctx, assignments, true); err == nil {
		t.Fatal("a DHCP service on an explicit WAN port must fail the plan")
	} else if !strings.Contains(err.Error(), "WAN link") || !strings.Contains(err.Error(), "uiacc-dhcp-wan") {
		t.Fatalf("failure is not actionable: %v", err)
	}
}

func TestExplicitWANAssignmentMatchesEverySpelling(t *testing.T) {
	assignments := []vpp.AddressAssignment{{ID: "wan-eth2", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", Role: "wan"}}
	for _, reference := range []struct{ canonical, linux string }{{"eth2", "ens224"}, {"", "ens224"}, {"lyroute-ens224", ""}} {
		if !explicitWANAssignment(assignments, reference.canonical, reference.linux) {
			t.Fatalf("WAN assignment not matched for %#v", reference)
		}
	}
	if explicitWANAssignment(assignments, "eth1", "ens256") {
		t.Fatal("LAN port was reported as an explicit WAN")
	}
}

// The failure envelope must name the object, the step, and a concrete
// suggestion instead of returning a bare internal error chain.
func TestApplyErrorEnvelopeIsActionable(t *testing.T) {
	cause := "runtime transaction: gateway multi-resource apply: gateway multi-resource transaction failed: resource routes: apply node routes run failed: route/WAN-group operation readback failed: radix FIB path does not match candidate"
	receipt := apply.RollbackReceipt{Status: apply.ReceiptRolledBack}
	envelope := applyErrorEnvelope(cause, receipt, []string{"lylan-ens256 192.168.10.1/24"})
	if envelope["object"] != "routes" {
		t.Fatalf("object = %v, want routes", envelope["object"])
	}
	if envelope["step"] != "apply" {
		t.Fatalf("step = %v, want apply", envelope["step"])
	}
	if envelope["rolled_back"] != true {
		t.Fatalf("rolled_back = %v, want true", envelope["rolled_back"])
	}
	suggestion, _ := envelope["suggestion"].(string)
	if !strings.Contains(suggestion, "next_hop") {
		t.Fatalf("suggestion is not actionable: %q", suggestion)
	}
	repaired, _ := envelope["repaired"].([]string)
	if len(repaired) != 1 {
		t.Fatalf("repaired = %#v, want the LAN address safety net entry", envelope["repaired"])
	}

	failedRollback := applyErrorEnvelope(cause, apply.RollbackReceipt{Status: apply.ReceiptFailed, RollbackError: "lcp missing"}, nil)
	if failedRollback["step"] != "rollback" {
		t.Fatalf("failed rollback step = %v, want rollback", failedRollback["step"])
	}
	if failedRollback["rollback_error"] != "lcp missing" {
		t.Fatalf("rollback error not surfaced: %#v", failedRollback)
	}
}
