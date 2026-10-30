package takeover

import "testing"

func TestRuleLibraryHitsVerifiedFamilies(t *testing.T) {
	cases := []struct {
		name          string
		driver        string
		device        string
		wantID        string
		wantPreferred Mode
		wantVerified  bool
	}{
		{name: "igc i226", driver: "igc", device: "8086:125c", wantID: "igc-dpdk-uio", wantPreferred: ModeDPDKUIO, wantVerified: false},
		{name: "igb i210", driver: "igb", device: "8086:1533", wantID: "igb-dpdk-uio", wantPreferred: ModeDPDKUIO, wantVerified: false},
		{name: "igc by driver only", driver: "igc", device: "", wantID: "igc-dpdk-uio", wantPreferred: ModeDPDKUIO, wantVerified: false},
		{name: "mlx5 cx5", driver: "mlx5_core", device: "15b3:1017", wantID: "mlx5-connectx4-5-rdma", wantPreferred: ModeRDMA, wantVerified: true},
		{name: "mlx5 cx4", driver: "mlx5_core", device: "15b3:1013", wantID: "mlx5-connectx4-5-rdma", wantPreferred: ModeRDMA, wantVerified: true},
		{name: "ixgbe", driver: "ixgbe", device: "", wantID: "ixgbe-dpdk-uio", wantPreferred: ModeDPDKUIO, wantVerified: false},
		{name: "virtio", driver: "virtio_net", device: "1af4:1000", wantID: "virtio-compat", wantPreferred: ModeAFPacket, wantVerified: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rule, ok := MatchRule(testCase.driver, testCase.device)
			if !ok {
				t.Fatalf("MatchRule(%q, %q) = no rule", testCase.driver, testCase.device)
			}
			if rule.ID != testCase.wantID || rule.Preferred != testCase.wantPreferred || rule.Verified != testCase.wantVerified {
				t.Fatalf("rule = %#v, want id=%s preferred=%s verified=%t", rule, testCase.wantID, testCase.wantPreferred, testCase.wantVerified)
			}
		})
	}
}

func TestRuleLibraryMarksOnlyVerifiedZeroCopyAndRDMAAsFirstPriority(t *testing.T) {
	for _, rule := range Rules() {
		if !rule.Verified {
			continue
		}
		switch rule.Preferred {
		case ModeRDMA, ModeAFXDPZeroCopy:
		default:
			t.Fatalf("rule %s is verified but prefers %s; only validated zero-copy/RDMA modes may skip the probe", rule.ID, rule.Preferred)
		}
	}
	rule, ok := MatchRule("mlx5_core", "15b3:1017")
	if !ok || !rule.Verified || rule.Preferred != ModeRDMA {
		t.Fatalf("mlx5 CX5 rule = %#v, %t", rule, ok)
	}
	if igc, ok := MatchRule("igc", "8086:125c"); !ok || igc.Verified {
		t.Fatalf("igc rule must not be marked verified (i225/i226 zero-copy is unproven): %#v", igc)
	}
	if igb, ok := MatchRule("igb", "8086:1533"); !ok || igb.Verified || igb.Preferred != ModeDPDKUIO {
		t.Fatalf("igb rule must default to DPDK UIO, not a verified zero-copy path: %#v", igb)
	}
}

func TestRuleLibraryForbidsAFXDPForIGBAndIGCAndE1000EAndR8169(t *testing.T) {
	for _, driver := range []string{"igb", "igc", "e1000e", "r8169"} {
		for _, mode := range []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy} {
			if ModeAllowed(driver, "", mode) {
				t.Fatalf("ModeAllowed(%q, %q) = true; AF_XDP must be forbidden", driver, mode)
			}
			if rule, ok := MatchRule(driver, ""); ok && rule.Allows(mode) {
				t.Fatalf("rule %s allows %s", rule.ID, mode)
			}
		}
		if !ModeAllowed(driver, "", ModeAFPacket) {
			t.Fatalf("ModeAllowed(%q, af_packet) = false; compatibility must always be available", driver)
		}
	}
	if !ModeAllowed("igc", "8086:125c", ModeDPDKUIO) {
		t.Fatal("igc must allow DPDK UIO")
	}
	if ModeAllowed("igc", "8086:125c", ModeDPDKVFIO) {
		t.Fatal("igc must not allow a VFIO bind")
	}
	if ModeAllowed("r8169", "", ModeDPDKUIO) {
		t.Fatal("r8169 has no PMD and must not allow DPDK")
	}
}

func TestUnknownHardwareNeverAllowsAFXDP(t *testing.T) {
	for _, mode := range []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA} {
		if ModeAllowed("bnxt_en", "14e4:16d7", mode) {
			t.Fatalf("unknown hardware was allowed %s", mode)
		}
		if ModeAllowed("usbnet", "", mode) {
			t.Fatalf("unknown non-PMD hardware was allowed %s", mode)
		}
	}
	if !ModeAllowed("bnxt_en", "", ModeDPDKUIO) {
		t.Fatal("an unknown driver with a PMD must be allowed DPDK")
	}
	if ModeAllowed("usbnet", "", ModeDPDKUIO) {
		t.Fatal("an unknown driver without a PMD must not be allowed DPDK")
	}
}

func TestUnknownHardwareUsesTheDPDKPMDTable(t *testing.T) {
	if _, ok := MatchRule("bnxt_en", ""); ok {
		t.Fatal("bnxt_en unexpectedly has a rule library entry")
	}
	if !HasDPDKPMD("bnxt_en") {
		t.Fatal("bnxt_en must be listed in the DPDK PMD table")
	}
	if HasDPDKPMD("usbnet") {
		t.Fatal("usbnet must not be listed in the DPDK PMD table")
	}
	if got := ParseDeviceID("0x8086", "0x125C"); got != "8086:125c" {
		t.Fatalf("ParseDeviceID = %q", got)
	}
}
