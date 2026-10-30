package vpp

import (
	"errors"
	"testing"
	"time"
)

func fallbackProof(now time.Time, tier DataplaneTier, hook NativeHook, mode NativeMode) CapabilityProof {
	proof := candidateProof(now, tier, hook, mode, 50)
	if tier == DataplaneTierDPDK && mode == NativeModeDPDKUIO {
		proof.IOMMUGroup = "none"
		proof.IOMMUProtected = false
		proof.VFIOAvailable = false
		proof.UIOPCIAvailable = true
		proof.Transport = "uio"
		proof.KernelDriver = "igb"
	}
	if tier == DataplaneTierDPDK && mode == NativeModeDPDKVFIO {
		proof.Transport = "vfio"
	}
	if hook == NativeHookAFPacket {
		proof.AcceptanceOnly = true
		proof.HighPerformance = false
		proof.KernelDriver = "igb"
		proof.MACAddress = "00:11:22:33:44:55"
	}
	return proof
}

func TestOrderedFallbackSelectsFirstUsableTransport(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	// The product's ordered fallback: lowest operational overhead at the same
	// performance class first, universal compatibility last.
	order := []CapabilityProof{
		fallbackProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeZeroCopy),
		fallbackProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKVFIO),
		fallbackProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO),
		fallbackProof(now, DataplaneTierNative, NativeHookRDMA, NativeModeRDMADV),
		fallbackProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeCopy),
		fallbackProof(now, DataplaneTierNative, NativeHookAFPacket, NativeModeAFPacket),
	}
	for index, expected := range order {
		t.Run(string(expected.Hook)+"/"+string(expected.Mode), func(t *testing.T) {
			// Only candidates at or below the expected rank are offered, in
			// reverse order, to prove preference (not input order) decides.
			candidates := make([]CapabilityProof, 0, len(order)-index)
			for cursor := len(order) - 1; cursor >= index; cursor-- {
				candidates = append(candidates, order[cursor])
			}
			path, err := SelectNativePath(NativePathRequest{
				ManagementInterface: "eth0",
				Now:                 now,
				AFPacketConsent:     true,
				Assignments: []NativeAssignment{{
					LinuxInterface: "eth1",
					Explicit:       true,
					Candidates:     candidates,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(path.Attachments) != 1 || path.Attachments[0].Hook != expected.Hook || path.Attachments[0].Mode != expected.Mode {
				t.Fatalf("attachment = %#v, want %s/%s", path.Attachments, expected.Hook, expected.Mode)
			}
		})
	}
}

// TestZeroCopyOutranksDPDKRegardlessOfScore pins the decision that AF_XDP
// zero-copy comes first: it keeps the kernel netdev (no PCI re-bind, survives a
// power loss) and reaches near-DPDK throughput. A lower static score must not
// demote it, because score only breaks ties inside one rank.
func TestZeroCopyOutranksDPDKRegardlessOfScore(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	zeroCopy := fallbackProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeZeroCopy)
	zeroCopy.PerformanceScore = 1
	dpdk := fallbackProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKVFIO)
	dpdk.PerformanceScore = 999

	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{dpdk, zeroCopy},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Attachments) != 1 || path.Attachments[0].Hook != NativeHookAFXDP || path.Attachments[0].Mode != NativeModeZeroCopy {
		t.Fatalf("attachment = %#v, want AF_XDP zero-copy", path.Attachments)
	}
}

func TestAFOnlyWithoutConsentRequiresExplicitFallback(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	proof := fallbackProof(now, DataplaneTierNative, NativeHookAFPacket, NativeModeAFPacket)
	_, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	var required *AFFallbackRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %T %v, want af_fallback_required", err, err)
	}
	if len(required.Interfaces) != 1 || required.Interfaces[0] != "eth1" {
		t.Fatalf("interfaces = %#v", required.Interfaces)
	}
}

func TestAFOnlyWithConsentDegradesInsteadOfLocking(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	proof := fallbackProof(now, DataplaneTierNative, NativeHookAFPacket, NativeModeAFPacket)
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		AFPacketConsent:     true,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	if err != nil {
		t.Fatalf("consented AF fallback must not lock: %v", err)
	}
	if !path.Degraded || len(path.Attachments) != 1 || path.Attachments[0].Hook != NativeHookAFPacket {
		t.Fatalf("path = %#v, want degraded AF attachment", path)
	}
	found := false
	for _, degradation := range path.Degradations {
		if degradation.Name == "af_packet_fallback" && degradation.Reason == AFPacketDegradedReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("degradations = %#v, want af_packet_fallback", path.Degradations)
	}
}

func TestMixedNativeAndAFPortsCommitDegraded(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	native := fallbackProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeZeroCopy)
	af := fallbackProof(now, DataplaneTierNative, NativeHookAFPacket, NativeModeAFPacket)
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		AFPacketConsent:     true,
		Assignments: []NativeAssignment{
			{LinuxInterface: "eth1", Explicit: true, Candidates: []CapabilityProof{native}},
			{LinuxInterface: "eth2", Explicit: true, Candidates: []CapabilityProof{af}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !path.Degraded || len(path.Attachments) != 2 {
		t.Fatalf("path = %#v, want mixed native+AF degraded path", path)
	}
}
