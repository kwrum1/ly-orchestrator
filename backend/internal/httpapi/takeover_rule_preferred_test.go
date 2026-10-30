package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// WS-AK: an igb/igc port whose rule selects dpdk_uio must get that one
// transactional attempt. It must not be adopted as AF_PACKET first and then
// frozen there by the "never upgrade an existing mode" guard.

type rulePreferredFixture struct {
	server *Server
	root   string
}

func newRulePreferredFixture(t *testing.T, iface, pci, driver, vendor, device string) rulePreferredFixture {
	t.Helper()
	root := t.TempDir()
	netdev := filepath.Join(root, "class/net", iface)
	deviceDir := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers", driver)
	uioDir := filepath.Join(root, "bus/pci/drivers/uio_pci_generic")
	for _, directory := range []string{netdev, deviceDir, driverDir, uioDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(deviceDir, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driverDir, filepath.Join(deviceDir, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "device"), []byte(device+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "address"), []byte("00:aa:bb:cc:dd:01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepare := filepath.Join(root, "prepare-vfio.sh")
	if err := os.WriteFile(prepare, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vppctl := filepath.Join(root, "vppctl")
	if err := os.WriteFile(vppctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "enp1s0")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "4096")
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_DPDK_ATTEMPTS", filepath.Join(root, "dpdk-attempts.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(root, "takeover.json"))
	t.Setenv("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", prepare)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_BOOT_ID", "boot-ws-ak")
	resetAllAutomaticModeSwitchBudgetsForTest()
	dataplaneBootstrapBlockedUntil = time.Time{}
	t.Cleanup(func() {
		resetAllAutomaticModeSwitchBudgetsForTest()
		dataplaneBootstrapBlockedUntil = time.Time{}
	})
	return rulePreferredFixture{server: &Server{now: time.Now}, root: root}
}

func TestRulePreferredDPKDFirstAdoptionKeepsDPDKBeforeAFPacket(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	plan := fixture.server.takeoverPlanForPort(context.Background(), "enp2s0")
	primary, ok := plan.Primary()
	if !ok || primary.Mode != "dpdk_uio" {
		t.Fatalf("first-adoption primary = %#v (ok=%t), want dpdk_uio", primary, ok)
	}
	if plan.RuleID != "igb-dpdk-uio" {
		t.Fatalf("rule = %q, want igb-dpdk-uio", plan.RuleID)
	}
	modes := plan.Modes()
	if len(modes) < 2 || modes[len(modes)-1] != "af_packet" {
		t.Fatalf("plan modes = %v, want af_packet as the guaranteed fallback", modes)
	}
	// With no recorded mode the backend plan already selects the rule's first
	// choice, so the watcher/boot path binds DPDK instead of adopting AF_PACKET.
	target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "enp2s0", "", dataplane.TakeoverCapabilityState{})
	if !upgrade || target != "dpdk_uio" {
		t.Fatalf("first takeover = %q (upgrade=%t), want dpdk_uio", target, upgrade)
	}
}

func TestRulePreferredMigrationMigratesAnExistingAFPacketPortOnce(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "enp2s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{})
	if !upgrade || target != takeoverModeDPDKUIO {
		t.Fatalf("af_packet port = %q (upgrade=%t), want a one-shot dpdk_uio migration", target, upgrade)
	}
	// The switch budget is claimed by the caller immediately before it runs.
	// Once claimed, no apply or watcher tick can run the same expensive switch
	// again in this boot.
	if !claimAutomaticModeSwitch("enp2s0") {
		t.Fatal("the first rule-preferred migration could not claim the switch budget")
	}
	if claimAutomaticModeSwitch("enp2s0") {
		t.Fatal("the rule-preferred migration was allowed to run twice in one boot")
	}
}

func TestRulePreferredMigrationHonoursTheDPDKAttemptFailureMarker(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igc", "0x8086", "0x125c")
	recordDPDKAttemptFailed("enp2s0", "0000:02:00.0", takeoverModeDPDKUIO, "VPP did not start with the DPDK device")
	if failed, reason := dpdkAttemptFailure("0000:02:00.0", "enp2s0"); !failed || !strings.Contains(reason, "VPP did not start") {
		t.Fatalf("failure marker = %t %q", failed, reason)
	}
	if target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "enp2s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{}); upgrade {
		t.Fatalf("a port with dpdk_attempt_failed was migrated again to %q", target)
	}
	// A real DPDK success clears the marker; a later boot may try the rule's
	// first choice again.
	recordDPDKAttemptSucceeded("enp2s0", "0000:02:00.0", takeoverModeDPDKUIO)
	if failed, _ := dpdkAttemptFailure("0000:02:00.0", "enp2s0"); failed {
		t.Fatal("a successful DPDK attachment left the failure marker behind")
	}
}

func TestRulePreferredMigrationSkipsTheManagementPort(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp1s0", "0000:01:00.0", "igb", "0x8086", "0x1533")
	plan := fixture.server.takeoverPlanForPort(context.Background(), "enp1s0")
	if plan.Skipped == "" || len(plan.Candidates) != 0 {
		t.Fatalf("management plan = %#v, want it skipped", plan)
	}
	if target, ok := fixture.server.rulePreferredMigrationCandidate(context.Background(), "enp1s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{}); ok {
		t.Fatalf("management port was offered a migration to %q", target)
	}
}

func TestFirstBindFailureRecordsDPDKAttemptFailedAndStopsRetrying(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	fixture.server.recordTakeoverFirstBindFailures([]dpdkTakeoverFailure{{
		LinuxInterface: "enp2s0",
		PCIAddress:     "0000:02:00.0",
		Mode:           takeoverModeDPDKUIO,
		Reason:         "the DPDK PMD rejected the device",
		Crashed:        false,
	}}, map[string]bool{"enp2s0": true})
	raw, err := os.ReadFile(filepath.Join(fixture.root, "dpdk-attempts.json"))
	if err != nil {
		t.Fatalf("no durable attempt record: %v", err)
	}
	if !strings.Contains(string(raw), `"dpdk_attempt_failed": true`) {
		t.Fatalf("attempt record = %s", raw)
	}
	if target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "enp2s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{}); upgrade {
		t.Fatalf("a failed first bind was retried as %q", target)
	}
}

func TestRulePreferredMigrationIgnoresCompatibilityOnlyFamilies(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp5s0", "0000:05:00.0", "r8169", "0x10ec", "0x8168")
	if target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "enp5s0", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{}); upgrade {
		t.Fatalf("r8169 is a compatibility-only family but was migrated to %q", target)
	}
}

func TestApplyRunsTheRulePreferredMigrationForAnOwnedAFPacketPort(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface:     "enp2s0",
		PCIAddress:         "0000:02:00.0",
		Mode:               takeoverModeAFPacket,
		Verification:       "verified",
		FunctionalVerified: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if !fixture.server.autoTakeoverPending(context.Background()) {
		t.Fatal("an owned af_packet igb port was not flagged as owing the rule-preferred DPDK attempt")
	}
}
