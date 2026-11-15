package orchestratorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/runtime/vpp"
)

const (
	TopologyPath            = "/api/v1/orchestrator/topology"
	OrchestrationGroupsPath = "/api/v1/orchestrator/orchestration-groups"
	PolicyPath              = "/api/v1/orchestrator/policy"
	PolicyStatusPath        = "/api/v1/orchestrator/policy/status"
	PolicyCompilePath       = "/api/v1/orchestrator/policy/compile"
	ServiceChainCompilePath = "/api/v1/orchestrator/service-chains/compile"
	ServiceChainApplyPath   = "/api/v1/orchestrator/service-chains/apply"
	ServiceChainStatusPath  = "/api/v1/orchestrator/service-chains/status"
	maxBodyBytes            = 1 << 20
)

type Handler struct {
	repository       orchestrator.Orchestrator
	access           AccessController
	runtime          ServiceChainRuntime
	updates          UpdateService
	operations       OperationTracker
	managementShared func(context.Context, string) bool
	ports            PortResolver
	reconcile        reconcileState
	bypass           BypassController
	// mutationMu serialises every configuration mutation from decoding to
	// persistence.  Apply used to hold only the runtime mutex and released it
	// before the record was persisted, so an overlapping topology/policy save
	// could commit its (older) desired state to VPP after the newer record was
	// stored.  The data plane then disagreed with the configuration until the
	// next manual apply.
	mutationMu sync.Mutex
	// policyTransactions tracks asynchronous policy writes. The console saves
	// with Prefer: respond-async so a 90-rule policy no longer blocks the form
	// for the whole VPP apply; it polls the status path instead.
	policyTxMu         sync.Mutex
	policyTransactions map[string]policyTransaction
}

// PortResolver maps a product interface id (eth2) to the kernel netdev that
// the dataplane actually uses (ens256). Topology writes persist the kernel
// name so a restart can probe the same interfaces without translating them
// again.
type PortResolver interface {
	InterfaceLinuxName(context.Context, string) string
}

type ServiceChainRuntime interface {
	ApplyServiceChain(context.Context, string, orchestrator.ServiceChain, []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error)
}

type TransparentRuntime interface {
	ApplyTransparent(context.Context, string, orchestrator.Topology, *orchestrator.Policy) error
}

type TransparentDisableRuntime interface {
	DisableTransparent(context.Context, string) error
}

// TransparentReconcileRuntime verifies the committed VPP generation/default
// against the configuration that was just persisted and re-applies it when the
// readback disagrees.
type TransparentReconcileRuntime interface {
	ReconcileTransparent(context.Context, string, orchestrator.Topology, *orchestrator.Policy) error
}

type TransparentRuntimeEvidence struct {
	TransactionID string    `json:"transaction_id"`
	Generation    string    `json:"generation"`
	State         string    `json:"state"`
	AppliedAt     time.Time `json:"applied_at"`
	ObservedAt    time.Time `json:"observed_at"`
	// PolicyGeneration is the generation derived from the persisted
	// configuration; VPPGeneration is what the plugin reports.  They are equal
	// only when the data plane enforces the configured policy.
	PolicyGeneration  string    `json:"policy_generation"`
	VPPGeneration     string    `json:"vpp_generation"`
	ConfiguredDefault string    `json:"configured_default"`
	ObservedDefault   string    `json:"observed_default"`
	Repairs           int       `json:"reconcile_repairs"`
	ReconcileFailures int       `json:"reconcile_failures"`
	LastDriftReason   string    `json:"last_drift_reason,omitempty"`
	LastReconciledAt  time.Time `json:"last_reconciled_at"`
	// Fault bypass is independent of the policy generation: when active the
	// plugin skips policy evaluation while still reporting the committed
	// generation, so the control plane can state both facts.
	Bypass        bool      `json:"bypass"`
	BypassForced  bool      `json:"bypass_forced,omitempty"`
	BypassCause   string    `json:"bypass_cause,omitempty"`
	BypassReason  string    `json:"bypass_reason,omitempty"`
	BypassSince   time.Time `json:"bypass_since,omitempty"`
	BypassPackets uint64    `json:"bypass_packets,omitempty"`
	// BypassGroups lists the groups currently bypassing policy.  A single
	// bypassed group does not degrade the appliance.
	BypassGroups []string `json:"bypass_groups,omitempty"`
}

