package vpp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GatewaySnapshotMatchesResource compares one typed resource readback with the
// persisted desired generation using the same semantics as commit evidence.
//
// Apply uses this before it returns a resource result, so a mutation that did
// not take effect is a failed transaction (rolled back to the previous
// committed generation) instead of a committed generation whose evidence can
// never match. That distinction is what prevents a role/address edit from
// locking the runtime in reconciled_degraded forever.
func GatewaySnapshotMatchesResource(resource string, snapshot Snapshot, desired Plan) bool {
	switch resource {
	case "interfaces":
		return InterfaceStatesMatchDesired(physicalInterfaceStates(snapshot.Interfaces), physicalInterfaceStates(desired.Interfaces))
	case "bonds":
		return gatewayItemsEqualUnordered(snapshot.Bonds, desired.Bonds)
	case "wan-groups":
		return gatewayItemsEqualUnordered(snapshot.WANGroups, desired.Policy.WANGroups)
	case "routes":
		// VPP readback order is not a policy semantic; priority is a field.
		return gatewayItemsEqualUnordered(snapshot.RoutePolicies, desired.Policy.RoutePolicies)
	case "acls":
		return gatewayItemsEqualUnordered(snapshot.ACLs, desired.Policy.SecurityACLs)
	case "qos":
		return gatewayItemsEqualUnordered(snapshot.QoS, desired.Flow.VPPGroups)
	case "nat44":
		return gatewayItemsEqualUnordered(snapshot.NAT.StaticMappings, desired.NAT.StaticMappings)
	case "port-maps":
		return gatewayItemsEqualUnordered(snapshot.NAT.PortMappings, desired.NAT.PortMappings)
	case "service-networks":
		// DNS service networks and proxy steering are supplemental operations,
		// not rows in the typed snapshot. Their evidence is validated from the
		// supplemental readback by the caller.
		return len(desired.DNSServiceNetworks) > 0 || len(desired.Proxy.VPPSteering) > 0
	default:
		return false
	}
}

// DescribeGatewaySnapshotMismatch renders the first actionable difference so a
// failed apply names the interface/address that did not converge instead of
// only reporting a generic evidence error.
func DescribeGatewaySnapshotMismatch(resource string, snapshot Snapshot, desired Plan) string {
	if resource == "interfaces" {
		liveInterfaces := physicalInterfaceStates(snapshot.Interfaces)
		observed := make(map[string]InterfaceState, len(liveInterfaces))
		for _, state := range liveInterfaces {
			observed[strings.TrimSpace(state.Name)] = state
		}
		physical := physicalInterfaceStates(desired.Interfaces)
		wanted := make(map[string]struct{}, len(physical))
		for _, state := range physical {
			name := strings.TrimSpace(state.Name)
			wanted[name] = struct{}{}
			live, found := observed[name]
			if !found {
				return fmt.Sprintf("interface %s is absent from the live readback", name)
			}
			if !InterfaceStateMatchesDesired(live, state) {
				return fmt.Sprintf("interface %s expected admin=%s link=%s addresses=%v, live admin=%s link=%s addresses=%v",
					name, state.AdminState, state.LinkState, state.Addresses,
					live.AdminState, live.LinkState, live.Addresses)
			}
		}
		extra := make([]string, 0)
		for name := range observed {
			if _, expected := wanted[name]; !expected {
				extra = append(extra, name)
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			return fmt.Sprintf("unexpected live interface(s): %s", strings.Join(extra, ", "))
		}
	}
	switch resource {
	case "bonds":
		return fmt.Sprintf("bonds live=%#v desired=%#v", snapshot.Bonds, desired.Bonds)
	case "wan-groups":
		return fmt.Sprintf("wan-groups live=%#v desired=%#v", snapshot.WANGroups, desired.Policy.WANGroups)
	}
	return fmt.Sprintf("%s live readback differs from the desired generation", resource)
}

// physicalInterfaceStates drops synthetic bond interfaces. The interfaces
// adapter owns physical ports; the bond lifecycle owns bond admin/address and
// its readback. Leaving a bond in the physical comparison made the interfaces
// node fail after the bond adapter had already applied it.
func physicalInterfaceNames(names []string) []string {
	physical := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "bond-") || strings.HasPrefix(name, "BondEthernet") {
			continue
		}
		physical = append(physical, name)
	}
	return physical
}

func physicalInterfaceStates(states []InterfaceState) []InterfaceState {
	physical := make([]InterfaceState, 0, len(states))
	for _, state := range states {
		name := strings.TrimSpace(state.Name)
		if strings.HasPrefix(name, "bond-") || strings.HasPrefix(name, "BondEthernet") {
			continue
		}
		physical = append(physical, state)
	}
	return physical
}

func gatewayItemsEqualUnordered[T any](observed, desired []T) bool {
	if len(observed) != len(desired) {
		return false
	}
	if len(observed) == 0 {
		return true
	}
	encode := func(items []T) ([]string, bool) {
		values := make([]string, 0, len(items))
		for _, item := range items {
			encoded, err := json.Marshal(item)
			if err != nil {
				return nil, false
			}
			values = append(values, string(encoded))
		}
		sort.Strings(values)
		return values, true
	}
	left, leftOK := encode(observed)
	right, rightOK := encode(desired)
	if !leftOK || !rightOK {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
