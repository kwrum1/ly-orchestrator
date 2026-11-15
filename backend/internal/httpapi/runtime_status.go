package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

const (
	RuntimeStatusCommitted = "committed"
	RuntimeStateRunning    = "running"

	// statusPlanBudget bounds the status request end to end. The component
	// phase gets its own detached budget (statusComponentBudget) so health
	// probes cannot inherit an exhausted plan deadline.
	statusPlanBudget      = 12 * time.Second
	statusComponentBudget = 5 * time.Second
)

type RuntimeEvidenceProvider = serviceRuntime.RuntimeEvidenceProvider

type RuntimeEvidenceRequest = serviceRuntime.EvidenceRequest

func (server *Server) setRuntimeEvidence(result RuntimeApplyResult) {
	// Validate the commit at its own commit time. The receipt/readback pair is
	// immutable commit evidence: its age must not retroactively degrade a
	// generation that was fresh when it committed (a restart hours later would
	// otherwise always report degraded). Apply-time freshness is still enforced
	// by the executor before the receipt is persisted.
	commitTime := runtimeCommitValidationTime(result)
	// A no-op/unchanged publish carries no receipt of its own: the evidence it
	// republishes was validated when that generation committed. Re-validating
	// it here fails on the missing receipt timestamp and turned every no-op
	// apply into a "partial success" banner in the UI.
	if result.Receipt.Status == apply.ReceiptApplied && (result.Status == RuntimeStatusCommitted || result.RuntimeState == RuntimeStateRunning) && !completeRuntimeEvidence(result, commitTime) {
		result.Status = apply.ReceiptDegraded
		result.RuntimeState = "degraded"
		result.Reason = incompleteRuntimeEvidenceReason(result, commitTime)
	}
	server.clearReconciliationNotice()
	server.runtimeMu.Lock()
	// A committed generation supersedes every older failure explanation; leaving
	// it behind kept 配置异常 on screen after the operator fixed the problem.
	if result.Status == RuntimeStatusCommitted && result.RuntimeState == RuntimeStateRunning {
		server.lastFailedApply = nil
	}
	previousTransactionID := ""
	if server.lastRuntime != nil {
		previousTransactionID = server.lastRuntime.TransactionID
	}
	server.lastRuntime = &result
	server.runtimeMu.Unlock()
	// Only a new generation invalidates the cached desired hash. Status
	// polling republishes the same committed evidence and must not defeat the
	// 60s cache that keeps the list endpoints cheap.
	if previousTransactionID != result.TransactionID {
		server.invalidateDesiredDisplayHash()
	}
	// A committed generation may have rebuilt the VPP DNS adapters, which
	// invalidates the app index inside the session rules until the heal
	// re-registers them. Ask for that repair immediately instead of leaving
	// clients without DNS until the next timer tick.
	if result.Status == RuntimeStatusCommitted && result.RuntimeState == RuntimeStateRunning {
		server.writeRouteABFIntent()
		server.requestDNSPostApplyRepair()
	}
}

func incompleteRuntimeEvidenceReason(result RuntimeApplyResult, now time.Time) string {
	if gatewayErr := completeGatewayRuntimeEvidence(result, now); gatewayErr != nil {
		return "gateway runtime evidence is incomplete: " + gatewayErr.Error()
	}
	return runtimeEvidenceReason(result)
}

