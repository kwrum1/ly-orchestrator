package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const duplicateTapInventory = `Interface: tap4096 (ifindex 3)
  name "lylan-ens192"
Interface: lyroute-ens161 (ifindex 5)
  name "lrh-5d41402a"
Interface: lyroute-ens161 (ifindex 6)
  name "lrh-5d41402b"
Interface: lyroute-ens192 (ifindex 7)
  name "lrh-5d41402c"
`

func dataplaneCleanupPlan(hook NativeHook, mode NativeMode) Plan {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	proof := CapabilityProof{
		Tier: DataplaneTierNative, Hook: hook, Mode: mode,
		Source: ProofSourceRuntimeProbe, RuntimeVerified: true, Native: true,
		ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute),
		KernelDriver: "vmxnet3", MACAddress: "00:0c:29:16:5b:1e", AcceptanceOnly: true,
	}
	return Plan{
		RequestID: "tap-cleanup",
		NativePath: NativePathRequest{
			ManagementInterface: "ens160", AcceptanceAFPacket: true, Now: now,
			Assignments: []NativeAssignment{
				{LinuxInterface: "ens161", Explicit: true, AFPacketConsent: true, Candidates: []CapabilityProof{proof}},
				{LinuxInterface: "ens192", Explicit: true, AFPacketConsent: true, Candidates: []CapabilityProof{proof}},
			},
		},
		AddressAssignments: []AddressAssignment{
			{ID: "wan", LinuxInterface: "ens161", VPPInterface: "lyroute-ens161", CIDR: "10.99.0.1/24", Role: "wan"},
			{ID: "lan", LinuxInterface: "ens192", VPPInterface: "lyroute-ens192", CIDR: "192.168.10.1/24", Role: "lan"},
		},
	}
}

// A leftover installer TAP with the product name must be released before the
// AF_PACKET attachment renames host-<port> onto it, otherwise VPP holds two
// interfaces named lyroute-<port> and every readback is ambiguous.
func TestCleanupDuplicateDataplaneTAPsRemovesTapsForAFPacket(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show tap":                 {stdout: duplicateTapInventory},
		"delete tap sw_if_index 5": {stdout: "deleted\n"},
		"delete tap sw_if_index 6": {stdout: "deleted\n"},
		"delete tap sw_if_index 7": {stdout: "deleted\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	adapter := Adapter{Client: NewProductionVPPCTLClient(binary)}
	if err := adapter.CleanupDuplicateDataplaneTAPs(context.Background(), dataplaneCleanupPlan(NativeHookAFPacket, NativeModeAFPacket)); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	// Both ports are attached with AF_PACKET, so every TAP carrying either
	// product name has to be released.
	for _, want := range []string{"delete tap sw_if_index 5", "delete tap sw_if_index 6", "delete tap sw_if_index 7"} {
		if !strings.Contains(string(logged), want) {
			t.Fatalf("%q missing from vppctl log:\n%s", want, logged)
		}
	}
	if strings.Contains(string(logged), "sw_if_index 3") {
		t.Fatalf("cleanup deleted an unrelated tap:\n%s", logged)
	}
}

// A live TAP bridge attachment must not be churned on replay: keep exactly one
// TAP per product name and delete only the duplicates.
func TestCleanupDuplicateDataplaneTAPsKeepsOneTapBridgeTap(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show tap":                 {stdout: duplicateTapInventory},
		"delete tap sw_if_index 6": {stdout: "deleted\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	adapter := Adapter{Client: NewProductionVPPCTLClient(binary)}
	if err := adapter.CleanupDuplicateDataplaneTAPs(context.Background(), dataplaneCleanupPlan(NativeHookTapBridge, NativeModeTapBridge)); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "delete tap sw_if_index 6") {
		t.Fatalf("duplicate TAP not removed:\n%s", logged)
	}
	if strings.Contains(string(logged), "delete tap sw_if_index 5") {
		t.Fatalf("live TAP bridge attachment was deleted:\n%s", logged)
	}
}

// Cleanup must never touch VPP when the apply is going to be rejected.
func TestCleanupDuplicateDataplaneTAPsSkipsUnselectablePlan(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{"show tap": {stdout: duplicateTapInventory}})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	plan := Plan{RequestID: "locked", NativePath: NativePathRequest{
		ManagementInterface: "ens160",
		Assignments:         []NativeAssignment{{LinuxInterface: "ens161", Explicit: true}},
	}}
	adapter := Adapter{Client: NewProductionVPPCTLClient(binary)}
	if err := adapter.CleanupDuplicateDataplaneTAPs(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if logged, _ := os.ReadFile(logPath); len(logged) != 0 {
		t.Fatalf("cleanup ran for an unselectable plan:\n%s", logged)
	}
}
