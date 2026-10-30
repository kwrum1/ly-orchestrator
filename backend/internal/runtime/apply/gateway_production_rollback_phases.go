package apply

import (
	"context"
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/vpp"
)

func (adapter *productionGatewayAdapter) RollbackCleanup(ctx context.Context, plan Plan) error {
	diff, prior, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found {
		return fmt.Errorf("%s rollback cleanup: reconciliation is unavailable", adapter.name)
	}
	operations, err := adapter.cleanupOperations(diff, prior)
	if err != nil {
		return err
	}
	// Only tear down supplemental objects this failed generation introduced.
	// Deleting a prerequisite that also existed in the previous generation
	// (notably the LAN LCP pair) removes the live address before rollback can
	// replay it, which is exactly the outage this rollback must prevent.
	introduced, err := adapter.introducedSupplementalCleanup(plan)
	if err != nil {
		return err
	}
	operations = append(introduced, operations...)
	if len(operations) == 0 {
		return nil
	}
	return adapter.reconciler.adapter.ExecuteOperations(ctx, operations)
}

func (adapter *productionGatewayAdapter) introducedSupplementalCleanup(plan Plan) ([]vpp.Operation, error) {
	owner := adapter.supplementalOwner()
	if owner == "" {
		return nil, nil
	}
	if plan.Previous.GatewayPlan == nil {
		// First generation: everything the desired plan owns was introduced.
		if !vpp.HasSupplementalOperations(plan.GatewayPlan, owner) {
			return nil, nil
		}
		return vpp.SupplementalCleanupOperations(plan.GatewayPlan, owner)
	}
	return vpp.SupplementalIntroducedCleanupOperations(*plan.Previous.GatewayPlan, plan.GatewayPlan, owner)
}

func (adapter *productionGatewayAdapter) RollbackRestore(ctx context.Context, plan Plan) error {
	diff, prior, found := adapter.reconciler.state(plan.Request.TransactionID)
	if !found {
		return fmt.Errorf("%s rollback restore: prior snapshot is unavailable", adapter.name)
	}
	// Prerequisites first: native attachments, the Linux control-plane LCP and
	// service handoffs must exist before interface addresses are replayed onto
	// them. Reversing this order (address first, LCP recreation second) leaves
	// the recreated LCP without its IPv4 address.
	if adapter.name == "interfaces" {
		if err := adapter.restoreNativeAttachments(ctx, plan); err != nil {
			return fmt.Errorf("restore interfaces native attachment: %w", err)
		}
	}
	// A failed generation can change the resolved LAN inventory even when the
	// per-resource diff looks empty. Restore the supplemental handoff for every
	// owner that owns live connectivity prerequisites (LCP, TAP handoff, PBR),
	// not only when a proof-comparable diff exists. Other owners keep the old
	// changed-only behaviour.
	if plan.Previous.GatewayPlan != nil && adapter.rollbackRestoresSupplemental(plan) {
		previous := priorReconciliationPlan(*plan.Previous.GatewayPlan)
		if vpp.HasSupplementalOperations(previous, adapter.supplementalOwner()) {
			if _, err := adapter.applySupplemental(ctx, previous); err != nil {
				return fmt.Errorf("restore %s prerequisites: %w", adapter.name, err)
			}
		}
	}
	if gatewaySnapshotHasResource(adapter.name, prior) {
		input := productionRollbackInput{transactionID: plan.Request.TransactionID, desired: plan.GatewayPlan, prior: prior, attempted: diff}
		if plan.Previous.GatewayPlan != nil {
			input.priorPlan = plan.Previous.GatewayPlan
		}
		gateway := adapter.priorPlan(input)
		// The desired objects were already removed by RollbackCleanup. Restore
		// replays the previous generation only; passing the attempted diff here
		// would make the ACL/QoS/NAT restore emit deletes for objects this
		// generation introduced and never owned.
		if _, err := adapter.runPrior(ctx, productionPriorApply{transactionID: plan.Request.TransactionID, gateway: gateway, prior: prior}); err != nil {
			return fmt.Errorf("restore %s state: %w", adapter.name, err)
		}
	}
	switch adapter.name {
	case "interfaces":
		if err := adapter.reconciler.adapter.EnsureInterfaceAddresses(ctx, plan.Request.TransactionID, prior.Interfaces); err != nil {
			return fmt.Errorf("verify restored LAN addresses: %w", err)
		}
		// Build-then-delete: the previous addresses are live again, so the
		// extra address this generation added can be removed without ever
		// leaving the interface addressless.
		if err := adapter.reconciler.adapter.RemoveStaleInterfaceAddresses(ctx, plan.Request.TransactionID, prior.Interfaces, diff.Interfaces.Interfaces); err != nil {
			return fmt.Errorf("remove addresses introduced by the failed generation: %w", err)
		}
	case "routes":
		if err := adapter.reconciler.adapter.RestorePreNATClassifier(ctx, plan.Request.TransactionID, prior.PreNATClassifier); err != nil {
			return fmt.Errorf("restore route policy classifier: %w", err)
		}
	}
	return nil
}