// statusHealthComponents is the restart-safe component inventory. It probes
// live service health without rebuilding the full gateway plan, so the first
// status request after a restart stays inside the plan budget.
func (server *Server) statusHealthComponents(ctx context.Context) []RuntimeComponentState {
	components := []RuntimeComponentState{}
	for _, item := range []struct {
		name           string
		service        serviceRuntime.ServiceName
		productService product.Service
	}{
		{"smartdns", serviceRuntime.SmartDNS, product.ServiceSmartDNS},
		{"dnsmasq", serviceRuntime.Dnsmasq, product.ServiceDnsmasq},
		{"xray", serviceRuntime.Xray, product.ServiceXray},
		{"pppoe", serviceRuntime.PPPoE, product.ServicePPPoE},
	} {
		// Never report a service the active product does not ship. Without
		// this filter the Orchestrator profile lists the Gateway-only
		// services as degraded and reports a healthy appliance as degraded.
		if !server.profile.AllowsService(item.productService) {
			continue
		}
		component := RuntimeComponentState{Name: item.name}
		if !server.serviceRuntimeConfigured() {
			component.State = "unavailable"
			component.Reason = "service runtime controller is not configured"
			components = append(components, component)
			continue
		}
		health, err := server.services.HealthCheck(ctx, item.service)
		if err != nil || len(health) == 0 || !health[0].Available {
			component.State = "degraded"
			if err != nil {
				component.Reason = err.Error()
			} else if len(health) > 0 {
				component.Reason = health[0].Reason
			}
		} else {
			component.State = "running"
			component.Available = true
		}
		components = append(components, component)
	}
	if server.serviceRuntimeConfigured() && server.profile.AllowsService(product.ServiceVPP) {
		health, err := server.services.HealthCheck(ctx, serviceRuntime.VPP)
		vpp := RuntimeComponentState{Name: "vpp"}
		if err == nil && len(health) > 0 && health[0].Available {
			vpp.State = "running"
			vpp.Available = true
			vpp.Reason = "VPP service is healthy"
		} else {
			vpp.State = "degraded"
			if err != nil {
				vpp.Reason = err.Error()
			} else if len(health) > 0 {
				vpp.Reason = health[0].Reason
			}
		}
		components = append(components, vpp)
	}
	// Proxy DNS endpoints are a dependency of SmartDNS, not of the whole
	// appliance. A closed endpoint or an unestablished egress is reported here
	// with its reason, so a proxied DNS rule can never succeed silently while
	// its resolver is unreachable.
	if plan, _, planErr := server.activeProxyDNSEndpoints(ctx, nil, nil); planErr == nil && !plan.Empty() {
		component := RuntimeComponentState{Name: "proxy_dns_endpoint", State: "running", Available: true}
		failures := make([]string, 0, len(plan.Problems)+len(plan.Endpoints))
		for _, problem := range plan.Problems {
			failures = append(failures, problem.Error())
		}
		for _, endpoint := range plan.Endpoints {
			probe := server.probeProxyDNSEndpoint(ctx, endpoint, 15*time.Second)
			if !probe.Healthy {
				failures = append(failures, fmt.Sprintf("%s (%s, %s) did not answer a real query: %s", endpoint.ID, endpoint.Listen(), endpoint.Protocol, probe.Reason))
			}
		}
		if len(failures) > 0 {
			component.State = "degraded"
			component.Available = false
			component.Reason = "proxy DNS endpoint: " + strings.Join(failures, "; ")
		} else {
			component.Reason = fmt.Sprintf("%d proxy DNS endpoint(s) answered a real query", len(plan.Endpoints))
		}
		components = append(components, component)
	}
	if server.store == nil {
		components = append(components, RuntimeComponentState{Name: "persistence", State: "unavailable", Reason: "local store is not configured"})
	} else {
		components = append(components, RuntimeComponentState{Name: "persistence", State: "running", Available: true})
	}
	if component, configured := server.mirrorRuntimeComponent(ctx); configured {
		components = append(components, component)
	}
	return components
}

// runtimeTransactionCommitted reports whether the last runtime transaction
// actually settled for the current desired generation. A component-level
// degradation (for example AF_PACKET compatibility) does not make the gateway
// resource graph less applied; resource readback must not remain
// desired_not_applied just because runtime_state is degraded. Failed,
// unreconciled and mismatched receipts still never promote anything.
func runtimeTransactionCommitted(result RuntimeApplyResult) bool {
	if result.TransactionID == "" || result.Receipt.TransactionID != result.TransactionID || result.Receipt.Status != apply.ReceiptApplied {
		return false
	}
	switch result.Status {
	case RuntimeStatusCommitted, "degraded":
		return true
	default:
		return false
	}
}

func (server *Server) runtimeEvidence() RuntimeApplyResult {
	server.runtimeMu.Lock()
	defer server.runtimeMu.Unlock()
	if server.lastRuntime != nil {
		return *server.lastRuntime
	}
	now := server.now().UTC()
	return RuntimeApplyResult{
		Status:        "unreconciled",
		RuntimeState:  "degraded",
		TransactionID: "unreconciled",
		Reason:        "no complete runtime apply receipt or readback",
		Receipt: apply.ApplyReceipt{
			TransactionID: "unreconciled",
			Status:        apply.ReceiptMissing,
			AppliedAt:     now,
		},
		Readback: apply.Readback{
			TransactionID: "unreconciled",
			Timestamp:     now,
			Fresh:         false,
			Reason:        "no complete runtime apply receipt or readback",
		},
		ReconciliationReceipt: apply.ReconciliationReceipt{TransactionID: "unreconciled", Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: "no persisted runtime evidence", Timestamp: now},
		AppliedAt:             now,
	}
}

