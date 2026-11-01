package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/applystate"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

// applyStateEnabled reports whether transactional generation tracking is
// configured. Tests and profiles without a durable state path keep the legacy
// result shape; the gateway entrypoint always enables it.
func (server *Server) applyStateEnabled() bool {
	return server != nil && server.applyGenerationState != nil && server.applyGenerationState.Enabled()
}

func (server *Server) beginRuntimeApplyGeneration(desiredHash, transactionID string) (*applystate.BeginResult, error) {
	if !server.applyStateEnabled() {
		return nil, nil
	}
	begin, err := server.applyGenerationState.Begin(desiredHash)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(server.applyIdentityPath) != "" {
		identity := applystate.Identity{
			TransactionID:     transactionID,
			DesiredGeneration: begin.State.DesiredGeneration,
			DesiredHash:       desiredHash,
			CreatedAt:         server.now().UTC(),
		}
		if err := applystate.WriteIdentity(server.applyIdentityPath, identity); err != nil {
			return nil, fmt.Errorf("publish apply identity: %w", err)
		}
	}
	return &begin, nil
}

func mergeApplyStateFields(response map[string]any, state applystate.State) {
	response["desired_generation"] = state.DesiredGeneration
	response["applied_generation"] = state.AppliedGeneration
	response["current_generation"] = state.CurrentGeneration
	response["desired_hash"] = state.DesiredHash
	response["applied_hash"] = state.AppliedDesiredHash
	response["current_hash"] = state.CurrentHash
	response["partial"] = state.Partial
	response["degraded_reasons"] = state.DegradedReasons
	response["apply_state_status"] = state.Status
	response["receipt_status"] = state.ReceiptStatusView(time.Now().UTC(), apply.ReadbackFreshnessWindow)
	if state.LastError != nil {
		response["last_error"] = state.LastError
	}
}

// runtimeTransactionReceipt is the control-plane receipt for this transaction.
//
// When an external receipt path is configured (for example a vpp-apply receipt)
// it is read and authoritative: a missing, malformed or failed receipt makes
// the apply fail. Otherwise the typed transaction publishes its own receipt
// and reads the same file back, so the state store never trusts in-memory
// success alone.
func (server *Server) runtimeTransactionReceipt(transactionID, sourceHash string, generation int64, execution apply.Result, applied []serviceRuntime.RenderedArtifact, failures []apply.CapabilityFailureEvidence, degraded bool, requireVPPReceipt bool, receiptSince time.Time) (applystate.Receipt, error) {
	if strings.TrimSpace(server.applyReceiptPath) != "" && server.applyReceiptReadOnly {
		return applystate.LoadReceipt(server.applyReceiptPath)
	}
	if requireVPPReceipt {
		// A non-persist-only VPP artifact executed the helper. Its receipt is
		// the only proof that this transaction's generation converged; never
		// substitute a synthetic success receipt.
		return applystate.LoadReceipt(server.vppReceiptPath)
	}
	if vppApplyReceiptFreshSince(server.vppReceiptPath, receiptSince) {
		// A vpp-apply ran during this apply even though the typed transaction
		// persisted the replay plan only (for example a helper side effect or a
		// concurrent boot/repair path). Its receipt is still authoritative: an
		// unbound transaction id or generation zero must fail this apply rather
		// than be hidden by a synthetic success receipt.
		return applystate.LoadReceipt(server.vppReceiptPath)
	}
	receipt := buildRuntimeTransactionReceipt(transactionID, sourceHash, generation, execution, applied, failures, degraded)
	if strings.TrimSpace(server.applyReceiptPath) != "" {
		if err := applystate.WriteReceipt(server.applyReceiptPath, receipt); err != nil {
			return applystate.Receipt{}, fmt.Errorf("write apply receipt: %w", err)
		}
		persisted, err := applystate.LoadReceipt(server.applyReceiptPath)
		if err != nil {
			return applystate.Receipt{}, err
		}
		return persisted, nil
	}
	return receipt, nil
}

