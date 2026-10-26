package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

// VPP selects the NAT44 outside address from the packet's TX interface only
// when the pool address belongs to the packet's inside FIB. Interface-address
// pool entries keep fib_index ~0, so with several WANs VPP always chose the
// last pool address. Static WAN addresses must be bound to the inside VRF and
// every WAN must be bound as an outside interface.
func TestNAT44InitializeBindsEveryStaticWANAddressToInsideVRF(t *testing.T) {
	plan := provenPlan(Plan{
		RequestID: "req-multi-wan-nat",
		AddressAssignments: []AddressAssignment{
			{ID: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24", Role: "lan"},
			{ID: "wan1", LinuxInterface: "enp0s3", VPPInterface: "lyroute-enp0s3", CIDR: "100.77.0.2/24", Role: "wan"},
			{ID: "wan2", LinuxInterface: "enp0s5", VPPInterface: "lyroute-enp0s5", CIDR: "100.78.0.2/24", Role: "wan"},
		},
		NAT: nat.CompiledConfig{
			Behavior: nat.BehaviorEndpointDependent,
			PortMappings: []nat.PortMapping{{
				ID:              "pm1",
				WANInterface:    "lyroute-enp0s3",
				ExternalAddress: "100.77.0.1",
				ExternalPort:    38180,
				InternalHost:    "192.168.77.50",
				InternalPort:    38088,
				Protocol:        "tcp",
			}},
		},
	}, "enp0s3", "enp0s4", "enp0s5")
	operations := mustBuildOperations(t, plan)
	var initialize string
	for _, operation := range operations {
		if operation.Name == "vpp.nat44.initialize" {
			initialize = commandText(operation.VPPCtlCommands)
			break
		}
	}
	if initialize == "" {
		t.Fatal("NAT44 initialize operation is missing")
	}
	for _, required := range []string{
		"?set interface nat44 in lyroute-enp0s4",
		"set interface nat44 out lyroute-enp0s3 output-feature",
		"set interface nat44 out lyroute-enp0s5 output-feature",
		"?nat44 add interface address lyroute-enp0s3 del",
		"?nat44 add interface address lyroute-enp0s5 del",
		"?nat44 add address 100.77.0.2 tenant-vrf 0",
		"?nat44 add address 100.78.0.2 tenant-vrf 0",
	} {
		if !strings.Contains(initialize, required) {
			t.Fatalf("NAT44 initialize is missing %q:\n%s", required, initialize)
		}
	}
	// The interface-address form must not be (re)installed for a static WAN:
	// that form keeps fib_index ~0 and reintroduces the failover bug.
	for _, forbidden := range []string{
		"\n?nat44 add interface address lyroute-enp0s3\n",
		"\n?nat44 add interface address lyroute-enp0s5\n",
	} {
		if strings.Contains(initialize, forbidden) {
			t.Fatalf("NAT44 initialize kept the interface-address pool form %q:\n%s", forbidden, initialize)
		}
	}
	for _, required := range []string{
		"?set interface nat44 in lyroute-enp0s4",
		"set interface nat44 out lyroute-enp0s3 output-feature",
		"set interface nat44 out lyroute-enp0s5 output-feature",
	} {
		if !strings.Contains(initialize, required) {
			t.Fatalf("NAT44 initialize is missing %q:\n%s", required, initialize)
		}
	}
}

func TestNAT44InitializeKeepsRuntimeAddressFormForUnknownWAN(t *testing.T) {
	plan := provenPlan(Plan{
		RequestID: "req-pppoe-nat",
		AddressAssignments: []AddressAssignment{
			{ID: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24", Role: "lan"},
		},
		NAT: nat.CompiledConfig{
			Behavior:     nat.BehaviorEndpointDependent,
			PortMappings: []nat.PortMapping{{ID: "pm", WANInterface: "pppoe_session_wan", ExternalPort: 1, InternalPort: 2, Protocol: "tcp"}},
		},
	}, "enp0s4")
	operations := mustBuildOperations(t, plan)
	for _, operation := range operations {
		if operation.Name != "vpp.nat44.initialize" {
			continue
		}
		text := commandText(operation.VPPCtlCommands)
		if !strings.Contains(text, "?nat44 add interface address pppoe_session_wan") {
			t.Fatalf("runtime-addressed WAN lost its interface-address pool form:\n%s", text)
		}
		return
	}
	t.Fatal("NAT44 initialize operation is missing")
}

func TestPlanRequiresNATForNATRoutePolicy(t *testing.T) {
	plan := Plan{Policy: trafficpolicy.Config{RoutePolicies: []trafficpolicy.RoutePolicy{{ID: "nat-route", Action: "nat"}}}}
	if !planRequiresNAT(plan) {
		t.Fatal("a NAT route policy requires NAT44 initialization even without port mappings")
	}
	plan.Policy = trafficpolicy.Config{RoutePolicies: []trafficpolicy.RoutePolicy{{ID: "plain-route", Action: "route"}}}
	if planRequiresNAT(plan) {
		t.Fatal("a plain route policy must not force NAT44 initialization")
	}
}

func TestNATOutsideInterfacesIncludesEveryWANAssignment(t *testing.T) {
	plan := Plan{AddressAssignments: []AddressAssignment{
		{VPPInterface: "lyroute-enp0s4", Role: "lan"},
		{VPPInterface: "lyroute-enp0s3", Role: "wan"},
		{VPPInterface: "lyroute-enp0s5", Role: "wan"},
	}}
	outside := natOutsideInterfaces(plan)
	if len(outside) != 2 || outside[0] != "lyroute-enp0s3" || outside[1] != "lyroute-enp0s5" {
		t.Fatalf("outside interfaces = %#v, want both WAN assignments", outside)
	}
}