// rollbackRestoresSupplemental reports whether this owner's supplemental
// prerequisites must be replayed even when the diff looks unchanged. LCP/TAP
// handoffs own the live LAN address; ACL/QoS supplementals do not, so they keep
// the conservative changed-only restore.
func (adapter *productionGatewayAdapter) rollbackRestoresSupplemental(plan Plan) bool {
	switch adapter.name {
	case "interfaces", "service-networks", "routes":
		return true
	}
	changed, err := adapter.supplementalChanged(plan)
	if err != nil {
		return true
	}
	return changed
}

// priorReconciliationPlan marks a persisted prior generation for operation
// reconstruction. Its native capability proof has legitimately expired by the
// time a failed apply rolls back, which must not block restoring the prior
// declarative state.
func priorReconciliationPlan(prior vpp.Plan) vpp.Plan {
	prior.AllowUnprovenOperations = true
	return prior
}

func (adapter *productionGatewayAdapter) cleanupOperations(diff vpp.GatewayDiff, prior vpp.Snapshot) ([]vpp.Operation, error) {
	switch adapter.name {
	case "interfaces":
		// Only interfaces this failed generation created are torn down here.
		// A surviving interface keeps its address; RollbackRestore replays the
		// previous state and then removes the extra address, so the LAN never
		// spends a moment without one.
		return vpp.BuildInterfaceRollbackCleanupOperations(vpp.InterfaceBondPlan{TransactionID: diff.Interfaces.TransactionID, ManagementInterface: diff.Interfaces.ManagementInterface, Interfaces: diff.Interfaces.Interfaces}, prior.Interfaces)
	case "bonds":
		return vpp.BuildInterfaceBondOperations(vpp.InterfaceBondPlan{TransactionID: diff.Bonds.TransactionID, ManagementInterface: diff.Bonds.ManagementInterface, DeleteBonds: bondStateNames(diff.Bonds.Bonds)})
	case "wan-groups":
		return vpp.BuildRouteWANGroupOperations(vpp.RouteWANGroupPlan{TransactionID: diff.WANGroups.TransactionID, DeleteWANGroups: wanGroupNames(diff.WANGroups.WANGroups), DeleteWANState: diff.WANGroups.WANGroups})
	case "service-networks":
		// Service handoffs are supplemental-only; cleanup is provided by
		// SupplementalCleanupOperations for the same owner.
		return nil, nil
	case "routes":
		// Route cleanup must use the same resolved LAN ingress as the apply
		// plan.  Delete operations still need to detach the ABF policy from
		// its interface before removing the ACL; leaving this blank makes a
		// stale route fail reconciliation even though the desired route was
		// already removed from the configuration.
		return vpp.BuildRouteWANGroupOperations(vpp.RouteWANGroupPlan{
			TransactionID:       diff.Routes.TransactionID,
			IngressVPPInterface: diff.Routes.IngressVPPInterface,
			DeleteRoutes:        routeNames(diff.Routes.Routes),
			DeleteRouteState:    diff.Routes.Routes,
		})
	case "acls":
		return vpp.BuildACLQoSOperations(vpp.ACLQoSPlan{TransactionID: diff.ACLs.TransactionID, IngressVPPInterface: diff.ACLs.IngressVPPInterface, DeleteACLs: aclNames(diff.ACLs.ACLs), DeleteACLState: diff.ACLs.ACLs})
	case "qos":
		return vpp.BuildACLQoSOperations(vpp.ACLQoSPlan{TransactionID: diff.QoS.TransactionID, IngressVPPInterface: diff.QoS.IngressVPPInterface, DeleteQoS: qosNames(diff.QoS.QoS), DeleteQoSState: diff.QoS.QoS})
	case "nat44":
		return vpp.BuildNAT44Operations(vpp.NAT44Plan{TransactionID: diff.NAT44.TransactionID, IngressVPPInterface: diff.NAT44.IngressVPPInterface, DeleteStaticMappings: staticMappingNames(diff.NAT44.StaticMappings), ReadbackStaticMappings: diff.NAT44.StaticMappings})
	case "port-maps":
		return vpp.BuildNAT44Operations(vpp.NAT44Plan{TransactionID: diff.PortMaps.TransactionID, IngressVPPInterface: diff.PortMaps.IngressVPPInterface, DeletePortMappings: portMappingNames(diff.PortMaps.PortMappings), ReadbackPortMappings: diff.PortMaps.PortMappings})
	default:
		return nil, fmt.Errorf("unsupported production gateway resource %q", adapter.name)
	}
}