func (server *Server) applyRuntimeEvidence(components []RuntimeComponentState) []RuntimeComponentState {
	return server.applyRuntimeEvidenceForResult(components, server.runtimeEvidence())
}

func (server *Server) applyRuntimeEvidenceForResult(components []RuntimeComponentState, result RuntimeApplyResult) []RuntimeComponentState {
	applied := runtimeAppliedServices(result)
	now := server.now().UTC()
	commitComplete := completeRuntimeEvidence(result, runtimeCommitValidationTime(result))
	currentlyFresh := completeRuntimeEvidence(result, now)
	for index := range components {
		component := &components[index]
		if service := runtimeServiceForComponent(component.Name); service != "" && !applied[service] {
			// This transaction did not execute the component. A successful
			// live health probe must not be turned degraded by an older
			// transaction's service receipt.
			if component.State == "running" {
				component.Available = true
				component.Unchanged = true
				component.Fresh = true
				continue
			}
		}
		if component.Unchanged {
			// An unchanged component was not executed by this transaction.
			// Its live health is the evidence; an old apply receipt must not
			// degrade it. Keep the last successful transaction attached by
			// attachServiceEvidence.
			if component.State == "running" {
				component.Available = true
				component.Fresh = true
			}
			continue
		}
		capability := component.Name
		component.TransactionID = result.TransactionID
		component.Capability = capability
		component.ApplyReceipt = result.Receipt
		component.ApplyReceipt.Capability = capability
		component.ReadbackAt = result.Readback.Timestamp
		component.Fresh = currentlyFresh
		if component.State == "running" && !commitComplete {
			component.State = "degraded"
			component.Available = false
			component.Reason = runtimeEvidenceReason(result)
		} else if component.Reason == "" && commitComplete {
			component.Reason = "apply receipt verified; live component health verified"
		}
	}
	return components
}

func runtimeCommitValidationTime(result RuntimeApplyResult) time.Time {
	reference := result.Readback.Timestamp
	if result.AppliedAt.After(reference) {
		reference = result.AppliedAt
	}
	for _, evidence := range result.GatewayEvidence {
		for _, candidate := range []time.Time{evidence.ApplyReceipt.AppliedAt, evidence.Readback.Timestamp, evidence.Before.ReadbackAt, evidence.After.ReadbackAt} {
			if candidate.After(reference) {
				reference = candidate
			}
		}
	}
	if reference.IsZero() {
		return time.Now().UTC()
	}
	return reference
}

func completeRuntimeEvidence(result RuntimeApplyResult, now time.Time) bool {
	if result.Receipt.Status != apply.ReceiptApplied || result.TransactionID == "" || result.Receipt.TransactionID != result.TransactionID || result.Receipt.Capability == "" || result.Readback.Capability != result.Receipt.Capability || result.Receipt.AppliedAt.IsZero() || result.Receipt.AppliedAt.After(result.Readback.Timestamp) {
		return false
	}
	if !result.Readback.Fresh || result.Readback.TransactionID != result.TransactionID || result.Readback.Timestamp.IsZero() || now.Sub(result.Readback.Timestamp) < 0 || now.Sub(result.Readback.Timestamp) > apply.ReadbackFreshnessWindow {
		return false
	}
	return completeGatewayRuntimeEvidence(result, now) == nil
}

func runtimeEvidenceReason(result RuntimeApplyResult) string {
	if strings.TrimSpace(result.Reason) != "" {
		return result.Reason
	}
	if result.Receipt.Status != apply.ReceiptApplied {
		return "runtime apply receipt is incomplete"
	}
	if result.Readback.Capability != result.Receipt.Capability {
		return "runtime receipt/readback capability mismatch"
	}
	if !result.Readback.Fresh {
		if strings.TrimSpace(result.Readback.Reason) != "" {
			return result.Readback.Reason
		}
		return "runtime readback is stale"
	}
	return "runtime apply receipt/readback chain is incomplete"
}

