package takeover

import (
	"reflect"
	"testing"
)

func igcFacts() Facts {
	return Facts{
		Interface:    "enp3s0",
		Driver:       "igc",
		VendorDevice: "8086:125c",
		Physical:     true,
		UIOAvailable: true,
		DPDKPlugin:   true,
		Hugepages:    true,
	}
}

func TestManagementPortIsNeverTakenOver(t *testing.T) {
	facts := igcFacts()
	facts.Interface = "enp2s0"
	facts.Management = true
	plan := BuildPlan(facts)
	if plan.Skipped == "" {
		t.Fatal("management port produced a takeover plan")
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("management port candidates = %v, want none", plan.Modes())
	}
}

func TestSharedManagementCanStillBeDataPort(t *testing.T) {
	facts := igcFacts()
	facts.Management = true
	facts.SharedManagement = true
	plan := BuildPlan(facts)
	if plan.Skipped != "" || len(plan.Candidates) == 0 {
		t.Fatalf("shared management port plan = %#v", plan)
	}
}

func TestNonPhysicalPortIsNotTakenOver(t *testing.T) {
	plan := BuildPlan(Facts{Interface: "br0", Driver: "bridge", Physical: false})
	if plan.Skipped == "" || len(plan.Candidates) != 0 {
		t.Fatalf("non-physical plan = %#v", plan)
	}
}

func TestIGCDecisionIsDPDKThenCompatibilityAndNeverAFXDP(t *testing.T) {
	plan := BuildPlan(igcFacts())
	want := []Mode{ModeDPDKUIO, ModeAFPacket}
	if got := plan.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %v, want %v (AF_XDP must be forbidden for igc)", got, want)
	}
	if plan.Candidates[0].Verified {
		t.Fatal("an unproven DPDK path must not be reported as verified")
	}
	if plan.RuleID != "igc-dpdk-uio" {
		t.Fatalf("rule = %q", plan.RuleID)
	}
	if len(plan.Dropped) != 1 || plan.Dropped[0] != ModeAFXDPCopy {
		t.Fatalf("dropped modes = %v, want af_xdp_copy", plan.Dropped)
	}
}

func TestDPDKUIOLeadsAndVFIOIsOnlyIsolatedAndAllowed(t *testing.T) {
	isolated := igcFacts()
	isolated.VFIOAvailable = true
	isolated.IOMMUIsolated = true
	isolated.VFIOPolicyAllowed = true
	if got, want := BuildPlan(isolated).Modes(), []Mode{ModeDPDKUIO, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("isolated+allowed modes = %v, want %v", got, want)
	}

	// An audit rule that permits VFIO still only gets it when the group is
	// isolated and the transport policy allows it.
	verified := Facts{
		Interface: "enp5s0", Driver: "ixgbe", Physical: true,
		UIOAvailable: true, VFIOAvailable: true, IOMMUIsolated: true,
		VFIOPolicyAllowed: true, DPDKPlugin: true, Hugepages: true,
	}
	if got, want := BuildPlan(verified).Modes(), []Mode{ModeDPDKUIO, ModeDPDKVFIO, ModeAFXDPCopy, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ixgbe isolated+allowed modes = %v, want %v", got, want)
	}
	unisolated := verified
	unisolated.IOMMUIsolated = false
	if got, want := BuildPlan(unisolated).Modes(), []Mode{ModeDPDKUIO, ModeAFXDPCopy, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unisolated modes = %v, want %v", got, want)
	}
	policyOff := verified
	policyOff.VFIOPolicyAllowed = false
	if got, want := BuildPlan(policyOff).Modes(), []Mode{ModeDPDKUIO, ModeAFXDPCopy, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("policy-off modes = %v, want %v", got, want)
	}
}

func TestVerifiedRDMAIsTakenOverDirectly(t *testing.T) {
	facts := Facts{
		Interface:    "enp5s0",
		Driver:       "mlx5_core",
		VendorDevice: "15b3:1017",
		Physical:     true,
		RDMAReady:    true,
		UIOAvailable: true,
		DPDKPlugin:   true,
		Hugepages:    true,
	}
	plan := BuildPlan(facts)
	want := []Mode{ModeRDMA, ModeDPDKUIO, ModeAFXDPCopy, ModeAFPacket}
	if got := plan.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %v, want %v", got, want)
	}
	primary, _ := plan.Primary()
	if !primary.Verified || primary.Mode != ModeRDMA {
		t.Fatalf("primary = %#v, want a verified RDMA takeover", primary)
	}

	// Without the RDMA plugin the verified RDMA path cannot be created; the
	// plan must not promise it and falls through to DPDK.
	facts.RDMAReady = false
	plan = BuildPlan(facts)
	if got, want := plan.Modes(), []Mode{ModeDPDKUIO, ModeAFXDPCopy, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-rdma-plugin modes = %v, want %v", got, want)
	}
}