type TransparentRuntimeEvidenceProvider interface {
	TransparentRuntimeEvidence(context.Context) (TransparentRuntimeEvidence, error)
}

type TransparentTransactionRuntime interface {
	CommitTransparentTransaction(context.Context) error
}

func (handler *Handler) SetManagementSharingResolver(resolver func(context.Context, string) bool) {
	handler.managementShared = resolver
}

// SetPortResolver installs the product-id -> kernel-netdev resolver used when
// normalizing topology writes.
func (handler *Handler) SetPortResolver(resolver PortResolver) {
	handler.ports = resolver
}

func (handler *Handler) resolvePort(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || handler.ports == nil {
		return value
	}
	if resolved := strings.TrimSpace(handler.ports.InterfaceLinuxName(ctx, value)); resolved != "" {
		return resolved
	}
	return value
}

func (handler *Handler) normalizeBondPorts(ctx context.Context, bond *bondDTO) {
	if bond == nil {
		return
	}
	for index := range bond.Members {
		bond.Members[index] = handler.resolvePort(ctx, bond.Members[index])
	}
}

func (handler *Handler) normalizeTopologyPorts(ctx context.Context, dto *topologyDTO) {
	dto.ManagementInterface = handler.resolvePort(ctx, dto.ManagementInterface)
	for index := range dto.Interfaces {
		dto.Interfaces[index].Port = handler.resolvePort(ctx, dto.Interfaces[index].Port)
		handler.normalizeBondPorts(ctx, dto.Interfaces[index].Bond)
	}
	for groupIndex := range dto.Groups {
		for portIndex := range dto.Groups[groupIndex].Ports {
			port := &dto.Groups[groupIndex].Ports[portIndex]
			port.Interface = handler.resolvePort(ctx, port.Interface)
			handler.normalizeBondPorts(ctx, port.Bond)
		}
	}
}

func (handler *Handler) normalizeGroupPorts(ctx context.Context, dto *groupDTO) {
	for portIndex := range dto.Ports {
		port := &dto.Ports[portIndex]
		port.Interface = handler.resolvePort(ctx, port.Interface)
		handler.normalizeBondPorts(ctx, port.Bond)
	}
}

func (handler *Handler) SetUpdateService(service UpdateService) {
	handler.updates = service
}

type ServiceChainTransitionRuntime interface {
	ApplyServiceChainTransition(context.Context, string, orchestrator.ServiceChain, orchestrator.ServiceChain, []vpp.NativeAttachment) (vpp.ServiceChainApplyResult, error)
}

type ServiceChainHealthRuntime interface {
	ServiceChainUnavailable(context.Context, []orchestrator.ServiceChainBindingInput) (map[string]bool, []orchestrator.GroupHealth, error)
}

type ServiceChainIntentRepository interface {
	SaveServiceChainIntent(context.Context, string, json.RawMessage) error
	ServiceChainIntents(context.Context) ([]orchestrator.ServiceChainIntentRecord, error)
}

func New(repository orchestrator.Orchestrator, access AccessController, runtimes ...ServiceChainRuntime) (*Handler, error) {
	if interfaceIsNil(repository) {
		return nil, orchestrator.ErrRepositoryUnavailable
	}
	if interfaceIsNil(access) {
		return nil, ErrAccessControllerUnavailable
	}
	var runtime ServiceChainRuntime
	if len(runtimes) > 0 {
		runtime = runtimes[0]
	}
	return &Handler{repository: repository, access: access, runtime: runtime}, nil
}

func interfaceIsNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}

