package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	controlapi "ly-route/backend/internal/api"
	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func (server *Server) orchestratorTransparentRuntimeComponents(ctx context.Context) ([]RuntimeComponentState, bool) {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil, false
	}
	provider, ok := server.orchestratorRuntime.(orchestratorapi.TransparentRuntimeEvidenceProvider)
	if !ok {
		return nil, false
	}
	evidence, err := provider.TransparentRuntimeEvidence(ctx)
	if err != nil {
		now := server.now().UTC()
		reason := "transparent orchestrator runtime readback failed: " + err.Error()
		server.setRuntimeEvidence(degradedRuntimeResult("orchestrator-observe", reason, now))
		components := []RuntimeComponentState{
			{Name: "vpp", State: "degraded", Available: false, Reason: reason},
			{Name: "persistence", State: "running", Available: server.store != nil, Reason: persistenceRuntimeReason(server.store != nil)},
		}
		return server.applyRuntimeEvidence(components), true
	}
	if strings.TrimSpace(evidence.TransactionID) == "" || strings.TrimSpace(evidence.Generation) == "" || evidence.State != "running" || evidence.AppliedAt.IsZero() || evidence.ObservedAt.IsZero() || evidence.ObservedAt.Before(evidence.AppliedAt) {
		now := server.now().UTC()
		reason := fmt.Sprintf("transparent orchestrator runtime evidence is incomplete: transaction=%q generation=%q state=%q", evidence.TransactionID, evidence.Generation, evidence.State)
		server.setRuntimeEvidence(degradedRuntimeResult(nonEmpty(evidence.TransactionID, "orchestrator-observe"), reason, now))
		return server.applyRuntimeEvidence([]RuntimeComponentState{{Name: "vpp", State: "degraded", Reason: reason}, {Name: "persistence", State: "running", Available: server.store != nil, Reason: persistenceRuntimeReason(server.store != nil)}}), true
	}
	capability := "vpp.transparent-orchestrator"
	result := RuntimeApplyResult{
		Status: RuntimeStatusCommitted, RuntimeState: RuntimeStateRunning,
		TransactionID: evidence.TransactionID, AppliedAt: evidence.AppliedAt,
		Receipt:               apply.ApplyReceipt{TransactionID: evidence.TransactionID, Capability: capability, Status: apply.ReceiptApplied, AppliedAt: evidence.AppliedAt},
		Readback:              apply.Readback{TransactionID: evidence.TransactionID, Capability: capability, Timestamp: evidence.ObservedAt, Fresh: true},
		ReconciliationReceipt: apply.ReconciliationReceipt{TransactionID: evidence.TransactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptApplied, Cause: "live transparent VPP generation verified", Timestamp: evidence.ObservedAt},
	}
	components := []RuntimeComponentState{
		{Name: "vpp", State: "running", Available: true, Reason: "transparent VPP generation " + evidence.Generation + " verified"},
		{Name: "persistence", State: "running", Available: server.store != nil, Reason: persistenceRuntimeReason(server.store != nil)},
	}
	// Persist the inventory with the receipt so every consumer (status,
	// telemetry, cached reprobe) sees the same transparent component set.
	result.Components = append([]RuntimeComponentState(nil), components...)
	server.setRuntimeEvidence(result)
	return server.applyRuntimeEvidence(components), true
}

// orchestratorStatusComponents is the only component inventory the
// Orchestrator profile may report: the transparent VPP generation and local
// persistence. The Gateway service set (smartdns/dnsmasq/xray/pppoe) and the
// Gateway plan-operation evidence describe a different product and must never
// be consulted here. handled is false for every other profile, so the Gateway
// status path is unchanged.
func (server *Server) orchestratorStatusComponents(ctx context.Context) ([]RuntimeComponentState, bool) {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil, false
	}
	if components, handled := server.orchestratorTransparentRuntimeComponents(ctx); handled {
		return components, true
	}
	reason := "transparent orchestrator runtime evidence is not configured"
	server.setRuntimeEvidence(degradedRuntimeResult("orchestrator-observe", reason, server.now().UTC()))
	return server.applyRuntimeEvidence([]RuntimeComponentState{
		{Name: "vpp", State: "degraded", Available: false, Reason: reason},
		{Name: "persistence", State: "running", Available: server.store != nil, Reason: persistenceRuntimeReason(server.store != nil)},
	}), true
}

// orchestratorRuntimeCapability makes /health reflect the committed
// transparent dataplane, not just vpp.service activity.  A restarted VPP loads
// the plugin in the locked state while the unit remains active; without this
// check the appliance reports a false green even though orchestration traffic
// is black-holed.
func (server *Server) orchestratorRuntimeCapability(ctx context.Context) *controlapi.CapabilityState {
	if server.profile.ID() != product.Orchestrator().ID() || server.orchestratorRepository == nil {
		return nil
	}
	if _, _, err := server.orchestratorRepository.Snapshot(ctx); errors.Is(err, orchestrator.ErrTopologyNotFound) {
		return nil
	} else if err != nil {
		return &controlapi.CapabilityState{Name: "orchestrator_runtime", Available: false, State: controlapi.CapabilityDegraded, Reason: serviceRuntime.Redact("read orchestrator topology: " + err.Error())}
	}
	provider, ok := server.orchestratorRuntime.(orchestratorapi.TransparentRuntimeEvidenceProvider)
	if !ok {
		return &controlapi.CapabilityState{Name: "orchestrator_runtime", Available: false, State: controlapi.CapabilityDegraded, Reason: "transparent orchestrator runtime evidence is not configured"}
	}
	evidence, err := provider.TransparentRuntimeEvidence(ctx)
	if err != nil {
		return &controlapi.CapabilityState{Name: "orchestrator_runtime", Available: false, State: controlapi.CapabilityDegraded, Reason: serviceRuntime.Redact(err.Error())}
	}
	if evidence.State != "running" {
		return &controlapi.CapabilityState{Name: "orchestrator_runtime", Available: false, State: controlapi.CapabilityDegraded, Reason: serviceRuntime.Redact("transparent orchestrator state is " + evidence.State)}
	}
	return &controlapi.CapabilityState{Name: "orchestrator_runtime", Available: true, State: controlapi.CapabilityAvailable}
}

func persistenceRuntimeReason(available bool) string {
	if available {
		return "persisted topology and policy are available"
	}
	return "local store is not configured"
}
