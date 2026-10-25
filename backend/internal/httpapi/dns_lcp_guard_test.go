package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestDNSLCPGuardPlanScopesDropsToLANControlPlaneInterface(t *testing.T) {
	plan := dnsLCPGuardPlan([]vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", CIDR: "192.168.88.1/24"},
		{Role: "wan", LinuxInterface: "eth1", VPPInterface: "lyroute-eth1", CIDR: "100.64.0.2/32"},
	}, nil)
	if len(plan.LCPGuardAddresses) != 1 || plan.LCPGuardAddresses[0] != "192.168.88.1" {
		t.Fatalf("guard addresses = %#v", plan.LCPGuardAddresses)
	}
	wantLCP := vpp.LANControlPlaneHostInterface("eth2")
	found := map[string]bool{}
	for _, name := range plan.LCPGuardInterfaces {
		found[name] = true
	}
	if !found["eth2"] || !found[wantLCP] {
		t.Fatalf("guard interfaces = %#v, want eth2 and %q", plan.LCPGuardInterfaces, wantLCP)
	}
	if wantLCP == "" || wantLCP == "eth2" {
		t.Fatalf("guard interface must include the LCP host interface, got %q", wantLCP)
	}
}

func TestDNSLCPGuardPlanFallsBackToVPPInterfaceName(t *testing.T) {
	plan := dnsLCPGuardPlan([]vpp.AddressAssignment{
		{Role: "LAN", VPPInterface: "lyroute-eth2", CIDR: "192.168.88.1/24"},
	}, nil)
	wantLCP := vpp.LANControlPlaneHostInterface("eth2")
	found := map[string]bool{}
	for _, name := range plan.LCPGuardInterfaces {
		found[name] = true
	}
	if !found["eth2"] || !found[wantLCP] {
		t.Fatalf("guard interfaces = %#v, want eth2 and %q", plan.LCPGuardInterfaces, wantLCP)
	}
}

func TestDNSLCPGuardPlanCoversDNSServiceHostTaps(t *testing.T) {
	network, err := vpp.DNSServiceNetworkForUpstreamID("acc-b-dns-upstream", "acc-b-wan", []string{"100.77.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	plan := dnsLCPGuardPlan([]vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24"},
	}, []vpp.DNSServiceNetwork{network})
	wantLCP := vpp.LANControlPlaneHostInterface("enp0s4")
	found := map[string]bool{}
	for _, name := range plan.LCPGuardInterfaces {
		found[name] = true
	}
	if !found[wantLCP] || !found[network.HostInterface] {
		t.Fatalf("guard interfaces = %#v, want %q and %q", plan.LCPGuardInterfaces, wantLCP, network.HostInterface)
	}
}