func buildRuntimeTransactionReceipt(transactionID, sourceHash string, generation int64, execution apply.Result, applied []serviceRuntime.RenderedArtifact, failures []apply.CapabilityFailureEvidence, degraded bool) applystate.Receipt {
	receipt := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     transactionID,
		Status:            applystate.StatusApplied,
		DesiredGeneration: generation,
		AppliedGeneration: generation,
		DesiredHash:       sourceHash,
	}
	seen := map[string]bool{}
	addStep := func(command, status, reason string) {
		command = strings.TrimSpace(command)
		if command == "" || seen[command] {
			return
		}
		seen[command] = true
		receipt.Steps = append(receipt.Steps, applystate.Step{
			Command:      command,
			Status:       status,
			Attempts:     1,
			Reason:       reason,
			Compensation: &applystate.Compensation{Status: "not-required"},
		})
	}
	for _, resource := range execution.GatewayResult.Receipts {
		addStep("resource:"+resource.Capability, "applied", "")
	}
	for _, evidence := range execution.GatewayResult.Evidence {
		addStep("resource:"+evidence.Resource, "applied", "")
	}
	for _, artifact := range applied {
		addStep("artifact:"+artifact.Path, "applied", "")
	}
	if len(receipt.Steps) == 0 {
		// An apply with no rendered delta still has a transaction identity.
		// The step exists so "receipt without steps" can never mean success.
		addStep("transaction:commit", "applied", "")
	}
	reasons := make([]string, 0, len(failures)+1)
	for _, failure := range failures {
		reason := strings.TrimSpace(failure.Capability)
		if detail := strings.TrimSpace(failure.Reason); detail != "" {
			reason += ": " + detail
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	// degraded reports that some component was not "running", but that alone is
	// not evidence of a partial transaction: unsupported optional capabilities
	// (for example QoS without the plugin) are normal. Only the named capability
	// failures collected above make the transaction partial.
	if len(reasons) > 0 {
		receipt.Partial = true
		receipt.DegradedReasons = reasons
	}
	return receipt
}

// legacyGenericDegradedReasons are placeholder strings written by older
// builds. They name no component or step, so they are never a concrete cause.
var legacyGenericDegradedReasons = map[string]bool{
	"runtime apply completed with degraded or failed steps": true,
	"runtime apply completed with degraded components":      true,
}

// runtimeConcreteDegradedReasons derives the partial flag from evidence an
// operator can act on: a named capability failure, a component that is really
// degraded/failed/unavailable, or a failed/needs-repair step. Optional
// capabilities that are merely unsupported are not degradation.
func runtimeConcreteDegradedReasons(components []RuntimeComponentState, steps []applystate.Step, failures []apply.CapabilityFailureEvidence) []string {
	reasons := make([]string, 0, len(failures)+len(components)+len(steps))
	seen := map[string]bool{}
	add := func(reason string) {
		reason = strings.TrimSpace(reason)
		if reason == "" || seen[reason] || legacyGenericDegradedReasons[reason] {
			return
		}
		// A receipt-partial reason describes the *previous* transaction's
		// helper receipt, which this transaction is about to rewrite. Copying
		// it forward kept every later receipt partial.
		if strings.Contains(reason, "VPP apply receipt is partial") {
			return
		}
		seen[reason] = true
		reasons = append(reasons, reason)
	}
	for _, failure := range failures {
		reason := strings.TrimSpace(failure.Capability)
		if detail := strings.TrimSpace(failure.Reason); detail != "" {
			if reason != "" {
				reason += ": "
			}
			reason += detail
		}
		add(reason)
	}
	for _, component := range components {
		switch strings.TrimSpace(component.State) {
		case "degraded", "failed", "unavailable":
			reason := strings.TrimSpace(component.Name)
			if detail := strings.TrimSpace(component.Reason); detail != "" {
				if reason != "" {
					reason += ": "
				}
				reason += detail
			}
			add(reason)
		}
	}
	for _, step := range steps {
		switch strings.TrimSpace(step.Status) {
		case "failed", "needs-repair", "needs_repair", "ignored-failure":
			reason := strings.TrimSpace(step.Command)
			if detail := firstLine(step.Reason, 240); detail != "" {
				if reason != "" {
					reason += ": "
				}
				reason += detail
			} else if reason == "" {
				reason = "step " + strings.TrimSpace(step.Status)
			}
			add(reason)
		}
	}
	return reasons
}

// commitRuntimeApplyState records the applied generation. It returns an error
// when the receipt is missing/unparseable or does not prove convergence. In
// that case the caller must report failure; the previous applied generation
// stays authoritative.
func (server *Server) commitRuntimeApplyState(transactionID, sourceHash string, generation *applystate.BeginResult, execution apply.Result, applied []serviceRuntime.RenderedArtifact, failures []apply.CapabilityFailureEvidence, degraded bool) (applystate.State, error) {
	return server.commitRuntimeApplyStateWithReceipt(transactionID, sourceHash, generation, execution, applied, failures, degraded, nil)
}

// commitRuntimeApplyStateWithReceipt lets the Go execution path publish the
// receipt it actually produced. A nil receipt keeps the legacy/vpp-apply
// fallback, which is only valid when the helper really ran.
func (server *Server) commitRuntimeApplyStateWithReceipt(transactionID, sourceHash string, generation *applystate.BeginResult, execution apply.Result, applied []serviceRuntime.RenderedArtifact, failures []apply.CapabilityFailureEvidence, degraded bool, goReceipt *applystate.Receipt) (applystate.State, error) {
	if !server.applyStateEnabled() || generation == nil {
		return applystate.State{}, nil
	}
	var receipt applystate.Receipt
	if goReceipt != nil {
		receipt = *goReceipt
	} else {
		var err error
		receipt, err = server.runtimeTransactionReceipt(transactionID, sourceHash, generation.State.DesiredGeneration, execution, applied, failures, degraded, vppHelperReceiptRequired(applied), generation.State.UpdatedAt)
		if err != nil {
			return applystate.State{}, err
		}
	}
	state, err := server.applyGenerationState.CommitWithReadback(transactionID, receipt, execution.Readback.Timestamp)
	if err != nil {
		return applystate.State{}, err
	}
	// Keep the identity until the next Begin. A VPP restart or boot replay
	// that starts just after commit must still echo the committed transaction;
	// applystate.Identity TTL bounds how long that binding can be reused.
	return state, nil
}

// vppApplyReceiptFreshSince reports whether a helper receipt was written at or
// after this apply started. A stale receipt from a previous generation must not
// influence the current transaction; a fresh one must.
func vppApplyReceiptFreshSince(path string, since time.Time) bool {
	path = strings.TrimSpace(path)
	if path == "" || since.IsZero() {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.ModTime().Before(since)
}

// vppHelperReceiptRequired reports whether this apply actually executed the
// VPP helper (rather than persisting the replay plan only). Only then is the
// helper receipt authoritative for the transaction.
func vppHelperReceiptRequired(applied []serviceRuntime.RenderedArtifact) bool {
	for _, artifact := range applied {
		if artifact.Service != serviceRuntime.VPP {
			continue
		}
		if strings.TrimSpace(artifact.ReloadMode) != serviceRuntime.ReloadModePersistOnly {
			return true
		}
	}
	return false
}

func (server *Server) failRuntimeApplyState(transactionID, reason, receiptStatus string) applystate.State {
	if !server.applyStateEnabled() {
		return applystate.State{}
	}
	state, err := server.applyGenerationState.Fail(applystate.Failure{
		TransactionID: transactionID,
		Reason:        reason,
		RecordStatus:  receiptStatus,
	})
	if err != nil {
		return applystate.State{}
	}
	server.clearRuntimeApplyIdentity()
	return state
}

func (server *Server) clearRuntimeApplyIdentity() {
	if server == nil || strings.TrimSpace(server.applyIdentityPath) == "" {
		return
	}
	_ = applystate.RemoveIdentity(server.applyIdentityPath)
}

func applyReceiptFailureStatus(err error) string {
	switch {
	case errors.Is(err, applystate.ErrReceiptMissing):
		return applystate.ReceiptStatusMissing
	case errors.Is(err, applystate.ErrReceiptInvalid):
		return applystate.ReceiptStatusInvalid
	default:
		return applystate.ReceiptStatusFailed
	}
}

// handleRuntimeApplyState exposes the durable generation document. "current"
// names the generation the dataplane is proven to run; it is not derived from
// process liveness.
func (server *Server) handleRuntimeApplyState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if !server.applyStateEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":        false,
			"status":         "unconfigured",
			"schema_version": applystate.SchemaVersion,
			"reason":         "apply state store is not configured",
			"request_id":     requestID(r),
		})
		return
	}
	state, err := server.applyGenerationState.Snapshot()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "apply_state_unavailable", err.Error())
		return
	}
	response := applyStateResponse(state, requestID(r), server.now().UTC())
	response["vpp_receipt"] = server.vppReceiptState(state)
	writeJSON(w, http.StatusOK, response)
}

