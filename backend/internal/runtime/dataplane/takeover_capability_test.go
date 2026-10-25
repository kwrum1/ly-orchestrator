package dataplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
