package apply

import (
	"context"
	"strings"

	"ly-route/backend/internal/runtime/flow"
	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/runtime/vpp"
)

type desiredLiveClient struct {
	state      vpp.Snapshot
	operations []vpp.Operation
}

func (client *desiredLiveClient) OpenChannel(context.Context) (vpp.Channel, error) {
	return &desiredLiveChannel{client: client}, nil
}

func (client *desiredLiveClient) mutations() []vpp.Operation {
	mutations := make([]vpp.Operation, 0)
	for _, operation := range client.operations {
		name := operation.Name
		if name == "vpp.tap.cleanup" || strings.HasSuffix(name, ".snapshot") || strings.Contains(name, ".rollback") || isRouteStagingOperation(name) {
			continue
		}
		mutations = append(mutations, operation)
	}
	return mutations
}

// isRouteStagingOperation reports internal steps of the staged route rebuild.
// They move private FIB tables and pre-clean the same route identity; they are
// not additional resource mutations, so the dependency-order assertions count
// the replay operation as the route mutation.
func isRouteStagingOperation(name string) bool {
	return strings.HasPrefix(name, "vpp.route-table.") || strings.HasSuffix(name, ".pre-delete")
}

type desiredLiveChannel struct{ client *desiredLiveClient }

func (channel *desiredLiveChannel) Do(_ context.Context, operation vpp.Operation) (vpp.Reply, error) {
	channel.client.operations = append(channel.client.operations, operation)
	if operation.Name == "vpp.tap.cleanup" {
		return vpp.Reply{Payload: vpp.VPPCTLReplyPayload{CommandResults: []vpp.VPPCTLCommandResult{{Command: "show tap"}}}}, nil
	}
	if strings.HasSuffix(operation.Name, ".snapshot") {
		return channel.snapshot(operation), nil
	}
	channel.mutate(operation)
	return vpp.Reply{}, nil
}

func (channel *desiredLiveChannel) Close() error { return nil }

func (channel *desiredLiveChannel) snapshot(operation vpp.Operation) vpp.Reply {
	request, _ := operation.Payload.(vpp.SnapshotRequest)
	switch operation.Name {
	case "vpp.interface.snapshot":
		return vpp.Reply{Payload: vpp.InterfaceReadback{Interfaces: selectByID(channel.client.state.Interfaces, request.Interfaces, func(item vpp.InterfaceState) string { return item.Name })}}
	case "vpp.interface-bond.snapshot":
		return vpp.Reply{Payload: vpp.BondReadback{Bonds: selectByID(channel.client.state.Bonds, request.Bonds, func(item vpp.BondState) string { return item.Name })}}
	case "vpp.pbr.next-hop-group.snapshot":
		return vpp.Reply{Payload: vpp.WANGroupReadback{Groups: selectByID(channel.client.state.WANGroups, request.WANGroups, func(item trafficpolicy.WANGroup) string { return item.ID })}}
	case "vpp.route-policy.snapshot":
		return vpp.Reply{Payload: vpp.RoutePolicyReadback{Policies: selectByID(channel.client.state.RoutePolicies, request.RoutePolicies, func(item trafficpolicy.RoutePolicy) string { return item.ID })}}
	case "vpp.security-acl.snapshot":
		return vpp.Reply{Payload: vpp.ACLReadback{ACLs: selectByID(channel.client.state.ACLs, request.ACLs, func(item trafficpolicy.SecurityACL) string { return item.ID })}}
	case "vpp.qos.snapshot":
		return vpp.Reply{Payload: vpp.QoSReadback{Groups: selectByID(channel.client.state.QoS, request.QoS, func(item flow.VPPObjectGroup) string { return item.Kind })}}
	case "vpp.nat44-ed.snapshot":
		return vpp.Reply{Payload: vpp.NAT44Readback{StaticMappings: selectByID(channel.client.state.NAT.StaticMappings, request.NATStaticMappings, func(item nat.StaticMapping) string { return item.ID }), PortMappings: selectByID(channel.client.state.NAT.PortMappings, request.NATPortMappings, func(item nat.PortMapping) string { return item.ID })}}
	default:
		return vpp.Reply{}
	}
}

