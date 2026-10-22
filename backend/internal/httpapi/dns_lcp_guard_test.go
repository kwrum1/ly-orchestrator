package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestDNSLCPGuardPlanScopesDropsToLANControlPlaneInterface(t *testing.T) {
	plan := dnsLCPGuardPlan([]vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", CIDR: "192.168.88.1/24"},
		{Role: "wan", LinuxInterface: "eth1", VPPInterface: "lyroute-eth1", CIDR: "100.64.0.2/32"},
	})
	if len(plan.LCPGuardAddresses) != 1 || plan.LCPGuardAddresses[0] != "192.168.88.1" {
		t.Fatalf("guard addresses = %#v", plan.LCPGuardAddresses)
	}
	want := vpp.LANControlPlaneHostInterface("eth2")
	if len(plan.LCPGuardInterfaces) != 1 || plan.LCPGuardInterfaces[0] != want {
		t.Fatalf("guard interfaces = %#v, want %q", plan.LCPGuardInterfaces, want)
	}
	if want == "" || want == "eth2" {
		t.Fatalf("guard interface must be the LCP host interface, got %q", want)
	}
}

func TestDNSLCPGuardPlanFallsBackToVPPInterfaceName(t *testing.T) {
	plan := dnsLCPGuardPlan([]vpp.AddressAssignment{
		{Role: "LAN", VPPInterface: "lyroute-eth2", CIDR: "192.168.88.1/24"},
	})
	want := vpp.LANControlPlaneHostInterface("eth2")
	if len(plan.LCPGuardInterfaces) != 1 || plan.LCPGuardInterfaces[0] != want {
		t.Fatalf("guard interfaces = %#v, want %q", plan.LCPGuardInterfaces, want)
	}
}
