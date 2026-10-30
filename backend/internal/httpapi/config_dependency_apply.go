package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"ly-route/backend/internal/configgraph"
	"ly-route/backend/internal/runtime/vpp"
)

// Apply ordering and repeat-apply semantics.
//
// WS-K adds two things to the apply path without touching WS-I's
// runtimeDesiredSourceHash / tryNoopRuntimeApply:
//
//  1. fail-closed cycle detection. A dependency cycle cannot be deployed in
//     any order; the apply refuses before a single VPP command is rendered.
//  2. graph_generation skip. Every desired-state mutation bumps a persisted
//     counter. If the counter and the dependency-graph content hash both match
//     the last successful apply, and the live generation is still committed,
//     the apply returns status=skipped instead of re-running VPP.
//
// The apply plan carries the dependency phases so operations.json (consumed by
// vpp-apply) and runtime/preview show which configuration phase an operation
// belongs to. The operational order inside a phase remains the existing
// vpp-apply staging order; the phases are the configuration-level contract.

// ConfigDependencyPlan is the dependency view embedded in RuntimePlan and the
// apply response.
type ConfigDependencyPlan struct {
	GraphGeneration int64                   `json:"graph_generation"`
	ContentHash     string                  `json:"content_hash"`
	CreateOrder     []configgraph.NodeRef   `json:"create_order"`
	DeleteOrder     []configgraph.NodeRef   `json:"delete_order"`
	Phases          []configgraph.Phase     `json:"phases"`
	Cycles          [][]configgraph.NodeRef `json:"cycles,omitempty"`
	Error           string                  `json:"error,omitempty"`
}

type configGraphSkipDecision struct {
	Skip        bool
	Reason      string
	Generation  int64
	ContentHash string
}

func configGraphSkipApplyEnabled() bool {
	return envBoolOrDefault("LY_ROUTE_CONFIG_GRAPH_SKIP_APPLY", true)
}

// configDependencyPlan builds the plan view. It never fails the caller: a
// graph read error is reported inside the plan so preview stays available while
// the apply path gets its own hard failure from configGraphApplyPreflight.
func (server *Server) configDependencyPlan(ctx context.Context) *ConfigDependencyPlan {
	graph, err := server.configGraph(ctx)
	if err != nil {
		return &ConfigDependencyPlan{Error: err.Error()}
	}
	generation, contentHash, err := server.configGraphGenerationFor(ctx, graph)
	if err != nil {
		return &ConfigDependencyPlan{ContentHash: graph.ContentHash(), Error: err.Error()}
	}
	return &ConfigDependencyPlan{
		GraphGeneration: generation,
		ContentHash:     contentHash,
		CreateOrder:     graph.CreateOrder,
		DeleteOrder:     graph.DeleteOrder,
		Phases:          graph.Phases,
		Cycles:          graph.Cycles,
	}
}

// configGraphApplyPreflight is the hard gate before planning. It returns the
// graph and the persisted state; a cycle is returned as errDependencyCycle.
func (server *Server) configGraphApplyPreflight(ctx context.Context) (*configgraph.Graph, configGraphState, error) {
	graph, err := server.configGraph(ctx)
	if err != nil {
		return nil, configGraphState{}, fmt.Errorf("dependency graph: %w", err)
	}
	if len(graph.Cycles) > 0 {
		return graph, configGraphState{}, dependencyCycleError(graph)
	}
	state, _, err := server.loadConfigGraphState(ctx)
	if err != nil {
		return graph, configGraphState{}, fmt.Errorf("dependency graph state: %w", err)
	}
	return graph, state, nil
}

// configGraphApplySkip decides whether this apply can be skipped. Every
// condition must hold:
//
//   - the caller did not force an apply;
//   - the persisted generation and content hash match the last successful
//     apply;
//   - the last recorded status is "applied";
//   - the live runtime result is committed, so a VPP restart or a failed
//     readback still re-applies.
func (server *Server) configGraphApplySkip(ctx context.Context, req ConfigApplyRequest, graph *configgraph.Graph, state configGraphState) configGraphSkipDecision {
	generation, contentHash, err := server.configGraphGenerationFor(ctx, graph)
	decision := configGraphSkipDecision{Generation: generation, ContentHash: contentHash}
	if err != nil {
		decision.Reason = "dependency graph generation unavailable: " + err.Error()
		return decision
	}
	if req.Force || !configGraphSkipApplyEnabled() {
		decision.Reason = "forced apply"
		return decision
	}
	if state.Generation == 0 || state.LastApplyStatus != "applied" {
		decision.Reason = "no successful apply for this graph generation"
		return decision
	}
	if state.LastAppliedGeneration != generation || state.LastAppliedContentHash != contentHash {
		decision.Reason = fmt.Sprintf("configuration changed since the last apply (generation %d -> %d)", state.LastAppliedGeneration, generation)
		return decision
	}
	last := server.lastRuntimeApply()
	if last == nil || !applyResultCommitted(*last) {
		decision.Reason = "the live generation is not committed"
		return decision
	}
	if last.Status == "" || last.Status == "apply_failed" || last.Status == "failed" {
		decision.Reason = "the last apply did not commit"
		return decision
	}
	decision.Skip = true
	decision.Reason = "configuration graph is unchanged and the last generation is committed"
	return decision
}