// vppReceiptState exposes the last helper receipt next to the durable state.
// It does not rewrite the state; it makes a mismatch visible to the operator
// (and to read-only VM acceptance) instead of leaving a stale applied/number
// looking authoritative.
func (server *Server) vppReceiptState(state applystate.State) map[string]any {
	path := strings.TrimSpace(server.vppReceiptPath)
	if path == "" {
		return map[string]any{"status": "unconfigured"}
	}
	receipt, err := applystate.LoadReceipt(path)
	if err != nil {
		return map[string]any{"status": "missing", "path": path, "error": err.Error()}
	}
	matches := state.TransactionID != "" &&
		receipt.TransactionID == state.TransactionID &&
		receipt.DesiredGeneration == state.AppliedGeneration &&
		receipt.AppliedGeneration == state.AppliedGeneration &&
		receipt.DesiredHash == state.AppliedDesiredHash
	return map[string]any{
		"status":                  "present",
		"path":                    path,
		"transaction_id":          receipt.TransactionID,
		"executor_transaction_id": receipt.ExecutorTransactionID,
		"receipt_status":          receipt.Status,
		"desired_generation":      receipt.DesiredGeneration,
		"applied_generation":      receipt.AppliedGeneration,
		"desired_hash":            receipt.DesiredHash,
		"partial":                 receipt.Partial,
		"degraded_reasons":        receipt.DegradedReasons,
		"matches_state":           matches,
	}
}

