package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/vpp"
)

func (fixture afxdpSwitchFixture) mode(t *testing.T) string {
	t.Helper()
	return fixture.stateValue(t, "mode")
}

func (fixture afxdpSwitchFixture) xdpProgram(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fixture.xdp)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "prog="))
}

func (fixture afxdpSwitchFixture) commandLogIfAny(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fixture.log)
	if err != nil {
		return ""
	}
	return string(data)
}

func (fixture afxdpSwitchFixture) useKernelDriver(t *testing.T, driver string) {
	t.Helper()
	device := filepath.Join(fixture.root, "bus/pci/devices/0000:13:00.0")
	driverDirectory := filepath.Join(fixture.root, "bus/pci/drivers", driver)
	if err := os.MkdirAll(driverDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(device, "driver"))
	if err := os.Symlink(driverDirectory, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
}

func TestIGCLiveAFXDPIsMigratedToAFPacketSafely(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	fixture.useKernelDriver(t, "igc")
	fixture.mutateState(t, map[string]string{"mode": "af_xdp", "rows": "1", "host": "0", "sw": "9", "lcp": "lylan-eth1"})
	fixture.setXDP(t, "id:42")
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface: "eth1", PCIAddress: "0000:13:00.0", Mode: takeoverModeAFXDPCopy, Verification: "verified", FunctionalVerified: true,
	}}); err != nil {
		t.Fatal(err)
	}
	staleLog := filepath.Join(fixture.root, "stale-modes.jsonl")
	t.Setenv("LY_ROUTE_DATAPLANE_STALE_MODES", staleLog)
	resetAllAutomaticModeSwitchBudgetsForTest()
	t.Cleanup(resetAllAutomaticModeSwitchBudgetsForTest)

	fixture.server.migrateForbiddenTakeoverModes(context.Background())

	log := fixture.commandLog(t)
	for _, want := range []string{
		"delete interface af_xdp sw_if_index 9",
		"link set dev eth1 xdp off",
		"create host-interface name eth1 cksum-gso-disable",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("migration did not run %q:\n%s", want, log)
		}
	}
	if strings.Index(log, "delete interface af_xdp sw_if_index 9") > strings.Index(log, "create host-interface name eth1") {
		t.Fatalf("AF_XDP was not torn down before AF_PACKET was created:\n%s", log)
	}
	if got := fixture.mode(t); got != takeoverModeAFPacket {
		t.Fatalf("live mode = %q, want af_packet", got)
	}
	if got := fixture.xdpProgram(t); got != "none" {
		t.Fatalf("XDP program = %q, want detached", got)
	}
	entries := fixture.verification(t)
	if len(entries) != 1 || entries[0].Mode != takeoverModeAFPacket {
		t.Fatalf("verification = %#v, want one af_packet proof", entries)
	}
	record, err := os.ReadFile(staleLog)
	if err != nil || !strings.Contains(string(record), "stale_mode_dropped") {
		t.Fatalf("stale-mode evidence = %q, err=%v", record, err)
	}
}

func TestRuleGuardKeepsAnAllowedDPDKProofForIGC(t *testing.T) {
	newTakeoverFixture(t, "eth1", "0000:13:00.0", "igc", "1", 1000, true)
	server := &Server{}
	request := takeoverSelectionRequest(time.Now().UTC())
	dpdk := vpp.CapabilityProof{
		Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO,
		PCIAddress: "0000:13:00.0", KernelDriver: "igc",
	}
	request.Assignments[0].Candidates = append(request.Assignments[0].Candidates, dpdk)
	server.enforceTakeoverRuleLibrary(context.Background(), &request)
	found := false
	for _, candidate := range request.Assignments[0].Candidates {
		if candidate.Hook == vpp.NativeHookDPDK && candidate.Mode == vpp.NativeModeDPDKUIO {
			found = true
		}
	}
	if !found {
		t.Fatalf("the allowed DPDK UIO proof was dropped: %#v", request.Assignments[0].Candidates)
	}
}

func TestApplyDoesNotSwitchAnExistingLiveMode(t *testing.T) {
	fixture := newAFXDPSwitchFixture(t)
	fixture.mutateState(t, map[string]string{"mode": "af_packet", "rows": "1", "host": "0", "lcp": "lylan-eth1"})
	fixture.setXDP(t, "none")
	target, upgrade := fixture.server.automaticTakeoverUpgrade(context.Background(), "eth1", takeoverModeAFPacket, dataplane.TakeoverCapabilityState{})
	if upgrade || target != "" {
		t.Fatalf("existing af_packet port was scheduled for an automatic switch to %q", target)
	}
	if fixture.server.autoTakeoverPending(context.Background()) {
		t.Fatal("an apply on an already-owned port was flagged as a first takeover")
	}
	if commands := fixture.commandLogIfAny(t); strings.Contains(commands, "create interface af_xdp") {
		t.Fatalf("an empty apply created an AF_XDP interface:\n%s", commands)
	}
}

func TestAFXDPSwitchIsRejectedWhenTheRuleLibraryForbidsIt(t *testing.T) {
	snapshot := takeoverTestSnapshot()
	snapshot.KernelDriver = "igc"
	snapshot.CurrentMode = takeoverModeAFPacket
	snapshot.ModeConfirmed = true
	transition, err := planTakeoverTransition(snapshot, takeoverModeAFXDPCopy, true, true, supportedDPDKCapability(), time.Now().UTC())
	if err == nil {
		t.Fatal("an igc AF_XDP switch was planned despite the rule library")
	}
	if transition.Status != takeoverStatusRejected || len(transition.Steps) != 0 {
		t.Fatalf("transition = %#v, want a rejected decision with no steps", transition)
	}
	if !strings.Contains(transition.Reason, "forbids") {
		t.Fatalf("reason = %q", transition.Reason)
	}
}

func TestAutomaticModeSwitchBudgetIsOncePerBootPerPort(t *testing.T) {
	resetAllAutomaticModeSwitchBudgetsForTest()
	t.Cleanup(resetAllAutomaticModeSwitchBudgetsForTest)
	t.Setenv("LY_ROUTE_BOOT_ID", "boot-test-a")
	if !claimAutomaticModeSwitch("eth1") {
		t.Fatal("first automatic switch was not allowed")
	}
	if claimAutomaticModeSwitch("eth1") {
		t.Fatal("a second automatic switch was allowed in the same boot")
	}
	if !claimAutomaticModeSwitch("eth2") {
		t.Fatal("the budget must be per port")
	}
	t.Setenv("LY_ROUTE_BOOT_ID", "boot-test-b")
	if !claimAutomaticModeSwitch("eth1") {
		t.Fatal("a new boot must reset the switch budget")
	}
}
