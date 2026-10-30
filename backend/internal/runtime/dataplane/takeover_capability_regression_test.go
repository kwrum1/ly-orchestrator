package dataplane

import (
	"testing"
	"time"
)

func TestTakeoverCapabilityGuardIsScopedToEnvironmentAndTTL(t *testing.T) {
	pci := "0000:04:00.0"
	environment := CapabilityEnvironment{
		KernelDriver: "igb",
		BoundDriver:  "uio_pci_generic",
		VPPVersion:   "vpp v26.06-release",
		VPPRuntimeID: "4242",
		Hugepages:    512,
		PCIAddress:   pci,
	}
	state := TakeoverCapabilityState{}
	state.MarkUnsupportedWithEnvironment("igb", pci, "set interface state up failed: SIGSEGV", environment, time.Minute)

	now := time.Now().UTC()
	capability, active, why := state.ActiveUnsupported("igb", pci, environment, now)
	if !active {
		t.Fatalf("guard is not active in the same environment: %s", why)
	}
	if capability.EnvironmentFingerprint != environment.Fingerprint() {
		t.Fatalf("fingerprint = %q, want %q", capability.EnvironmentFingerprint, environment.Fingerprint())
	}
	if !capability.ValidUntil.After(now) {
		t.Fatalf("valid_until = %v, want a future TTL", capability.ValidUntil)
	}

	upgraded := environment
	upgraded.VPPVersion = "vpp v26.10-release"
	if _, active, why = state.ActiveUnsupported("igb", pci, upgraded, now); active {
		t.Fatalf("guard survived a VPP upgrade: %s", why)
	}

	rebound := environment
	rebound.BoundDriver = "vfio-pci"
	if _, active, why = state.ActiveUnsupported("igb", pci, rebound, now); active {
		t.Fatalf("guard survived a transport re-bind: %s", why)
	}

	restarted := environment
	restarted.VPPRuntimeID = "9999"
	if _, active, why = state.ActiveUnsupported("igb", pci, restarted, now); active {
		t.Fatalf("guard survived a VPP restart: %s", why)
	}

	if _, active, why = state.ActiveUnsupported("igb", pci, environment, now.Add(2*time.Minute)); active {
		t.Fatalf("guard survived its TTL: %s", why)
	}

	legacy := TakeoverCapabilityState{Devices: []TakeoverCapability{{
		Driver:            "igb",
		PCIAddress:        pci,
		NativeUnsupported: true,
		Reason:            "legacy",
	}}}
	if _, active, why = legacy.ActiveUnsupported("igb", pci, environment, now); active {
		t.Fatalf("legacy permanent guard was reused: %s", why)
	}
}

func TestTakeoverCapabilitySuccessClearsGuardAndStampsFingerprint(t *testing.T) {
	pci := "0000:04:00.0"
	environment := CapabilityEnvironment{
		KernelDriver: "igb",
		BoundDriver:  "uio_pci_generic",
		VPPVersion:   "vpp v26.06-release",
		VPPRuntimeID: "4242",
		Hugepages:    512,
		PCIAddress:   pci,
	}
	state := TakeoverCapabilityState{}
	state.MarkUnsupportedWithEnvironment("igb", pci, "transient probe contention", environment, time.Minute)

	if !state.MarkSupported("igb", pci, "trusted ownership proof", environment, time.Hour) {
		t.Fatal("MarkSupported did not update the existing guard")
	}
	var capability TakeoverCapability
	for _, device := range state.Devices {
		if device.PCIAddress == pci {
			capability = device
		}
	}
	if capability.NativeUnsupported {
		t.Fatalf("guard still negative: %#v", capability)
	}
	if capability.EnvironmentFingerprint != environment.Fingerprint() {
		t.Fatalf("fingerprint = %q, want %q", capability.EnvironmentFingerprint, environment.Fingerprint())
	}
	if capability.Reason != "trusted ownership proof" {
		t.Fatalf("reason = %q", capability.Reason)
	}
	if !capability.ValidUntil.After(time.Now().UTC()) {
		t.Fatalf("valid_until = %v, want a future lease", capability.ValidUntil)
	}
	if _, active, _ := state.ActiveUnsupported("igb", pci, environment, time.Now().UTC()); active {
		t.Fatal("a positive cache entry must not suppress the next probe")
	}
}

func TestTakeoverCapabilityProbeSuccessWritesFingerprintedCache(t *testing.T) {
	pci := "0000:04:00.0"
	environment := CapabilityEnvironment{
		KernelDriver: "igb",
		BoundDriver:  "uio_pci_generic",
		VPPVersion:   "vpp v26.06-release",
		VPPRuntimeID: "4242",
		Hugepages:    512,
		PCIAddress:   pci,
	}
	state := TakeoverCapabilityState{}
	state.RecordSupported("igb", pci, "DPDK takeover verified", environment, time.Hour)
	if len(state.Devices) != 1 {
		t.Fatalf("devices = %#v, want the successful probe cached", state.Devices)
	}
	cached := state.Devices[0]
	if cached.NativeUnsupported {
		t.Fatalf("successful probe was cached as unsupported: %#v", cached)
	}
	if cached.EnvironmentFingerprint != environment.Fingerprint() {
		t.Fatalf("fingerprint = %q, want %q", cached.EnvironmentFingerprint, environment.Fingerprint())
	}
	if cached.ValidUntil.IsZero() || !cached.ValidUntil.After(time.Now().UTC()) {
		t.Fatalf("valid_until = %v, want a future lease", cached.ValidUntil)
	}
}
