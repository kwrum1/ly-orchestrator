package httpapi

import (
	"context"
	"testing"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/flow"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func TestWithoutQoSPluginGroupsDropsOnlyPluginBackedGroups(t *testing.T) {
	groups := []flow.VPPObjectGroup{
		{Kind: "vpp.qos.classify", Objects: []flow.VPPObject{{RuleID: "voice"}}},
		{Kind: "vpp.qos.record", Objects: []flow.VPPObject{{RuleID: "record"}}},
		{Kind: "vpp.qos.store", Objects: []flow.VPPObject{{RuleID: "store"}}},
		{Kind: "vpp.qos.egress-map", Objects: []flow.VPPObject{{RuleID: "map"}}},
		{Kind: "vpp.qos.mark", Objects: []flow.VPPObject{{RuleID: "mark"}}},
		{Kind: "vpp.acl.drop", Objects: []flow.VPPObject{{RuleID: "drop"}}},
		{Kind: "vpp.behavior.rate", Objects: []flow.VPPObject{{RuleID: "rate"}}},
		{Kind: "vpp.policer", Objects: []flow.VPPObject{{RuleID: "police"}}},
	}
	filtered := withoutQoSPluginGroups(groups)
	got := make([]string, 0, len(filtered))
	for _, group := range filtered {
		got = append(got, group.Kind)
	}
	want := []string{"vpp.acl.drop", "vpp.behavior.rate", "vpp.policer"}
	if len(got) != len(want) {
		t.Fatalf("filtered groups = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("filtered groups = %#v, want %#v", got, want)
		}
	}
}

func TestRuntimeStateTreatsSkippedQoSAsNotDegraded(t *testing.T) {
	components := []RuntimeComponentState{
		{Name: "xray", State: "running", Available: true},
		{Name: "qos", State: "skipped", Reason: "qos plugin unavailable"},
	}
	if state := runtimeState(components); state != RuntimeStateRunning {
		t.Fatalf("runtimeState = %q, want running", state)
	}
}

func TestWANLinkTransactionCommittedAcceptsDegradedCommit(t *testing.T) {
	now := fixedClock()()
	base := RuntimeApplyResult{
		Status:        "degraded",
		RuntimeState:  "degraded",
		TransactionID: "runtime-wan",
		Receipt:       apply.ApplyReceipt{TransactionID: "runtime-wan", Status: apply.ReceiptApplied, AppliedAt: now},
	}
	if !wanLinkTransactionCommitted(base) {
		t.Fatal("committed transaction with a degraded component was rejected")
	}
	base.Receipt.Status = apply.ReceiptFailed
	if wanLinkTransactionCommitted(base) {
		t.Fatal("failed receipt was accepted as committed")
	}
	base.Receipt.Status = apply.ReceiptApplied
	base.Status = "apply_failed"
	if wanLinkTransactionCommitted(base) {
		t.Fatal("apply_failed transaction was accepted as committed")
	}
	base.Status = RuntimeStatusCommitted
	base.Receipt.TransactionID = "other"
	if wanLinkTransactionCommitted(base) {
		t.Fatal("receipt for another transaction was accepted")
	}
}

func TestWANLinkAppliedAfterDegradedCommit(t *testing.T) {
	committed := wanLinkTransactionCommitted(RuntimeApplyResult{
		Status:        "degraded",
		RuntimeState:  "degraded",
		TransactionID: "runtime-wan",
		Receipt:       apply.ApplyReceipt{TransactionID: "runtime-wan", Status: apply.ReceiptApplied},
	})
	item := map[string]any{"id": "acc-wan", "type": "static", "enabled": true, "runtime_state": "desired_not_applied"}
	if !wanLinkAppliedAfterCommit(committed, item) {
		t.Fatal("WAN link stayed pending after a committed degraded transaction")
	}
}

func TestProxyComponentReportsRunningAfterCommittedApply(t *testing.T) {
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
	}}
	server := New(WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))
	now := fixedClock()()
	server.setRuntimeEvidence(RuntimeApplyResult{
		Status:        RuntimeStatusCommitted,
		RuntimeState:  RuntimeStateRunning,
		TransactionID: "runtime-proxy",
		Receipt:       apply.ApplyReceipt{TransactionID: "runtime-proxy", Capability: "/api/v1/runtime/apply", Status: apply.ReceiptApplied, AppliedAt: now},
		Readback:      apply.Readback{TransactionID: "runtime-proxy", Capability: "/api/v1/runtime/apply", Fresh: true, Timestamp: now},
		AppliedAt:     now,
	})
	components := server.runtimeStatusComponents(context.Background(), []serviceRuntime.RenderedArtifact{{Service: serviceRuntime.Xray}}, true)
	for _, component := range components {
		if component.Name != string(serviceRuntime.Xray) {
			continue
		}
		if component.State != "running" || !component.Available {
			t.Fatalf("xray component = %#v, want running", component)
		}
		return
	}
	t.Fatalf("xray component missing from %#v", components)
}
