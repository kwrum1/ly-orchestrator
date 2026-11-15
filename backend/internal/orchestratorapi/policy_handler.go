package orchestratorapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

type policyCompileRequest struct {
	Flow    orchestrator.FlowInput    `json:"flow"`
	Prelude orchestrator.PreludeInput `json:"prelude"`
}

type serviceChainCompileRequest struct {
	Flow     orchestrator.FlowInput                  `json:"flow"`
	Prelude  orchestrator.PreludeInput               `json:"prelude"`
	Bindings []orchestrator.ServiceChainBindingInput `json:"bindings"`
}

type serviceChainApplyRequest struct {
	Flow        orchestrator.FlowInput                  `json:"flow"`
	Prelude     orchestrator.PreludeInput               `json:"prelude"`
	Bindings    []orchestrator.ServiceChainBindingInput `json:"bindings"`
	Attachments []vpp.NativeAttachment                  `json:"attachments"`
}

func (handler *Handler) policyRepository() (orchestrator.PolicyRepository, bool) {
	repository, ok := handler.repository.(orchestrator.PolicyRepository)
	return repository, ok
}

func (handler *Handler) handlePolicy(writer http.ResponseWriter, request *http.Request) {
	permission := PermissionRead
	if request.Method == http.MethodPut || request.Method == http.MethodDelete {
		permission = PermissionAdminWrite
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPut && request.Method != http.MethodDelete {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, permission) {
		return
	}
	repository, ok := handler.policyRepository()
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, orchestrator.ErrRepositoryUnavailable)
		return
	}
	switch request.Method {
	case http.MethodGet:
		policy, checksum, err := repository.PolicySnapshot(request.Context())
		if err != nil {
			writePolicyRepositoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"item": policy.View(), "checksum": checksum})
	case http.MethodPut:
		body, bodyErr := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
		if bodyErr != nil {
			writeAPIError(writer, http.StatusBadRequest, bodyErr)
			return
		}
		if len(body) > maxBodyBytes {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("request body is too large"))
			return
		}
		// Validate before accepting an asynchronous transaction: a malformed
		// document is still a 400, never a 202 that fails in the background.
		if err := decodeStrictJSON(writer, requestWithBody(request, body), &orchestrator.PolicyInput{}); err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		if wantsAsyncPolicy(request) {
			transaction, beginErr := handler.beginPolicyTransaction()
			if beginErr != nil {
				writeAPIError(writer, http.StatusConflict, beginErr)
				return
			}
			writeJSON(writer, http.StatusAccepted, map[string]any{
				"transaction_id": transaction.ID,
				"status":         transaction.Status,
			})
			worker := requestWithBody(request.Clone(context.Background()), body)
			go handler.runAsyncPolicyPut(transaction.ID, worker)
			return
		}
		handler.servePolicyPut(writer, requestWithBody(request, body))
	case http.MethodDelete:
		topology, _, topologyErr := handler.repository.Snapshot(request.Context())
		if topologyErr != nil {
			writeRepositoryError(writer, topologyErr)
			return
		}
		previous, _, previousErr := repository.PolicySnapshot(request.Context())
		if previousErr != nil {
			writePolicyRepositoryError(writer, previousErr)
			return
		}
		if err := handler.applyTransparent(request.Context(), topology, nil); err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		if err := repository.DeletePolicy(request.Context()); err != nil {
			if rollbackErr := handler.rollbackTransparent(request.Context(), topology, &previous); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("delete policy: %v; restore transparent dataplane: %w", err, rollbackErr))
				return
			}
			writePolicyRepositoryError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !handler.requireConverged(writer, request.Context(), topology, nil) {
			return
		}
		handler.recordAudit(request, PolicyPath, "delete", "success", "")
		writer.WriteHeader(http.StatusNoContent)
	}
}

// servePolicyPut is the synchronous policy write: VPP apply, durable
// persistence, transaction journal commit and convergence verification. The
// asynchronous worker reuses it verbatim and reports its outcome through
// policy/status.
func (handler *Handler) servePolicyPut(writer http.ResponseWriter, request *http.Request) {
	trace := policyTraceFrom(request.Context())
	trace.mark("decode")
	repository, ok := handler.policyRepository()
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, orchestrator.ErrRepositoryUnavailable)
		return
	}
	var input orchestrator.PolicyInput
	if err := decodeStrictJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	topology, _, err := handler.repository.Snapshot(request.Context())
	if err != nil {
		writeRepositoryError(writer, err)
		return
	}
	trace.mark("snapshot")
	policy, err := orchestrator.ParsePolicy(topology, input)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	trace.mark("parse")
	previous, _, previousErr := repository.PolicySnapshot(request.Context())
	if previousErr != nil && !errors.Is(previousErr, orchestrator.ErrPolicyNotFound) {
		writePolicyRepositoryError(writer, previousErr)
		return
	}
	trace.mark("previous")
	if err := handler.applyTransparent(request.Context(), topology, &policy); err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	trace.mark("apply")
	trace.mark("persist-start")
	if err := repository.ReplacePolicy(request.Context(), policy); err != nil {
		var rollbackErr error
		if previousErr == nil {
			rollbackErr = handler.rollbackTransparent(request.Context(), topology, &previous)
		} else {
			rollbackErr = handler.rollbackTransparent(request.Context(), topology, nil)
		}
		if rollbackErr != nil {
			writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("persist policy: %v; restore transparent dataplane: %w", err, rollbackErr))
			return
		}
		writePolicyRepositoryError(writer, err)
		return
	}
	trace.mark("persist")
	if err := handler.commitTransparentTransaction(request.Context()); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	trace.mark("commit")
	stored, checksum, err := repository.PolicySnapshot(request.Context())
	if err != nil {
		writePolicyRepositoryError(writer, err)
		return
	}
	trace.mark("readback")
	if !handler.requireConverged(writer, request.Context(), topology, &stored) {
		return
	}
	trace.mark("verify")
	handler.recordAudit(request, PolicyPath, "update", "success", checksum)
	trace.mark("audit")
	writeJSON(writer, http.StatusOK, map[string]any{"item": stored.View(), "checksum": checksum})
}