func TestUnknownNICWithPMDUsesDPDKAndWithoutPMDReliesOnCompatibility(t *testing.T) {
	withPMD := Facts{
		Interface: "enp6s0", Driver: "bnxt_en", VendorDevice: "14e4:16d7", Physical: true,
		UIOAvailable: true, DPDKPlugin: true, Hugepages: true,
	}
	if got, want := BuildPlan(withPMD).Modes(), []Mode{ModeDPDKUIO, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PMD modes = %v, want %v", got, want)
	}

	noPMD := withPMD
	noPMD.Driver = "usbnet"
	if got, want := BuildPlan(noPMD).Modes(), []Mode{ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-PMD modes = %v, want %v", got, want)
	}
}

func TestParavirtualNICNeverGetsDPDK(t *testing.T) {
	facts := Facts{
		Interface: "ens3", Driver: "virtio_net", VendorDevice: "1af4:1000", Physical: true,
		UIOAvailable: true, DPDKPlugin: true, Hugepages: true,
	}
	if got, want := BuildPlan(facts).Modes(), []Mode{ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("virtio modes = %v, want %v", got, want)
	}
}

func TestMissingHugepagesSkipTheDestructiveAttempt(t *testing.T) {
	facts := igcFacts()
	facts.Hugepages = false
	if got, want := BuildPlan(facts).Modes(), []Mode{ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-hugepage modes = %v, want %v", got, want)
	}
}

// The DPDK plugin is disabled in startup.conf until the first bind enables it.
// For a device family whose rule selects DPDK, that flag must not demote the
// rule's first choice to AF_PACKET before the attempt has run (WS-AK).
func TestRulePreferredDPDKOutranksADisabledPlugin(t *testing.T) {
	facts := igcFacts()
	facts.DPDKPlugin = false
	if got, want := BuildPlan(facts).Modes(), []Mode{ModeDPDKUIO, ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-plugin rule-preferred modes = %v, want %v", got, want)
	}
	// Unknown hardware keeps the conservative behaviour: without a shipped
	// plugin there is no PMD evidence and AF_PACKET is the only candidate.
	unknown := Facts{
		Interface: "enp6s0", Driver: "bnxt_en", VendorDevice: "14e4:16d7", Physical: true,
		UIOAvailable: true, DPDKPlugin: false, Hugepages: true,
	}
	if got, want := BuildPlan(unknown).Modes(), []Mode{ModeAFPacket}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no-plugin unknown modes = %v, want %v", got, want)
	}
}

func TestCompatibilityModeIsAlwaysTheLastCandidate(t *testing.T) {
	inputs := []Facts{
		igcFacts(),
		{Interface: "enp6s0", Driver: "usbnet", Physical: true},
		{Interface: "enp5s0", Driver: "mlx5_core", VendorDevice: "15b3:1017", Physical: true, RDMAReady: true, UIOAvailable: true, DPDKPlugin: true, Hugepages: true},
	}
	for _, facts := range inputs {
		plan := BuildPlan(facts)
		modes := plan.Modes()
		if len(modes) == 0 || modes[len(modes)-1] != ModeAFPacket {
			t.Fatalf("plan for %s = %v, want AF_PACKET last", facts.Interface, modes)
		}
	}
}

func TestDisplayContractUsesOnlyThreeLabels(t *testing.T) {
	cases := []struct {
		mode       Mode
		management bool
		repair     bool
		want       Display
	}{
		{mode: ModeDPDKUIO, want: DisplayHighPerformance},
		{mode: ModeAFXDPZeroCopy, want: DisplayHighPerformance},
		{mode: ModeRDMA, want: DisplayHighPerformance},
		{mode: ModeAFPacket, want: DisplayCompatibility},
		{mode: ModeAFPacket, management: true, want: DisplayManagement}, {mode: ModeDPDKUIO, repair: true, want: DisplayRepair},
		{mode: "", want: DisplayPending},
	}
	for _, testCase := range cases {
		if got := DisplayFor(testCase.mode, testCase.management, testCase.repair); got != testCase.want {
			t.Fatalf("DisplayFor(%s, %t, %t) = %s, want %s", testCase.mode, testCase.management, testCase.repair, got, testCase.want)
		}
	}
}