func (handler *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Every write handler is wrapped so the whole "decode -> apply to VPP ->
	// persist -> commit journal" transaction is serialised with respect to
	// other configuration writes. Reads stay lock-free.
	mutate := func(handle http.HandlerFunc) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			switch request.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				handle(writer, request)
				return
			}
			if request.Method == http.MethodPut && request.URL.Path == PolicyPath && wantsAsyncPolicy(request) {
				// The asynchronous worker takes mutationMu itself. Holding it here
				// while the worker runs would block the status reads and every other
				// configuration write for the whole apply.
				handle(writer, request)
				return
			}
			handler.mutationMu.Lock()
			defer handler.mutationMu.Unlock()
			handle(writer, request)
		}
	}
	mux.HandleFunc(TopologyPath, mutate(handler.handleTopology))
	mux.HandleFunc(OrchestrationGroupsPath, mutate(handler.handleGroups))
	mux.HandleFunc(OrchestrationGroupsPath+"/", mutate(handler.handleGroup))
	mux.HandleFunc(PolicyPath, mutate(handler.handlePolicy))
	mux.HandleFunc(PolicyStatusPath, handler.handlePolicyStatus)
	mux.HandleFunc(PolicyCompilePath, handler.handlePolicyCompile)
	if _, transparent := handler.runtime.(TransparentRuntime); !transparent {
		mux.HandleFunc(ServiceChainCompilePath, handler.handleServiceChainCompile)
		mux.HandleFunc(ServiceChainApplyPath, handler.handleServiceChainApply)
		mux.HandleFunc(ServiceChainStatusPath, handler.handleServiceChainStatus)
	}
	if handler.updates != nil {
		mux.HandleFunc(UpgradesPath, handler.handleUpgrades)
		mux.HandleFunc(UpgradesPath+"/", handler.handleUpgradeItem)
		mux.HandleFunc(ConversionsPath, handler.handleConversions)
		mux.HandleFunc(ConversionsPath+"/", handler.handleConversionItem)
	}
	if handler.operations != nil {
		mux.HandleFunc(OperationsPath, handler.handleCurrentOperation)
	}
	handler.registerBypassRoutes(mux)
}

func (handler *Handler) handleTopology(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		handler.writeTopology(writer, request)
	case http.MethodPut:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		var dto topologyDTO
		if err := decodeStrictJSON(writer, request, &dto); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		if handler.managementShared != nil {
			dto.ManagementShared = handler.managementShared(request.Context(), dto.ManagementInterface)
		} else {
			dto.ManagementShared = false
		}
		// Persist kernel netdev names. The UI submits product ids (eth2), and
		// the startup probe / VPP boundary commands need ens256. Accept both
		// spellings on write but never store the logical alias again.
		handler.normalizeTopologyPorts(request.Context(), &dto)
		topology, err := orchestrator.ParseTopology(dto.input())
		if err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		previous, _, previousErr := handler.repository.Snapshot(request.Context())
		if previousErr != nil && !errors.Is(previousErr, orchestrator.ErrTopologyNotFound) {
			writeRepositoryError(writer, previousErr)
			return
		}
		var policy *orchestrator.Policy
		if previousErr == nil {
			var policyErr error
			policy, policyErr = handler.currentPolicy(request.Context())
			if policyErr != nil {
				writePolicyRepositoryError(writer, policyErr)
				return
			}
		}
		if err := handler.applyTransparent(request.Context(), topology, policy); err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		if err := handler.repository.Replace(request.Context(), topology); err != nil {
			if previousErr == nil {
				if rollbackErr := handler.rollbackTransparent(request.Context(), previous, policy); rollbackErr != nil {
					writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("persist topology: %v; restore transparent dataplane: %w", err, rollbackErr))
					return
				}
			} else if rollbackErr := handler.rollbackTransparentAbsent(request.Context()); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("persist topology: %v; disable unpersisted transparent dataplane: %w", err, rollbackErr))
				return
			}
			writeRepositoryError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !handler.requireConverged(writer, request.Context(), topology, policy) {
			return
		}
		handler.recordAudit(request, TopologyPath, "update", "success", "")
		handler.writeTopology(writer, request)
	case http.MethodDelete:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		current, _, snapshotErr := handler.repository.Snapshot(request.Context())
		if snapshotErr != nil {
			writeRepositoryError(writer, snapshotErr)
			return
		}
		policy, policyErr := handler.currentPolicy(request.Context())
		if policyErr != nil {
			writePolicyRepositoryError(writer, policyErr)
			return
		}
		if policy != nil {
			writeMutationError(writer, orchestrator.ErrDeletedPolicyReference)
			return
		}
		if err := handler.disableTransparent(request.Context()); err != nil {
			writeAPIError(writer, http.StatusUnprocessableEntity, err)
			return
		}
		if err := handler.repository.Delete(request.Context()); err != nil {
			if rollbackErr := handler.rollbackTransparent(request.Context(), current, nil); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("delete topology: %v; restore transparent dataplane: %w", err, rollbackErr))
				return
			}
			writeRepositoryError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		handler.recordAudit(request, TopologyPath, "delete", "success", "")
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleGroups(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, request, PermissionRead) {
			return
		}
		topology, checksum, err := handler.repository.Snapshot(request.Context())
		if err != nil {
			writeRepositoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, groupsResponse{Items: topology.View().Groups, Checksum: checksum})
	case http.MethodPost:
		if !handler.authorize(writer, request, PermissionAdminWrite) {
			return
		}
		group, err := handler.parseGroupRequest(writer, request)
		if err != nil {
			return
		}
		current, _, snapshotErr := handler.repository.Snapshot(request.Context())
		if snapshotErr != nil {
			writeMutationError(writer, snapshotErr)
			return
		}
		candidate, candidateErr := orchestrator.AddTopologyGroup(current, group)
		if candidateErr != nil {
			writeMutationError(writer, candidateErr)
			return
		}
		policy, policyErr := handler.currentPolicy(request.Context())
		if policyErr != nil {
			writePolicyRepositoryError(writer, policyErr)
			return
		}
		if err := handler.applyTransparent(request.Context(), candidate, policy); err != nil {
			writeOrchestrationApplyError(writer, err)
			return
		}
		if err := handler.repository.CreateGroup(request.Context(), group); err != nil {
			if rollbackErr := handler.rollbackTransparent(request.Context(), current, policy); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("create group: %v; restore transparent dataplane: %w", err, rollbackErr))
				return
			}
			writeMutationError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !handler.requireConverged(writer, request.Context(), candidate, policy) {
			return
		}
		handler.recordAudit(request, OrchestrationGroupsPath+"/"+group.Name(), "create", "success", "")
		handler.writeGroup(writer, request, group.Name(), http.StatusCreated)
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
	}
}