func (server *Server) reconcileRuntime(ctx context.Context) {
	if server.store == nil {
		return
	}
	snapshots, err := server.store.GenerationSnapshotsLimit(ctx, 50)
	if err != nil || len(snapshots) == 0 {
		return
	}
	snapshot := snapshots[0]
	if err := persistence.VerifyPayload(snapshot.Payload, snapshot.PayloadHash); err != nil {
		server.setRuntimeEvidence(degradedRuntimeResult(snapshot.SourceTransactionID, fmt.Sprintf("runtime snapshot integrity failed: %v", err), server.now().UTC()))
		return
	}
	now := server.now().UTC()
	result, err := runtimeResultFromSnapshot(snapshot)
	if err != nil {
		degraded := degradedRuntimeResult(snapshot.SourceTransactionID, fmt.Sprintf("incomplete persisted runtime evidence: %v", err), now)
		server.setRuntimeEvidence(degraded)
		return
	}
	// A committed snapshot is evidence for the desired state that produced it,
	// not for whatever is in the document store now. Detect both a desired
	// generation change and a live dataplane drift at startup so /status stops
	// republishing an old committed generation after an operator edit.
	if persistedSource := strings.TrimSpace(result.SourceHash); persistedSource != "" {
		if currentSource, hashErr := server.runtimeDesiredSourceHash(ctx); hashErr == nil && currentSource != "" && currentSource != persistedSource {
			reason := "desired configuration changed since the last committed generation; apply must recompute the plan"
			server.publishReconciliationNotice(snapshot.SourceTransactionID, reason, currentSource, now, result)
			return
		}
		if driftErr := server.verifyCommittedGenerationLive(ctx, result); driftErr != nil {
			reason := "live dataplane drift from the last committed generation: " + driftErr.Error()
			server.publishReconciliationNotice(snapshot.SourceTransactionID, reason, persistedSource, now, result)
			return
		}
	}
	// The persisted receipt and typed readback are immutable commit evidence:
	// their timestamps record when the generation was applied, not a lease on
	// liveness. Validate them at the generation's own commit time so a restart
	// does not turn a legitimately committed generation into drift. Live
	// liveness is established separately from the service runtime health
	// probes, which is why an unverifiable host still fails closed below.
	commitTime := runtimeCommitValidationTime(result)
	if commitTime.After(now.Add(time.Minute)) {
		reason := "persisted runtime evidence has a future timestamp"
		degraded := degradedRuntimeResult(snapshot.SourceTransactionID, reason, now)
		degraded.CapabilityFailures = append([]apply.CapabilityFailureEvidence(nil), result.CapabilityFailures...)
		degraded.ReconciliationReceipt = apply.ReconciliationReceipt{TransactionID: snapshot.SourceTransactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: reason, Timestamp: now}
		server.setRuntimeEvidence(degraded)
		return
	}
	if commitComplete := completeRuntimeEvidence(result, commitTime); !commitComplete {
		reason := "incomplete persisted runtime evidence"
		if !result.Readback.Timestamp.IsZero() && commitTime.Sub(result.Readback.Timestamp) > apply.ReadbackFreshnessWindow {
			reason = "stale persisted runtime readback"
		} else if gatewayErr := completeGatewayRuntimeEvidence(result, commitTime); gatewayErr != nil {
			reason = fmt.Sprintf("incomplete persisted gateway resource evidence: %v", gatewayErr)
		}
		degraded := degradedRuntimeResult(snapshot.SourceTransactionID, reason, now)
		degraded.CapabilityFailures = append([]apply.CapabilityFailureEvidence(nil), result.CapabilityFailures...)
		degraded.ReconciliationReceipt = apply.ReconciliationReceipt{TransactionID: snapshot.SourceTransactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: reason, Timestamp: now}
		server.setRuntimeEvidence(degraded)
		return
	}
	cause := "persisted runtime evidence reconciled"
	if !completeRuntimeEvidence(result, now) {
		if !server.serviceRuntimeConfigured() {
			// Without a live service-runtime probe there is no way to prove the
			// generation is still running; keep the stale-readback failure
			// instead of reporting an unverified running state.
			reason := "stale persisted runtime readback"
			degraded := degradedRuntimeResult(snapshot.SourceTransactionID, reason, now)
			degraded.CapabilityFailures = append([]apply.CapabilityFailureEvidence(nil), result.CapabilityFailures...)
			degraded.ReconciliationReceipt = apply.ReconciliationReceipt{TransactionID: snapshot.SourceTransactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: reason, Timestamp: now}
			server.setRuntimeEvidence(degraded)
			return
		}
		// The commit is structurally complete and every configured component is
		// probed live by the status path. Report the readback as not currently
		// fresh (components carry Fresh=false) while keeping the committed
		// generation authoritative.
		cause = "persisted runtime commit evidence verified; live component health probed"
	}
	result.ReconciliationReceipt = apply.ReconciliationReceipt{TransactionID: result.TransactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptApplied, Cause: cause, Timestamp: now}
	server.clearReconciliationNotice()
	server.setRuntimeEvidence(result)
}

