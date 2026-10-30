package vpp

import "testing"

// A gateway with a WAN outside interface and a LAN ingress is a NAT gateway even
// when no port mapping / static NAT object exists yet: without NAT44 bindings
// LAN clients have no SNAT and nothing leaves the line.
func TestPlanRequiresNATForPlainRoutedGateway(t *testing.T) {
	t.Parallel()
	plan := Plan{
		LANVPPInterfaces: []string{"lyroute-enp0s4"},
		AddressAssignments: []AddressAssignment{
			{VPPInterface: "lyroute-enp0s4", Role: "lan"},
			{VPPInterface: "lyroute-enp0s3", Role: "wan"},
		},
	}
	if !planRequiresNAT(plan) {
		t.Fatal("a WAN outside plus a LAN ingress must initialise NAT44")
	}

	wanOnly := Plan{AddressAssignments: []AddressAssignment{{VPPInterface: "lyroute-enp0s3", Role: "wan"}}}
	if planRequiresNAT(wanOnly) {
		t.Fatal("a WAN without a LAN ingress must not initialise NAT44")
	}
	lanOnly := Plan{LANVPPInterfaces: []string{"lyroute-enp0s4"},
		AddressAssignments: []AddressAssignment{{VPPInterface: "lyroute-enp0s4", Role: "lan"}}}
	if planRequiresNAT(lanOnly) {
		t.Fatal("a LAN without a WAN outside interface must not initialise NAT44")
	}

	// A port mapping still implies NAT, and NAT44 inside/outside must be the
	// plan's LAN ingress and every WAN outside interface.
	mapped := plan
	mapped.NAT.PortMappings = append(mapped.NAT.PortMappings[:0:0], mapped.NAT.PortMappings...)
	outside := natOutsideInterfaces(plan)
	if len(outside) != 1 || outside[0] != "lyroute-enp0s3" {
		t.Fatalf("outside interfaces = %v", outside)
	}
}
