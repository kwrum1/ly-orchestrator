package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ly-route/backend/internal/runtime/dataplane"
)

func TestPruneDataplaneFactsDropsRemovedHardware(t *testing.T) {
	root := t.TempDir()
	present := "0000:04:00.0"
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "devices", present), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)

	ownershipPath := filepath.Join(t.TempDir(), "ownership.json")
	ownership := dataplane.OwnershipState{
		SchemaVersion: 1,
		Devices: []dataplane.OwnershipDevice{
			{PCIAddress: present, LinuxInterface: "enp4s0", KernelDriver: "uio_pci_generic", Transport: "dpdk"},
			{PCIAddress: "0000:0c:00.0", LinuxInterface: "enp12s0", KernelDriver: "igb", Transport: "kernel"},
		},
	}
	raw, err := json.Marshal(ownership)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownershipPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	verificationPath := filepath.Join(t.TempDir(), "dpdk-verification.json")
	verification := dataplane.DPDKVerificationState{
		SchemaVersion: 1,
		Interfaces: []dataplane.DPDKVerification{
			{LinuxInterface: "enp4s0", PCIAddress: present, Mode: "dpdk_uio"},
			{LinuxInterface: "enp12s0", PCIAddress: "0000:0c:00.0", Mode: "dpdk_uio"},
		},
	}
	raw, err = json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verificationPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if removed := pruneOwnershipFacts(ownershipPath); removed != 1 {
		t.Fatalf("ownership pruned = %d, want 1", removed)
	}
	if removed := pruneVerificationFacts(verificationPath); removed != 1 {
		t.Fatalf("verification pruned = %d, want 1", removed)
	}

	prunedOwnership, err := dataplane.LoadOwnershipState(ownershipPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedOwnership.Devices) != 1 || prunedOwnership.Devices[0].PCIAddress != present {
		t.Fatalf("ownership after prune = %+v", prunedOwnership.Devices)
	}
	prunedVerification, err := dataplane.LoadDPDKVerification(verificationPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(prunedVerification.Interfaces) != 1 || prunedVerification.Interfaces[0].PCIAddress != present {
		t.Fatalf("verification after prune = %+v", prunedVerification.Interfaces)
	}

	// A second pass must be a no-op, not a rewrite loop.
	if removed := pruneOwnershipFacts(ownershipPath); removed != 0 {
		t.Fatalf("second ownership prune removed %d records", removed)
	}
	if removed := pruneVerificationFacts(verificationPath); removed != 0 {
		t.Fatalf("second verification prune removed %d records", removed)
	}
}

func TestDevicePresentKeepsRecordsWithoutPCI(t *testing.T) {
	t.Setenv("LY_ROUTE_SYSFS_ROOT", t.TempDir())
	if !devicePresent("") {
		t.Fatal("a record without a PCI address must be kept")
	}
	if devicePresent("0000:99:00.0") {
		t.Fatal("an absent PCI address must be pruned")
	}
}