func applyStateResponse(state applystate.State, requestIDValue string, now time.Time) map[string]any {
	verdict := state.ReceiptStatusView(now, apply.ReadbackFreshnessWindow)
	readbackAgeSeconds := int64(0)
	if !state.ReadbackAt.IsZero() {
		readbackAgeSeconds = int64(now.Sub(state.ReadbackAt).Seconds())
	}
	response := map[string]any{
		"enabled":                           true,
		"schema_version":                    state.SchemaVersion,
		"desired_generation":                state.DesiredGeneration,
		"desired_hash":                      state.DesiredHash,
		"applied_generation":                state.AppliedGeneration,
		"applied_hash":                      state.AppliedDesiredHash,
		"current_generation":                state.CurrentGeneration,
		"current_hash":                      state.CurrentHash,
		"transaction_id":                    state.TransactionID,
		"status":                            state.Status,
		"receipt_status":                    verdict,
		"receipt_status_raw":                state.ReceiptStatus,
		"partial":                           state.Partial,
		"degraded_reasons":                  state.DegradedReasons,
		"readback_at":                       state.ReadbackAt,
		"readback_age_seconds":              readbackAgeSeconds,
		"readback_freshness_window_seconds": int64(apply.ReadbackFreshnessWindow.Seconds()),
		"updated_at":                        state.UpdatedAt,
		"request_id":                        requestIDValue,
	}
	response["desired"] = map[string]any{"generation": state.DesiredGeneration, "hash": state.DesiredHash}
	response["applied"] = map[string]any{
		"generation": state.AppliedGeneration, "hash": state.AppliedDesiredHash,
		"transaction_id": state.TransactionID,
	}
	response["current"] = map[string]any{
		"generation": state.CurrentGeneration, "hash": state.CurrentHash,
		"status": state.Status,
	}
	response["transaction"] = map[string]any{
		"id": state.TransactionID, "status": state.ReceiptStatus, "receipt_status": verdict,
		"partial": state.Partial, "degraded_reasons": state.DegradedReasons,
	}
	if state.LastError != nil {
		response["last_error"] = state.LastError
	} else {
		response["last_error"] = nil
	}
	return response
}

// applyStateReceiptPath returns the default audit receipt next to the state
// document, so a state commit always has a rereadable receipt.
func applyStateIdentityPath(statePath string) string {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "apply-identity.json")
}

func applyStateReceiptPath(statePath string) string {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "apply-receipt.json")
}