func (server *Server) recordConfigGraphApplyStatus(ctx context.Context, status, transactionID, reason string) {
	if server.store == nil {
		return
	}
	server.configGraphMu.Lock()
	defer server.configGraphMu.Unlock()
	state, _, err := server.loadConfigGraphState(ctx)
	if err != nil {
		return
	}
	state.LastApplyStatus = status
	state.LastTransactionID = transactionID
	state.LastReason = reason
	now := server.now().UTC()
	state.LastApplyAt = &now
	state.UpdatedAt = now
	_ = server.saveConfigGraphState(ctx, state)
}

func (server *Server) recordConfigGraphApplied(ctx context.Context, transactionID string, graph *configgraph.Graph) {
	if server.store == nil {
		return
	}
	generation, contentHash, err := server.configGraphGenerationFor(ctx, graph)
	if err != nil {
		server.recordConfigGraphApplyStatus(ctx, "applied_no_hash", transactionID, err.Error())
		return
	}
	server.configGraphMu.Lock()
	defer server.configGraphMu.Unlock()
	state, _, err := server.loadConfigGraphState(ctx)
	if err != nil {
		return
	}
	now := server.now().UTC()
	state.LastApplyStatus = "applied"
	state.LastAppliedGeneration = generation
	state.LastAppliedContentHash = contentHash
	state.LastTransactionID = transactionID
	state.LastApplyAt = &now
	state.LastReason = "applied graph generation " + fmt.Sprint(generation)
	state.UpdatedAt = now
	_ = server.saveConfigGraphState(ctx, state)
}

// configGraphApplyFailureResponse renders the 422 body for a cycle. The cycle
// path is included verbatim so the operator can see the loop.
func configGraphApplyFailureResponse(r *http.Request, transactionID string, graph *configgraph.Graph, err error) map[string]any {
	cycles := [][]configgraph.NodeRef{}
	if graph != nil {
		cycles = graph.Cycles
	}
	return map[string]any{
		"status":           "apply_failed",
		"runtime_state":    "degraded",
		"reason":           err.Error(),
		"transaction_id":   transactionID,
		"graph_generation": 0,
		"cycles":           cycles,
		"error": map[string]any{
			"code":       "dependency_cycle",
			"message":    err.Error(),
			"request_id": requestID(r),
		},
		"request_id": requestID(r),
	}
}

// configPhaseForOperation maps a vpp operation name to the configuration phase
// that owns it. The mapping is advisory and only groups the plan for humans and
// UI progress; vpp-apply keeps its own staging order for correctness.
func configPhaseForOperation(name string) string {
	switch {
	case strings.HasPrefix(name, "vpp.dataplane"), strings.HasPrefix(name, "vpp.interface"), strings.HasPrefix(name, "vpp.lcp"),
		strings.HasPrefix(name, "vpp.tap"), strings.HasPrefix(name, "vpp.bond"), strings.HasPrefix(name, "vpp.management"):
		return "interfaces"
	case strings.HasPrefix(name, "vpp.wan"), strings.HasPrefix(name, "vpp.pppoe"):
		return "lines"
	case strings.HasPrefix(name, "vpp.nat"), strings.HasPrefix(name, "vpp.route"), strings.HasPrefix(name, "vpp.pre-nat"):
		return "routing"
	case strings.HasPrefix(name, "vpp.proxy"), strings.HasPrefix(name, "vpp.dns"):
		return "proxy"
	case strings.HasPrefix(name, "vpp.acl"), strings.HasPrefix(name, "vpp.abf"), strings.HasPrefix(name, "vpp.security"),
		strings.HasPrefix(name, "vpp.qos"), strings.HasPrefix(name, "vpp.flow"):
		return "policies"
	case strings.HasPrefix(name, "vpp.service"):
		return "services"
	default:
		return "interfaces"
	}
}

// annotateConfigPhases stamps every operation with the configuration phase it
// belongs to. The field is additive in operations.json; vpp-apply ignores
// unknown keys.
func annotateConfigPhases(operations []vpp.Operation, plan *ConfigDependencyPlan) {
	if plan == nil || len(plan.Cycles) > 0 {
		return
	}
	for index := range operations {
		operations[index].ConfigPhase = configPhaseForOperation(operations[index].Name)
	}
}

// applyResultCommitted is defined in apply_gate.go and shared here.
