package httpapi

import (
	"context"
	"strings"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/vpp"
)

func (server *Server) decorateTrafficControlRuntimeStates(ctx context.Context, items []map[string]any) []map[string]any {
	applied := server.appliedTrafficControlIDs(ctx)
	capability, probeErr := server.qosCapability(ctx)
	// A build that provably cannot enforce an item's actions must not present a
	// stale applied generation as if the policy were in force. Unknown probe
	// results keep the evidence-based state: they are not proof of failure.
	capabilityKnown := probeErr == nil
	decorated := make([]map[string]any, 0, len(items))
	for _, item := range items {
		clone := decorateTrafficControlItem(item)
		clone["runtime_state"] = "desired_not_applied"
		clone["dataplane_observed"] = false
		if capabilityKnown {
			if reason, unsupported := qosItemUnsupported(capability, item); unsupported {
				clone["runtime_state"] = "unsupported"
				clone["capability_reason"] = reason
				decorated = append(decorated, clone)
				continue
			}
		}
		if _, ok := applied[stringField(clone, "id")]; ok {
			clone["runtime_state"] = "applied"
			clone["dataplane_observed"] = true
		}
		decorated = append(decorated, clone)
	}
	return decorated
}

// qosItemUnsupported reports whether this stored policy needs a dataplane
// facility the running build does not provide. The check is per item: a
// blocking rule stays enforceable on a build without any QoS facility.
func qosItemUnsupported(capability vpp.QoSCapability, item map[string]any) (string, bool) {
	intent, err := flowIntentFromDesiredPayload(item)
	if err != nil {
		return "", false
	}
	compiled, err := flow.CompileIntent(intent)
	if err != nil {
		return "", false
	}
	missing := vpp.MissingQoSCapabilities(capability, compiled.VPPGroups)
	if len(missing) == 0 {
		return "", false
	}
	return "traffic-control policy needs " + strings.Join(missing, ", ") + "; this VPP build does not provide it", true
}

func (server *Server) decorateTrafficControlRuntimeState(ctx context.Context, item map[string]any) map[string]any {
	items := server.decorateTrafficControlRuntimeStates(ctx, []map[string]any{item})
	return items[0]
}

func (server *Server) appliedTrafficControlIDs(ctx context.Context) map[string]struct{} {
	result := server.runtimeEvidence()
	if !runtimeTransactionCommitted(result) || result.GatewayPlan == nil {
		return nil
	}

	var observed []flow.VPPObjectGroup
	for _, evidence := range result.GatewayEvidence {
		if evidence.Resource != "qos" || evidence.TransactionID != result.TransactionID {
			continue
		}
		if evidence.ApplyReceipt.Status != apply.ReceiptApplied || evidence.ApplyReceipt.TransactionID != result.TransactionID {
			continue
		}
		observed = evidence.After.QoS
		break
	}
	if observed == nil || !equalGatewaySlice(observed, result.GatewayPlan.Flow.VPPGroups) {
		return nil
	}

	current, err := server.currentFlowIntent(ctx)
	if err != nil {
		return nil
	}
	expanded, err := server.expandFlowIntentAddressGroups(ctx, current)
	if err != nil {
		return nil
	}
	compiled, err := flow.CompileIntent(expanded)
	if err != nil || compiled.ID != result.GatewayPlan.Flow.ID {
		return nil
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		return nil
	}
	if err := vpp.ResolveFlowShapingInterfaces(&compiled, assignments); err != nil {
		return nil
	}

	if !equalGatewaySlice(compiled.Targets, result.GatewayPlan.Flow.Targets) || !equalGatewaySlice(compiled.VPPGroups, result.GatewayPlan.Flow.VPPGroups) {
		return nil
	}
	return map[string]struct{}{current.ID: {}}
}
