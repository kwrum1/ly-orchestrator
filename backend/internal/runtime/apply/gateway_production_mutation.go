package apply

import (
	"context"
	"fmt"

	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/runtime/vpp"
)

func (adapter *productionGatewayAdapter) deleteDiff(ctx context.Context, diff vpp.GatewayDiff, prior vpp.Snapshot, desired vpp.Plan, previous *vpp.Plan) (vpp.Snapshot, error) {
	switch adapter.name {
	case "interfaces":
		plan := diff.Interfaces
		plan.Interfaces = nil
		result, err := adapter.reconciler.adapter.ApplyInterfaceBond(ctx, plan, prior)
		return result.Readback, err
	case "bonds":
		plan := diff.Bonds
		plan.Bonds = nil
		result, err := adapter.reconciler.adapter.ApplyInterfaceBond(ctx, plan, prior)
		return result.Readback, err
	case "wan-groups":
		plan := diff.WANGroups
		plan.WANGroups = nil
		result, err := adapter.reconciler.adapter.ApplyRouteWANGroup(ctx, plan, prior)
		return result.Readback, err
	case "routes":
		plan := diff.Routes
		plan.Routes = nil
		plan.PriorAddressAssignments = priorAddressAssignments(previous)
		result, err := adapter.reconciler.adapter.ApplyRouteWANGroup(ctx, plan, prior)
		return result.Readback, err
	case "acls":
		plan := diff.ACLs
		plan.ACLs = nil
		plan.IngressVPPInterface = vppGatewayLANInterface(desired)
		result, err := adapter.reconciler.adapter.ApplyACLQoS(ctx, plan, prior)
		return result.Readback, err
	case "qos":
		plan := diff.QoS
		plan.QoS = nil
		plan.IngressVPPInterface = vppGatewayLANInterface(desired)
		result, err := adapter.reconciler.adapter.ApplyACLQoS(ctx, plan, prior)
		return result.Readback, err
	case "nat44":
		plan := diff.NAT44
		plan.StaticMappings = nil
		result, err := adapter.reconciler.adapter.ApplyNAT44(ctx, plan, prior)
		return result.Readback, err
	case "port-maps":
		plan := diff.PortMaps
		plan.PortMappings = nil
		plan.ReadbackStaticMappings = prior.NAT.StaticMappings
		result, err := adapter.reconciler.adapter.ApplyNAT44(ctx, plan, prior)
		return result.Readback, err
	default:
		return vpp.Snapshot{}, fmt.Errorf("unsupported production gateway resource %q", adapter.name)
	}
}

func (adapter *productionGatewayAdapter) applyDiff(ctx context.Context, diff vpp.GatewayDiff, prior vpp.Snapshot, desired vpp.Plan, previous *vpp.Plan) (vpp.Snapshot, error) {
	switch adapter.name {
	case "interfaces":
		plan := diff.Interfaces
		plan.DeleteInterfaces = nil
		result, err := adapter.reconciler.adapter.ApplyInterfaceBond(ctx, plan, prior)
		return result.Readback, err
	case "bonds":
		plan := diff.Bonds
		plan.DeleteBonds = nil
		result, err := adapter.reconciler.adapter.ApplyInterfaceBond(ctx, plan, prior)
		return result.Readback, err
	case "wan-groups":
		plan := diff.WANGroups
		plan.DeleteWANGroups = nil
		result, err := adapter.reconciler.adapter.ApplyRouteWANGroup(ctx, plan, prior)
		if err == nil && adapter.wanGroups != nil {
			*adapter.wanGroups = vpp.NewWANGroupsContext(prior.WANGroups, plan.WANGroups)
		}
		return result.Readback, err
	case "routes":
		plan := diff.Routes
		plan.DeleteRoutes = nil
		// Rollback resolves each restored policy's classifier ingress against
		// the previous generation's LANs, never the failed generation's.
		plan.PriorAddressAssignments = priorAddressAssignments(previous)
		// Route compilation and readback need the complete desired WAN-group
		// inventory even when no group changed in this transaction.  Relying on
		// the preceding wan-groups adapter left this context empty on route-only
		// repairs, so a valid lookup-in-table path was compared with the literal
		// group ID and rejected as drift.
		plan.WANGroupsContext = vpp.NewWANGroupsContext(prior.WANGroups, desired.Policy.WANGroups)
		plan.RoutePolicyContext = append([]trafficpolicy.RoutePolicy(nil), desired.Policy.RoutePolicies...)
		result, err := adapter.reconciler.adapter.ApplyRouteWANGroup(ctx, plan, prior)
		return result.Readback, err
	case "acls":
		plan := diff.ACLs
		plan.DeleteACLs = nil
		plan.IngressVPPInterface = vppGatewayLANInterface(desired)
		result, err := adapter.reconciler.adapter.ApplyACLQoS(ctx, plan, prior)
		return result.Readback, err
	case "qos":
		plan := diff.QoS
		plan.DeleteQoS = nil
		plan.IngressVPPInterface = vppGatewayLANInterface(desired)
		result, err := adapter.reconciler.adapter.ApplyACLQoS(ctx, plan, prior)
		return result.Readback, err
	case "nat44":
		plan := diff.NAT44
		plan.DeleteStaticMappings = nil
		result, err := adapter.reconciler.adapter.ApplyNAT44(ctx, plan, prior)
		return result.Readback, err
	case "port-maps":
		plan := diff.PortMaps
		plan.DeletePortMappings = nil
		plan.ReadbackStaticMappings = desired.NAT.StaticMappings
		result, err := adapter.reconciler.adapter.ApplyNAT44(ctx, plan, prior)
		return result.Readback, err
	default:
		return vpp.Snapshot{}, fmt.Errorf("unsupported production gateway resource %q", adapter.name)
	}
}

func (adapter *productionGatewayAdapter) routeWANGroupsContext() vpp.WANGroupsContext {
	if adapter.wanGroups == nil {
		return vpp.NewWANGroupsContext(nil, nil)
	}
	return *adapter.wanGroups
}

// priorAddressAssignments is the previous generation's LAN inventory, used by
// rollback to bind each restored route policy to the LAN it came from.
func priorAddressAssignments(previous *vpp.Plan) []vpp.AddressAssignment {
	if previous == nil {
		return nil
	}
	return append([]vpp.AddressAssignment(nil), previous.AddressAssignments...)
}