func (channel *desiredLiveChannel) mutate(operation vpp.Operation) {
	switch operation.Name {
	case "vpp.interface.address":
		channel.client.state.Interfaces = mutateByID(channel.client.state.Interfaces, operation, func(item vpp.InterfaceState) string { return item.Name })
	case "vpp.interface-bond":
		channel.client.state.Bonds = mutateByID(channel.client.state.Bonds, operation, func(item vpp.BondState) string { return item.Name })
	case "vpp.pbr.next-hop-group":
		channel.client.state.WANGroups = mutateByID(channel.client.state.WANGroups, operation, func(item trafficpolicy.WANGroup) string { return item.ID })
	case "vpp.route-policy":
		if strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "abf attach ip4 del") {
			channel.client.state.RoutePolicies = removeByID(channel.client.state.RoutePolicies, operation.Resource, func(item trafficpolicy.RoutePolicy) string { return item.ID })
		} else {
			channel.client.state.RoutePolicies = mutateByID(channel.client.state.RoutePolicies, operation, func(item trafficpolicy.RoutePolicy) string { return item.ID })
		}
	case "vpp.security-acl":
		if strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "delete acl-plugin acl") {
			channel.client.state.ACLs = removeByID(channel.client.state.ACLs, operation.Resource, func(item trafficpolicy.SecurityACL) string { return item.ID })
		} else {
			channel.client.state.ACLs = mutateByID(channel.client.state.ACLs, operation, func(item trafficpolicy.SecurityACL) string { return item.ID })
		}
	case "vpp.route-policy.pre-delete":
		// The staged rebuild removes every live policy before replaying the
		// desired chain, so its readback must observe the object as gone.
		channel.client.state.RoutePolicies = removeByID(channel.client.state.RoutePolicies, operation.Resource, func(item trafficpolicy.RoutePolicy) string { return item.ID })
	case "vpp.route-policy.replay":
		channel.client.state.RoutePolicies = mutateByID(channel.client.state.RoutePolicies, operation, func(item trafficpolicy.RoutePolicy) string { return item.ID })
	case "vpp.route-table.prepare", "vpp.route-table.populate", "vpp.nat44.initialize":
		// Private FIB tables and NAT plugin state are already represented by
		// the typed snapshot; these staged operations only carry commands.
	case "vpp.qos":
		if strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "delete") || strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "disable") || strings.Contains(strings.Join(operation.VPPCtlCommands, " "), "policer del") {
			channel.client.state.QoS = removeByID(channel.client.state.QoS, operation.Resource, func(item flow.VPPObjectGroup) string { return item.Kind })
		} else {
			channel.client.state.QoS = mutateByID(channel.client.state.QoS, operation, func(item flow.VPPObjectGroup) string { return item.Kind })
		}
	case "vpp.nat44-ed.static-mapping":
		if natMappingDeleteOnly(operation, "static mapping") {
			channel.client.state.NAT.StaticMappings = removeByID(channel.client.state.NAT.StaticMappings, operation.Resource, func(item nat.StaticMapping) string { return item.ID })
		} else {
			channel.client.state.NAT.StaticMappings = mutateByID(channel.client.state.NAT.StaticMappings, operation, func(item nat.StaticMapping) string { return item.ID })
		}
	case "vpp.nat44-ed.port-map":
		if natMappingDeleteOnly(operation, "static mapping") {
			channel.client.state.NAT.PortMappings = removeByID(channel.client.state.NAT.PortMappings, operation.Resource, func(item nat.PortMapping) string { return item.ID })
		} else {
			channel.client.state.NAT.PortMappings = mutateByID(channel.client.state.NAT.PortMappings, operation, func(item nat.PortMapping) string { return item.ID })
		}
	}
}

func removeByID[T any](items []T, id string, identity func(T) string) []T {
	result := make([]T, 0, len(items))
	for _, item := range items {
		if identity(item) != id {
			result = append(result, item)
		}
	}
	return result
}

func selectByID[T any](items []T, ids []string, identity func(T) string) []T {
	if len(ids) == 0 {
		return cloneSlice(items)
	}
	selected := make([]T, 0, len(ids))
	for _, id := range ids {
		for _, item := range items {
			if identity(item) == id {
				selected = append(selected, item)
			}
		}
	}
	return selected
}

func mutateByID[T any](items []T, operation vpp.Operation, identity func(T) string) []T {
	updated := make([]T, 0, len(items)+1)
	for _, item := range items {
		if identity(item) != operation.Resource {
			updated = append(updated, item)
		}
	}
	if payload, ok := operation.Payload.(T); ok {
		updated = append(updated, payload)
	}
	return updated
}

func natMappingDeleteOnly(operation vpp.Operation, marker string) bool {
	hasApply := false
	for _, command := range operation.VPPCtlCommands {
		command = strings.TrimSpace(strings.TrimPrefix(command, "?"))
		if strings.Contains(command, "add "+marker) && !strings.HasSuffix(command, " del") {
			hasApply = true
			break
		}
	}
	return !hasApply
}

func mutationClasses(operations []vpp.Operation) []string {
	classes := make([]string, 0, len(operations))
	for _, operation := range operations {
		classes = append(classes, map[string]string{"vpp.interface.address": "interfaces", "vpp.interface-bond": "bonds", "vpp.pbr.next-hop-group": "wan-groups", "vpp.route-policy": "routes", "vpp.route-policy.replay": "routes", "vpp.route-policy.pre-delete": "routes", "vpp.security-acl": "acls", "vpp.qos": "qos", "vpp.nat44-ed.static-mapping": "nat44", "vpp.nat44-ed.port-map": "port-maps"}[operation.Name])
	}
	return classes
}