// publishReconciliationNotice keeps the committed generation as last_apply and
// records startup drift as display state only. The next apply always rebuilds
// from desired; a reconcile notice must never become the apply result or a
// terminal "reconciled_degraded" generation.
func (server *Server) publishReconciliationNotice(transactionID, reason, sourceHash string, now time.Time, committed RuntimeApplyResult) {
	notice := degradedRuntimeResult(transactionID, reason, now)
	notice.SourceHash = sourceHash
	notice.Recomputed = true
	notice.Components = []RuntimeComponentState{{Name: "generation", State: "degraded", Available: false, TransactionID: transactionID, Reason: reason}}
	notice.CapabilityFailures = append([]apply.CapabilityFailureEvidence(nil), committed.CapabilityFailures...)
	notice.ReconciliationReceipt = apply.ReconciliationReceipt{TransactionID: transactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: reason, Timestamp: now}
	server.runtimeMu.Lock()
	server.lastReconcile = &notice
	// Keep the valid committed snapshot authoritative. Only a generation that
	// has no usable commit evidence may be replaced by the degraded notice.
	if !applyResultCommitted(committed) || !completeRuntimeEvidence(committed, runtimeCommitValidationTime(committed)) {
		server.lastRuntime = &notice
	} else if server.lastRuntime == nil || !applyResultCommitted(*server.lastRuntime) || !committed.AppliedAt.Before(server.lastRuntime.AppliedAt) {
		copied := committed
		server.lastRuntime = &copied
	}
	server.runtimeMu.Unlock()
}

func (server *Server) lastReconciliationNotice() *RuntimeApplyResult {
	server.runtimeMu.Lock()
	defer server.runtimeMu.Unlock()
	if server.lastReconcile == nil {
		return nil
	}
	notice := *server.lastReconcile
	return &notice
}

func (server *Server) clearReconciliationNotice() {
	server.runtimeMu.Lock()
	server.lastReconcile = nil
	server.runtimeMu.Unlock()
}

// runtimeAttemptNotice returns the most recent non-committed attempt that must
// be shown as the current generation status. A failed apply takes precedence
// over a startup reconcile notice, but never replaces last_apply.
func (server *Server) runtimeAttemptNotice(committed RuntimeApplyResult) *RuntimeApplyResult {
	failed := server.lastFailedRuntimeApply()
	reconcile := server.lastReconciliationNotice()
	switch {
	case failed != nil && failed.AppliedAt.After(committed.AppliedAt):
		return failed
	case reconcile != nil && (failed == nil || !reconcile.AppliedAt.Before(failed.AppliedAt)):
		return reconcile
	case failed != nil && !failed.AppliedAt.Before(committed.AppliedAt):
		return failed
	default:
		return nil
	}
}

func runtimeComponentsWithAttempt(components []RuntimeComponentState, notice *RuntimeApplyResult) []RuntimeComponentState {
	if notice == nil {
		return components
	}
	updated := make([]RuntimeComponentState, 0, len(components)+1)
	updated = append(updated, RuntimeComponentState{
		Name:          "generation",
		State:         "degraded",
		Available:     false,
		TransactionID: notice.TransactionID,
		Reason:        notice.Reason,
	})
	for _, component := range components {
		if component.Name == "generation" {
			continue
		}
		updated = append(updated, component)
	}
	return updated
}

func runtimeResultFromSnapshot(snapshot persistence.RuntimeSnapshot) (RuntimeApplyResult, error) {
	var result RuntimeApplyResult
	if err := json.Unmarshal(snapshot.Payload, &result); err != nil {
		return RuntimeApplyResult{}, err
	}
	if result.TransactionID != "" && result.Receipt.TransactionID != "" {
		return result, nil
	}
	var payload apply.SnapshotPayload
	if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
		return RuntimeApplyResult{}, err
	}
	if payload.Receipt.TransactionID == "" || payload.Readback.TransactionID == "" {
		return RuntimeApplyResult{}, fmt.Errorf("snapshot has no receipt/readback envelope")
	}
	return RuntimeApplyResult{
		Status:             "committed",
		RuntimeState:       "running",
		TransactionID:      snapshot.SourceTransactionID,
		Receipt:            payload.Receipt,
		Readback:           payload.Readback,
		GatewayPlan:        payload.GatewayPlan,
		SnapshotHash:       payload.GenerationHash,
		GatewayEvidence:    payload.GatewayEvidence,
		CapabilityFailures: payload.CapabilityFailures,
		SelfHealed:         payload.SelfHealed,
		SourceHash:         payload.SourceHash,
		ArtifactHashes:     payload.ArtifactHashes,
		AppliedAt:          payload.Receipt.AppliedAt,
	}, nil
}