func (handler *Handler) handleGroup(writer http.ResponseWriter, request *http.Request) {
	var permission Permission
	switch request.Method {
	case http.MethodGet:
		permission = PermissionRead
	case http.MethodPut, http.MethodDelete:
		permission = PermissionAdminWrite
	default:
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, permission) {
		return
	}
	name, err := groupNameFromPath(request.URL.Path)
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, orchestrator.ErrGroupNotFound)
		return
	}
	switch request.Method {
	case http.MethodGet:
		handler.writeGroup(writer, request, name, http.StatusOK)
	case http.MethodPut:
		group, parseErr := handler.parseGroupRequest(writer, request)
		if parseErr != nil {
			return
		}
		if group.Name() != name {
			writeAPIError(writer, http.StatusUnprocessableEntity, fmt.Errorf("%w: body name must match path", orchestrator.ErrInvalidName))
			return
		}
		current, _, snapshotErr := handler.repository.Snapshot(request.Context())
		if snapshotErr != nil {
			writeMutationError(writer, snapshotErr)
			return
		}
		candidate, candidateErr := orchestrator.ReplaceTopologyGroup(current, name, group)
		if candidateErr != nil {
			writeMutationError(writer, candidateErr)
			return
		}
		policy, policyErr := handler.currentPolicy(request.Context())
		if policyErr != nil {
			writePolicyRepositoryError(writer, policyErr)
			return
		}
		if err := handler.applyTransparent(request.Context(), candidate, policy); err != nil {
			writeOrchestrationApplyError(writer, err)
			return
		}
		if err := handler.repository.ReplaceGroup(request.Context(), name, group); err != nil {
			if rollbackErr := handler.rollbackTransparent(request.Context(), current, policy); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("replace group: %v; restore transparent dataplane: %w", err, rollbackErr))
				return
			}
			writeMutationError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !handler.requireConverged(writer, request.Context(), candidate, policy) {
			return
		}
		handler.recordAudit(request, OrchestrationGroupsPath+"/"+name, "update", "success", "")
		handler.writeGroup(writer, request, name, http.StatusOK)
	case http.MethodDelete:
		current, _, snapshotErr := handler.repository.Snapshot(request.Context())
		if snapshotErr != nil {
			writeMutationError(writer, snapshotErr)
			return
		}
		policy, policyErr := handler.currentPolicy(request.Context())
		if policyErr != nil {
			writePolicyRepositoryError(writer, policyErr)
			return
		}
		if policy != nil && policyReferencesGroup(policy, name) {
			handler.recordAudit(request, OrchestrationGroupsPath+"/"+name, "delete", "failure", "orchestration group is referenced by policy")
			writeAPIError(writer, http.StatusUnprocessableEntity, fmt.Errorf("%w: group %q is referenced by policy; update or delete the referencing rules first", orchestrator.ErrGroupInUse, name))
			return
		}
		candidate, candidateErr := orchestrator.RemoveTopologyGroup(current, name)
		if candidateErr != nil {
			writeMutationError(writer, candidateErr)
			return
		}
		if err := handler.applyTransparent(request.Context(), candidate, policy); err != nil {
			writeOrchestrationApplyError(writer, err)
			return
		}
		if err := handler.repository.DeleteGroup(request.Context(), name); err != nil {
			if rollbackErr := handler.rollbackTransparent(request.Context(), current, policy); rollbackErr != nil {
				writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("delete group: %v; restore transparent dataplane: %w", err, rollbackErr))
				return
			}
			writeMutationError(writer, err)
			return
		}
		if err := handler.commitTransparentTransaction(request.Context()); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !handler.requireConverged(writer, request.Context(), candidate, policy) {
			return
		}
		handler.recordAudit(request, OrchestrationGroupsPath+"/"+name, "delete", "success", "")
		writer.WriteHeader(http.StatusNoContent)
	}
}

