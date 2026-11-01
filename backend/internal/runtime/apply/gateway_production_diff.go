package apply

import "ly-route/backend/internal/runtime/vpp"

func gatewayDiffApplies(name string, diff vpp.GatewayDiff) bool {
	return gatewayDiffHasDelete(name, diff) || gatewayDiffHasApply(name, diff)
}

func gatewayDiffHasDelete(name string, diff vpp.GatewayDiff) bool {
	switch name {
	case "interfaces":
		return len(diff.Interfaces.DeleteInterfaces) > 0
	case "bonds":
		return len(diff.Bonds.DeleteBonds) > 0 || len(diff.Bonds.DeleteInterfaces) > 0
	case "wan-groups":
		return len(diff.WANGroups.DeleteWANGroups) > 0
	case "routes":
		return len(diff.Routes.DeleteRoutes) > 0
	case "acls":
		return len(diff.ACLs.DeleteACLs) > 0
	case "qos":
		return len(diff.QoS.DeleteQoS) > 0
	case "nat44":
		return len(diff.NAT44.DeleteStaticMappings) > 0 || len(diff.NAT44.DeletePoolAddresses) > 0 || len(diff.NAT44.DeleteInterfaces) > 0
	case "port-maps":
		return len(diff.PortMaps.DeletePortMappings) > 0
	default:
		return false
	}
}

func gatewayDiffHasApply(name string, diff vpp.GatewayDiff) bool {
	switch name {
	case "interfaces":
		return len(diff.Interfaces.Interfaces) > 0
	case "bonds":
		return len(diff.Bonds.Bonds) > 0 || len(diff.Bonds.Interfaces) > 0
	case "wan-groups":
		return len(diff.WANGroups.WANGroups) > 0
	case "routes":
		return len(diff.Routes.Routes) > 0
	case "acls":
		return len(diff.ACLs.ACLs) > 0
	case "qos":
		return len(diff.QoS.QoS) > 0
	case "nat44":
		// The static WAN pool is part of NAT44 initialization even when no
		// static/port mapping changed this generation. Re-running the
		// idempotent initialize keeps every WAN's own outside address bound
		// after a WAN-group membership/config change.
		//
		// A LAN-only generation must not schedule this node: it has no NAT44
		// object, and the evidence validator rejects a NAT44 resource that the
		// persisted plan does not contain. The LAN inside role is reconciled by
		// the LAN control-plane lifecycle instead.
		return len(diff.NAT44.StaticMappings) > 0 || len(diff.NAT44.PoolAddresses) > 0
	case "port-maps":
		return len(diff.PortMaps.PortMappings) > 0
	default:
		return false
	}
}