func degradedRuntimeResult(transactionID, reason string, now time.Time) RuntimeApplyResult {
	if transactionID == "" {
		transactionID = "reconcile"
	}
	return RuntimeApplyResult{
		Status:                "reconciled_degraded",
		RuntimeState:          "degraded",
		TransactionID:         transactionID,
		Reason:                reason,
		Receipt:               apply.ApplyReceipt{TransactionID: transactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptFailed, AppliedAt: now, Cause: reason},
		Readback:              apply.Readback{TransactionID: transactionID, Timestamp: now, Fresh: false, Reason: reason},
		ReconciliationReceipt: apply.ReconciliationReceipt{TransactionID: transactionID, Capability: "runtime.reconciliation", Status: apply.ReceiptDegraded, Cause: reason, Timestamp: now},
		AppliedAt:             now,
	}
}

func (server *Server) runtimeReceipt(ctx context.Context, request RuntimeEvidenceRequest) (apply.ApplyReceipt, error) {
	if server.services == nil || server.services.Controller == nil {
		return apply.ApplyReceipt{}, fmt.Errorf("runtime apply receipt provider is not configured: %w", apply.ErrIncompleteEvidence)
	}
	provider, ok := server.services.Controller.(RuntimeEvidenceProvider)
	if !ok {
		return apply.ApplyReceipt{}, fmt.Errorf("runtime apply receipt provider is not configured: %w", apply.ErrIncompleteEvidence)
	}
	return provider.Receipt(ctx, request)
}

func (server *Server) runtimeReadback(ctx context.Context, request RuntimeEvidenceRequest) (apply.Readback, error) {
	if server.services == nil || server.services.Controller == nil {
		return apply.Readback{}, fmt.Errorf("runtime readback provider is not configured: %w", apply.ErrIncompleteEvidence)
	}
	provider, ok := server.services.Controller.(RuntimeEvidenceProvider)
	if !ok {
		return apply.Readback{}, fmt.Errorf("runtime readback provider is not configured: %w", apply.ErrIncompleteEvidence)
	}
	return provider.Readback(ctx, request)
}

func (server *Server) latestRuntimeSnapshotID(ctx context.Context) string {
	if server.store == nil {
		return ""
	}
	snapshots, err := server.store.GenerationSnapshotsLimit(ctx, 50)
	if err != nil || len(snapshots) == 0 {
		return ""
	}
	// A request that was interrupted after the gateway phase can leave a
	// snapshot whose service commit happened long after the typed VPP evidence
	// was collected.  It must not become the rollback target for every later
	// request.  Walk back to the newest structurally valid generation instead
	// of allowing one incomplete snapshot to brick the apply path.
	for _, snapshot := range snapshots {
		if runtimeSnapshotRollbackValid(snapshot, server.now().UTC()) {
			return snapshot.ID
		}
	}
	return ""
}

func runtimeSnapshotRollbackValid(snapshot persistence.RuntimeSnapshot, now time.Time) bool {
	if err := persistence.VerifyPayload(snapshot.Payload, snapshot.PayloadHash); err != nil {
		return false
	}
	var payload apply.SnapshotPayload
	if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
		return false
	}
	if payload.Receipt.TransactionID == "" || payload.Readback.TransactionID == "" {
		return false
	}
	if payload.Receipt.TransactionID != snapshot.SourceTransactionID || payload.Readback.TransactionID != snapshot.SourceTransactionID {
		return false
	}
	if len(payload.GatewayEvidence) == 0 {
		return true
	}
	validationTime := snapshot.CreatedAt
	if validationTime.IsZero() || validationTime.After(now) {
		validationTime = now
	}
	return apply.ValidateGatewayEvidence(payload.GatewayEvidence, snapshot.SourceTransactionID, validationTime) == nil
}
