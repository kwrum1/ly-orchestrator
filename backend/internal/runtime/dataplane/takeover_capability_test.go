package dataplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTakeoverCapabilitiesPersistCrashGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "takeover-capabilities.json")
	state, err := LoadTakeoverCapabilities(path)
	if err != nil {
		t.Fatal(err)
	}
	state.MarkUnsupported("vmxnet3", "0000:13:00.0", "set interface state up failed: SIGSEGV in clib_sysfs_read_bitmap")
	if err := SaveTakeoverCapabilities(path, state); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadTakeoverCapabilities(path)
	if err != nil {
		t.Fatal(err)
	}
	capability, unsupported := reloaded.NativeUnsupported("vmxnet3", "0000:13:00.0")
	if !unsupported {
		t.Fatal("crash guard was not persisted")
	}
	if !strings.Contains(capability.Reason, "SIGSEGV") {
		t.Fatalf("reason = %q", capability.Reason)
	}
	// A different device on the same driver is not tainted by PCI-specific
	// evidence.
	if _, other := reloaded.NativeUnsupported("vmxnet3", "0000:1b:00.0"); other {
		t.Fatal("crash guard leaked to an unrelated PCI device")
	}
	// Idempotent: re-marking updates in place instead of duplicating.
	reloaded.MarkUnsupported("vmxnet3", "0000:13:00.0", "second observation")
	if len(reloaded.Devices) != 1 {
		t.Fatalf("devices = %#v", reloaded.Devices)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestActiveSupportedRequiresTheSameEnvironmentAndTTL(t *testing.T) {
	now := time.Now().UTC()
	environment := CapabilityEnvironment{KernelDriver: "igc", BoundDriver: "uio_pci_generic", VPPVersion: "v26", VPPRuntimeID: "123", Hugepages: 512, PCIAddress: "0000:03:00.0"}
	state := TakeoverCapabilityState{}
	state.RecordSupported("igc", "0000:03:00.0", "probe verified", environment, time.Minute)
	proof, ok := state.ActiveSupported("igc", "0000:03:00.0", environment, now)
	if !ok || proof.NativeUnsupported {
		t.Fatalf("proof = %#v, %t, want a current positive proof", proof, ok)
	}
	other := environment
	other.VPPRuntimeID = "456"
	if _, ok := state.ActiveSupported("igc", "0000:03:00.0", other, now); ok {
		t.Fatal("a proof from another VPP runtime was accepted")
	}
	if _, ok := state.ActiveSupported("igc", "0000:03:00.0", environment, now.Add(2*time.Minute)); ok {
		t.Fatal("an expired proof was accepted")
	}
	state.MarkUnsupportedWithEnvironment("igc", "0000:03:00.0", "boom", environment, time.Minute)
	if _, ok := state.ActiveSupported("igc", "0000:03:00.0", environment, now); ok {
		t.Fatal("an unsupported verdict was reported as a positive proof")
	}
}