// restoreNativeAttachments recreates the native AF_PACKET/AF_XDP/TAP underlay
// that cleanup removed, before interface state is replayed onto it. The prior
// plan's capability proof is intentionally allowed to be stale here: this is a
// declarative restore, never a new apply.
func (adapter *productionGatewayAdapter) restoreNativeAttachments(ctx context.Context, plan Plan) error {
	if plan.Previous.GatewayPlan == nil {
		return nil
	}
	prior := *plan.Previous.GatewayPlan
	prior.AllowUnprovenOperations = true
	prior.DataplanePrepared = true
	// The current generation knows the QoS plugin is absent. A prior plan
	// persisted from a plugin-capable build still carries QoS groups; they
	// must not be replayed (or even compiled into commands) during rollback.
	if plan.GatewayPlan.SkipQoS {
		prior.SkipQoS = true
	}
	// Persisted plans strip the verified proof but keep the observed candidate.
	// Reuse it so a declarative restore recreates the same AF_PACKET/AF_XDP
	// attachment instead of defaulting to an unsupported hook.
	for index := range prior.NativePath.Assignments {
		assignment := &prior.NativePath.Assignments[index]
		if strings.TrimSpace(string(assignment.Proof.Hook)) == "" && len(assignment.Candidates) > 0 {
			assignment.Proof = assignment.Candidates[0]
		}
	}
	operations, err := vpp.BuildOperations(prior)
	if err != nil {
		return fmt.Errorf("rebuild prior native attachments: %w", err)
	}
	attach := make([]vpp.Operation, 0)
	for _, operation := range operations {
		if operation.Name == "vpp.dataplane.attach" {
			attach = append(attach, operation)
		}
	}
	if len(attach) == 0 {
		return nil
	}
	if err := adapter.reconciler.adapter.ExecuteOperations(ctx, attach); err != nil {
		return fmt.Errorf("restore native attachments: %w", err)
	}
	return nil
}

func interfaceStateNames(states []vpp.InterfaceState) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	return names
}

func bondStateNames(states []vpp.BondState) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	return names
}
