package vpp

import "testing"

func TestVerifyVPPCTLAbsenceAcceptsEmptyRetainedFIB(t *testing.T) {
	const command = "show ip fib table 86417"
	results := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:86417, fib_index:14, locks:[CLI:1, ]\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: dpo-load-balance\n      [@0]: dpo-drop ip4\n"}}
	if err := verifyVPPCTLAbsence(results, []string{command}, "WAN group", "acceptance-dual-wan"); err != nil {
		t.Fatalf("retained blackhole FIB should prove absence: %v", err)
	}
}

func TestVerifyVPPCTLAbsenceRejectsLiveOutput(t *testing.T) {
	const command = "show ip fib table 86417"
	results := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:86417\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: ipv4 via 198.51.100.1 pppoe0\n"}}
	if err := verifyVPPCTLAbsence(results, []string{command}, "WAN group", "acceptance-dual-wan"); err == nil {
		t.Fatal("live FIB forwarding path must not prove absence")
	}
}
