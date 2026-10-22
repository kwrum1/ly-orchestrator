package httpapi

import (
	"context"
	"time"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/vpp"
)

// decoratePortMapRuntimeStates keeps the port-map list consistent with the
// route-policy list: a desired resource is only shown as applied after the
// same transaction has a committed typed VPP readback and the current desired
// NAT plan still matches that evidence.
func (server *Server) decoratePortMapRuntimeStates(ctx context.Context, items []map[string]any) []map[string]any {
	applied := server.appliedNATPortMapIDs(ctx)
	decorated := make([]map[string]any, 0, len(items))
	for _, item := range items {
		clone := decoratePortMapItem(item)
		clone["runtime_state"] = "desired_not_applied"
		if _, ok := applied[stringField(clone, "id")]; ok {
			clone["runtime_state"] = "applied"
			clone["dataplane_observed"] = true
		}
		decorated = append(decorated, clone)
	}
	return decorated
}

func (server *Server) appliedNATPortMapIDs(ctx context.Context) map[string]struct{} {
	if applied := server.evidenceNATPortMapIDs(ctx); applied != nil {
		return applied
	}
	// A cold boot replays the durable VPP operations before the next control
	// transaction. The persisted evidence still describes the previous
	// transaction (and may contain the previous PPPoE address), so status must
	// verify the live VPP mapping rather than reporting desired_not_applied for
	// a mapping that is actually forwarding.
	return server.liveNATPortMapIDs(ctx)
}

func (server *Server) evidenceNATPortMapIDs(ctx context.Context) map[string]struct{} {
	result := server.runtimeEvidence()
	if result.Status != RuntimeStatusCommitted || result.RuntimeState != RuntimeStateRunning || result.GatewayPlan == nil {
		return nil
	}
	if result.TransactionID == "" || result.Receipt.Status != apply.ReceiptApplied || result.Receipt.TransactionID != result.TransactionID {
		return nil
	}
	matched := false
	for _, evidence := range result.GatewayEvidence {
		if evidence.Resource != "port-maps" || evidence.TransactionID != result.TransactionID {
			continue
		}
		if evidence.ApplyReceipt.Status != apply.ReceiptApplied || evidence.ApplyReceipt.TransactionID != result.TransactionID {
			continue
		}
		if equalGatewaySliceUnordered(evidence.After.NAT.PortMappings, result.GatewayPlan.NAT.PortMappings) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	current, err := server.currentNATConfig(ctx)
	if err != nil || !equalGatewaySliceUnordered(current.PortMappings, result.GatewayPlan.NAT.PortMappings) {
		return nil
	}
	applied := make(map[string]struct{}, len(current.PortMappings))
	for _, mapping := range current.PortMappings {
		applied[mapping.ID] = struct{}{}
	}
	return applied
}

func (server *Server) liveNATPortMapIDs(ctx context.Context) map[string]struct{} {
	current, err := server.currentNATConfig(ctx)
	if err != nil || len(current.PortMappings) == 0 {
		return nil
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		return nil
	}
	lanInterface := ""
	for _, assignment := range assignments {
		if assignment.Role == "lan" {
			lanInterface = assignment.VPPInterface
			break
		}
	}
	if lanInterface == "" {
		return nil
	}
	ids := make([]string, 0, len(current.PortMappings))
	for _, mapping := range current.PortMappings {
		ids = append(ids, mapping.ID)
	}
	request := vpp.SnapshotRequest{
		TransactionID:          "status-port-maps",
		Capabilities:           []vpp.SnapshotCapability{vpp.SnapshotCapabilityNAT44},
		NATBehavior:            current.Behavior,
		NATIngressVPPInterface: lanInterface,
		NATPortMappings:        ids,
		Candidates:             vpp.SnapshotCandidates{NATPortMappings: current.PortMappings},
		VerifyNATReturnGuards:  true,
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snapshot, err := (vpp.Adapter{Client: vpp.NewProductionVPPCTLClient(liveVPPBinary())}).Snapshot(readCtx, request)
	if err != nil || !equalGatewaySliceUnordered(snapshot.NAT.PortMappings, current.PortMappings) {
		return nil
	}
	applied := make(map[string]struct{}, len(current.PortMappings))
	for _, mapping := range current.PortMappings {
		applied[mapping.ID] = struct{}{}
	}
	return applied
}