func (handler *Handler) recordAudit(request *http.Request, resource, action, status, detail string) {
	recorder, ok := handler.access.(AuditRecorder)
	if !ok {
		return
	}
	recorder.RecordAudit(request, resource, action, status, detail)
}

// policyTransaction is the observable state of one asynchronous policy write.
// Status values match the console contract: applying, applied, apply_failed.
type policyTransaction struct {
	ID         string    `json:"transaction_id"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func wantsAsyncPolicy(request *http.Request) bool {
	if request.Method != http.MethodPut {
		return false
	}
	if strings.Contains(strings.ToLower(request.Header.Get("Prefer")), "respond-async") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(request.URL.Query().Get("async"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func requestWithBody(request *http.Request, body []byte) *http.Request {
	clone := request.Clone(request.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	return clone
}

func (handler *Handler) beginPolicyTransaction() (policyTransaction, error) {
	handler.policyTxMu.Lock()
	defer handler.policyTxMu.Unlock()
	if handler.policyTransactions == nil {
		handler.policyTransactions = map[string]policyTransaction{}
	}
	// Prune finished transactions after ten minutes: the console polls for at
	// most a couple of minutes and the map must not grow forever.
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, transaction := range handler.policyTransactions {
		if !transaction.StartedAt.After(cutoff) {
			delete(handler.policyTransactions, id)
		}
	}
	for _, transaction := range handler.policyTransactions {
		if transaction.Status == "applying" {
			return policyTransaction{}, errors.New("another policy apply is already in progress")
		}
	}
	transaction := policyTransaction{
		ID:        fmt.Sprintf("policy-%d", time.Now().UnixNano()),
		Status:    "applying",
		StartedAt: time.Now().UTC(),
	}
	handler.policyTransactions[transaction.ID] = transaction
	return transaction, nil
}

func (handler *Handler) finishPolicyTransaction(id, status, reason string) {
	handler.policyTxMu.Lock()
	defer handler.policyTxMu.Unlock()
	transaction, ok := handler.policyTransactions[id]
	if !ok {
		return
	}
	transaction.Status = status
	transaction.Reason = reason
	transaction.FinishedAt = time.Now().UTC()
	handler.policyTransactions[id] = transaction
}

func (handler *Handler) policyTransactionByID(id string) (policyTransaction, bool) {
	handler.policyTxMu.Lock()
	defer handler.policyTxMu.Unlock()
	transaction, ok := handler.policyTransactions[id]
	return transaction, ok
}

// runAsyncPolicyPut executes one asynchronous policy write. The caller already
// validated the JSON body, so the worker can serialise with other mutations and
// reuse the exact synchronous handler; its response is captured for the status
// endpoint instead of being sent to the original request.
func (handler *Handler) runAsyncPolicyPut(transactionID string, request *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	request = request.WithContext(ctx)
	recorder := &policyResponseRecorder{header: http.Header{}}
	handler.mutationMu.Lock()
	handler.servePolicyPut(recorder, request)
	handler.mutationMu.Unlock()
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 200 && status < 300 {
		handler.finishPolicyTransaction(transactionID, "applied", "")
		return
	}
	handler.finishPolicyTransaction(transactionID, "apply_failed", recorder.reason())
}

// policyResponseRecorder captures the synchronous handler's status and error
// message so the asynchronous status endpoint can report a real reason.
type policyResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (recorder *policyResponseRecorder) Header() http.Header {
	return recorder.header
}

func (recorder *policyResponseRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
}

func (recorder *policyResponseRecorder) Write(payload []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.body.Write(payload)
}

func (recorder *policyResponseRecorder) reason() string {
	var payload errorResponse
	if err := json.Unmarshal(recorder.body.Bytes(), &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	text := strings.TrimSpace(recorder.body.String())
	if len(text) > 400 {
		text = text[:400]
	}
	if text == "" {
		return fmt.Sprintf("policy apply failed with status %d", recorder.status)
	}
	return text
}

func (handler *Handler) handlePolicyStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeAPIError(writer, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if !handler.authorize(writer, request, PermissionRead) {
		return
	}
	id := strings.TrimSpace(request.URL.Query().Get("transaction_id"))
	if id == "" {
		writeAPIError(writer, http.StatusBadRequest, errors.New("transaction_id is required"))
		return
	}
	transaction, ok := handler.policyTransactionByID(id)
	if !ok {
		writeAPIError(writer, http.StatusNotFound, errors.New("policy transaction not found"))
		return
	}
	writeJSON(writer, http.StatusOK, transaction)
}

func policyReferencesGroup(policy *orchestrator.Policy, name string) bool {
	if policy == nil {
		return false
	}
	for _, group := range policy.View().Groups {
		for _, rule := range group.Rules {
			if rule.Action.Kind == orchestrator.ActionVia && rule.Action.Group == name {
				return true
			}
		}
	}
	return false
}

func (handler *Handler) currentPolicy(ctx context.Context) (*orchestrator.Policy, error) {
	repository, ok := handler.policyRepository()
	if !ok {
		return nil, nil
	}
	policy, _, err := repository.PolicySnapshot(ctx)
	if errors.Is(err, orchestrator.ErrPolicyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func (handler *Handler) applyTransparent(ctx context.Context, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	runtime, ok := handler.runtime.(TransparentRuntime)
	if !ok {
		return nil
	}
	if handler.operations != nil {
		handler.operations.BeginOperation("transparent-apply", "orchestrator topology/policy")
		defer handler.operations.EndOperation()
	}
	requestID := fmt.Sprintf("orchestrator-transparent-%d", time.Now().UnixNano())
	return runtime.ApplyTransparent(ctx, requestID, topology, policy)
}

func (handler *Handler) disableTransparent(ctx context.Context) error {
	runtime, ok := handler.runtime.(TransparentDisableRuntime)
	if !ok {
		return nil
	}
	requestID := fmt.Sprintf("orchestrator-transparent-disable-%d", time.Now().UnixNano())
	return runtime.DisableTransparent(ctx, requestID)
}

func (handler *Handler) rollbackTransparent(ctx context.Context, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	if err := handler.applyTransparent(ctx, topology, policy); err != nil {
		return err
	}
	return handler.commitTransparentTransaction(ctx)
}

func (handler *Handler) rollbackTransparentAbsent(ctx context.Context) error {
	if err := handler.disableTransparent(ctx); err != nil {
		return err
	}
	return handler.commitTransparentTransaction(ctx)
}

func (handler *Handler) commitTransparentTransaction(ctx context.Context) error {
	runtime, ok := handler.runtime.(TransparentTransactionRuntime)
	if !ok {
		return nil
	}
	if err := runtime.CommitTransparentTransaction(ctx); err != nil {
		return fmt.Errorf("commit transparent transaction journal: %w", err)
	}
	return nil
}

// reconcileTransparent is the post-persist readback: the configuration is
// already durable, so this compares the plugin's committed generation and
// default action with that durable state and re-applies it when they differ.
func (handler *Handler) reconcileTransparent(ctx context.Context, topology orchestrator.Topology, policy *orchestrator.Policy) error {
	runtime, ok := handler.runtime.(TransparentReconcileRuntime)
	if !ok {
		return nil
	}
	requestID := fmt.Sprintf("orchestrator-reconcile-%d", time.Now().UnixNano())
	return runtime.ReconcileTransparent(ctx, requestID, topology, policy)
}

// requireConverged reports a 500 when the data plane did not converge on the
// configuration that was just persisted. The record is kept (and the periodic
// reconciler retries), but the API must not claim success.
func (handler *Handler) requireConverged(writer http.ResponseWriter, ctx context.Context, topology orchestrator.Topology, policy *orchestrator.Policy) bool {
	if err := handler.reconcileTransparent(ctx, topology, policy); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, fmt.Errorf("configuration persisted but transparent dataplane did not converge: %w", err))
		return false
	}
	return true
}

func (handler *Handler) writeTopology(writer http.ResponseWriter, request *http.Request) {
	topology, checksum, err := handler.repository.Snapshot(request.Context())
	if err != nil {
		writeRepositoryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, topologyResponse{Item: topology.View(), Checksum: checksum})
}

func (handler *Handler) writeGroup(writer http.ResponseWriter, request *http.Request, name string, status int) {
	topology, checksum, err := handler.repository.Snapshot(request.Context())
	if err != nil {
		writeRepositoryError(writer, err)
		return
	}
	for _, group := range topology.View().Groups {
		if group.Name == name {
			writeJSON(writer, status, groupResponse{Item: group, Checksum: checksum})
			return
		}
	}
	writeAPIError(writer, http.StatusNotFound, fmt.Errorf("%w: %q", orchestrator.ErrGroupNotFound, name))
}

func (handler *Handler) parseGroupRequest(writer http.ResponseWriter, request *http.Request) (orchestrator.Group, error) {
	var dto groupDTO
	if err := decodeStrictJSON(writer, request, &dto); err != nil {
		writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return orchestrator.Group{}, err
	}
	handler.normalizeGroupPorts(request.Context(), &dto)
	group, err := orchestrator.ParseGroup(dto.input())
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
		return orchestrator.Group{}, err
	}
	return group, nil
}

func decodeStrictJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func groupNameFromPath(path string) (string, error) {
	raw := strings.TrimPrefix(path, OrchestrationGroupsPath+"/")
	if raw == "" || strings.ContainsAny(raw, `/\`) {
		return "", orchestrator.ErrGroupNotFound
	}
	name, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(name) == "" || strings.ContainsAny(name, `/\`) {
		return "", orchestrator.ErrGroupNotFound
	}
	return name, nil
}

func writeOrchestrationApplyError(writer http.ResponseWriter, err error) {
	var locked *vpp.DataplaneLockedError
	if errors.As(err, &locked) {
		writeJSON(writer, http.StatusUnprocessableEntity, errorResponse{Error: errorBody{
			Code:    "prerequisite_unmet",
			Message: "no qualified dataplane path is available for the requested orchestration group; remove or rebind a group, or add a free data interface",
		}})
		return
	}
	writeAPIError(writer, http.StatusUnprocessableEntity, err)
}

func writeMutationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrTopologyNotFound), errors.Is(err, orchestrator.ErrDuplicateGroup), errors.Is(err, orchestrator.ErrTopologyConflict):
		writeAPIError(writer, http.StatusConflict, err)
	case errors.Is(err, orchestrator.ErrGroupNotFound):
		writeAPIError(writer, http.StatusNotFound, err)
	case codeForError(err) != "internal_error":
		writeAPIError(writer, http.StatusUnprocessableEntity, err)
	default:
		writeAPIError(writer, http.StatusInternalServerError, errors.New("orchestrator state mutation failed"))
	}
}

func writeRepositoryError(writer http.ResponseWriter, err error) {
	if errors.Is(err, orchestrator.ErrTopologyNotFound) || errors.Is(err, orchestrator.ErrGroupNotFound) {
		writeAPIError(writer, http.StatusNotFound, err)
		return
	}
	writeAPIError(writer, http.StatusInternalServerError, errors.New("orchestrator state is unavailable"))
}