func (handler *Handler) handlePolicyCompile(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, PermissionRead) {
		return
	}
	repository, ok := handler.policyRepository()
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, orchestrator.ErrRepositoryUnavailable)
		return
	}
	var input policyCompileRequest
	if err := decodeStrictJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	policy, checksum, err := repository.PolicySnapshot(request.Context())
	if err != nil {
		writePolicyRepositoryError(writer, err)
		return
	}
	flow, err := orchestrator.ParseFlow(input.Flow)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	prelude, err := orchestrator.ParsePrelude(input.Prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	path, err := orchestrator.CompilePolicy(policy, flow, prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"path": path, "policy_checksum": checksum})
}

func (handler *Handler) handleServiceChainCompile(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, PermissionRead) {
		return
	}
	repository, ok := handler.policyRepository()
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, orchestrator.ErrRepositoryUnavailable)
		return
	}
	var input serviceChainCompileRequest
	if err := decodeStrictJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	topology, topologyChecksum, err := handler.repository.Snapshot(request.Context())
	if err != nil {
		writeRepositoryError(writer, err)
		return
	}
	policy, policyChecksum, err := repository.PolicySnapshot(request.Context())
	if err != nil {
		writePolicyRepositoryError(writer, err)
		return
	}
	flow, err := orchestrator.ParseFlow(input.Flow)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	prelude, err := orchestrator.ParsePrelude(input.Prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	path, err := orchestrator.CompilePolicy(policy, flow, prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	response := map[string]any{"path": path, "topology_checksum": topologyChecksum, "policy_checksum": policyChecksum}
	if path.Exit == orchestrator.PathExitLAN {
		chain, chainErr := orchestrator.CompileServiceChain(topology, flow, path, input.Bindings)
		if chainErr != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, chainErr)
			return
		}
		response["chain"] = chain
	}
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) handleServiceChainApply(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, PermissionAdminWrite) {
		return
	}
	if handler.runtime == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, errors.New("orchestrator service-chain runtime is not configured"))
		return
	}
	repository, ok := handler.policyRepository()
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, orchestrator.ErrRepositoryUnavailable)
		return
	}
	var input serviceChainApplyRequest
	if err := decodeStrictJSON(writer, request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	topology, topologyChecksum, err := handler.repository.Snapshot(request.Context())
	if err != nil {
		writeRepositoryError(writer, err)
		return
	}
	policy, policyChecksum, err := repository.PolicySnapshot(request.Context())
	if err != nil {
		writePolicyRepositoryError(writer, err)
		return
	}
	flow, err := orchestrator.ParseFlow(input.Flow)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	prelude, err := orchestrator.ParsePrelude(input.Prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	path, err := orchestrator.CompilePolicy(policy, flow, prelude)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	if path.Exit != orchestrator.PathExitLAN {
		writeAPIError(writer, http.StatusUnprocessableEntity, errors.New("service-chain apply requires a LAN-exit policy path"))
		return
	}
	desiredChain, err := orchestrator.CompileServiceChain(topology, flow, path, input.Bindings)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	intentRepository, ok := handler.repository.(ServiceChainIntentRepository)
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, errors.New("orchestrator service-chain intent persistence is unavailable"))
		return
	}
	intentPayload, err := json.Marshal(input)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, errors.New("service-chain intent could not be encoded"))
		return
	}
	if err := intentRepository.SaveServiceChainIntent(request.Context(), desiredChain.ID, intentPayload); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, errors.New("service-chain intent could not be persisted"))
		return
	}
	unavailable := map[string]bool{}
	var health []orchestrator.GroupHealth
	if healthRuntime, ok := handler.runtime.(ServiceChainHealthRuntime); ok {
		unavailable, health, err = healthRuntime.ServiceChainUnavailable(request.Context(), input.Bindings)
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
	}
	chain, err := orchestrator.CompileServiceChainWithHealth(topology, flow, path, input.Bindings, unavailable)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	chain.ID = desiredChain.ID
	transactionID := strings.TrimSpace(request.Header.Get("X-Request-ID"))
	if transactionID == "" {
		transactionID = fmt.Sprintf("orchestrator-chain-%d", time.Now().UnixNano())
	}
	var result vpp.ServiceChainApplyResult
	if transitionRuntime, ok := handler.runtime.(ServiceChainTransitionRuntime); ok {
		result, err = transitionRuntime.ApplyServiceChainTransition(request.Context(), transactionID, desiredChain, chain, input.Attachments)
	} else {
		result, err = handler.runtime.ApplyServiceChain(request.Context(), transactionID, chain, input.Attachments)
	}
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"chain":             chain,
		"receipt":           result.Receipt,
		"readback":          result.Readback,
		"topology_checksum": topologyChecksum,
		"policy_checksum":   policyChecksum,
		"health":            health,
		"intent_id":         desiredChain.ID,
	})
}

func writePolicyRepositoryError(writer http.ResponseWriter, err error) {
	if errors.Is(err, orchestrator.ErrPolicyNotFound) || errors.Is(err, orchestrator.ErrTopologyNotFound) {
		writeAPIError(writer, http.StatusNotFound, err)
		return
	}
	if isPolicyValidationError(err) {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	writeAPIError(writer, http.StatusInternalServerError, errors.New("orchestration policy state is unavailable"))
}
