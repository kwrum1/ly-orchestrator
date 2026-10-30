package vpp

import (
	"errors"
	"testing"
	"time"
)

func TestCapabilityStatusHasExactlyFourStates(t *testing.T) {
	cases := map[string]CapabilityStatus{
		"supported":                  CapabilitySupported,
		"SUPPORTED":                  CapabilitySupported,
		"supported_with_constraints": CapabilitySupportedWithConstraints,
		"unsupported":                CapabilityUnsupported,
		"unknown":                    CapabilityUnknown,
		"":                           CapabilityUnknown,
		"maybe-it-works-i-think":     CapabilityUnknown,
	}
	for input, want := range cases {
		if got := NormalizeCapabilityStatus(input); got != want {
			t.Fatalf("NormalizeCapabilityStatus(%q) = %q, want %q", input, got, want)
		}
	}
	if !CapabilityUnknown.Conservative() {
		t.Fatal("unknown must be conservative")
	}
	if CapabilitySupported.Conservative() || CapabilitySupportedWithConstraints.Conservative() {
		t.Fatal("a supported state must not force the conservative path")
	}
	if CapabilityUnknown.DestructiveAllowed() || CapabilityUnsupported.DestructiveAllowed() {
		t.Fatal("unknown and unsupported must never authorise a destructive takeover")
	}
	if !CapabilitySupportedWithConstraints.DestructiveAllowed() {
		t.Fatal("supported_with_constraints is still an explicit approval")
	}
}

func TestAssessDPDKTakeoverSeparatesConstraintsFromUnknown(t *testing.T) {
	uio := AssessDPDKTakeover(NativeModeDPDKUIO, DPDKTakeoverEvidence{
		UIOPCIAvailable: KnownFact(true),
		DPDKPlugin:      KnownFact(true),
		Hugepages:       KnownFact(true),
		PCIAddressSafe:  KnownFact(true),
		Queues:          1,
	})
	if uio.Status != CapabilitySupportedWithConstraints {
		t.Fatalf("uio status = %q, want supported_with_constraints", uio.Status)
	}
	if !containsString(uio.Constraints, "no IOMMU protection (uio_pci_generic)") || !containsString(uio.Constraints, "single RX queue") {
		t.Fatalf("constraints = %#v", uio.Constraints)
	}

	unknown := AssessDPDKTakeover(NativeModeDPDKVFIO, DPDKTakeoverEvidence{
		VFIOAvailable:  KnownFact(true),
		DPDKPlugin:     KnownFact(true),
		Hugepages:      UnknownFact("sysfs hugepage counter is unreadable"),
		PCIAddressSafe: KnownFact(true),
		IOMMUProtected: KnownFact(true),
		IOMMUGroup:     "17",
	})
	if unknown.Status != CapabilityUnknown || !unknown.Conservative() || unknown.AllowsDestructiveTakeover() {
		t.Fatalf("unknown assessment = %#v", unknown)
	}

	unsupported := AssessDPDKTakeover(NativeModeDPDKVFIO, DPDKTakeoverEvidence{
		VFIOAvailable:  KnownFact(false),
		DPDKPlugin:     KnownFact(true),
		Hugepages:      KnownFact(true),
		PCIAddressSafe: KnownFact(true),
		IOMMUProtected: KnownFact(true),
		IOMMUGroup:     "17",
	})
	if unsupported.Status != CapabilityUnsupported {
		t.Fatalf("status = %q, want unsupported", unsupported.Status)
	}
}

func TestAssessDPDKTakeoverRejectsTransportMismatch(t *testing.T) {
	assessment := AssessDPDKTakeover(NativeModeDPDKVFIO, DPDKTakeoverEvidence{
		Transport:      "uio",
		VFIOAvailable:  KnownFact(true),
		DPDKPlugin:     KnownFact(true),
		Hugepages:      KnownFact(true),
		PCIAddressSafe: KnownFact(true),
		IOMMUProtected: KnownFact(true),
		IOMMUGroup:     "17",
	})
	if assessment.Status != CapabilityUnsupported {
		t.Fatalf("status = %q, want unsupported for a uio transport on vfio mode", assessment.Status)
	}
}

func TestAssessCapabilityProofUnknownNeverPromotesToSupported(t *testing.T) {
	proof := CapabilityProof{Hook: NativeHookDPDK, Mode: NativeModeDPDKVFIO, Tier: DataplaneTierDPDK}
	if status := AssessCapabilityProof(proof).Status; status != CapabilityUnknown {
		t.Fatalf("status = %q, want unknown without a runtime proof", status)
	}
	proof.RuntimeVerified = true
	proof.ObservedAt = time.Now().UTC()
	proof.VFIOAvailable = true
	proof.DPDKPluginAvailable = true
	proof.HugepagesAvailable = true
	proof.IOMMUProtected = true
	proof.IOMMUGroup = "17"
	proof.PCIAddress = "0000:03:00.0"
	if status := AssessCapabilityProof(proof).Status; status != CapabilitySupported {
		t.Fatalf("status = %q, want supported", status)
	}
}

func TestCommonTierUnknownCapabilityFallsBackToConsentedCompatibilityPath(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	unknownDPDK := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKVFIO, 90)
	unknownDPDK.CapabilityStatus = CapabilityUnknown
	unknownDPDK.CapabilityReason = "virtualization probe did not answer"
	afPacket := CapabilityProof{
		Hook: NativeHookAFPacket, Mode: NativeModeAFPacket, Source: ProofSourceRuntimeProbe,
		RuntimeVerified: true, ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute),
		AcceptanceOnly: true, MACAddress: "00:50:56:aa:bb:cc",
	}
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		AFPacketConsent:     true,
		Assignments:         []NativeAssignment{{LinuxInterface: "eth1", Explicit: true, Candidates: []CapabilityProof{unknownDPDK, afPacket}}},
	})
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if len(path.Attachments) != 1 || path.Attachments[0].Hook != NativeHookAFPacket || !path.Degraded {
		t.Fatalf("path = %#v, want the consented compatibility path", path)
	}
	sawUnknown := false
	for _, evaluation := range path.Candidates {
		if evaluation.Hook == NativeHookDPDK {
			if evaluation.Capability != CapabilityUnknown || evaluation.Eligible {
				t.Fatalf("unknown DPDK evaluation = %#v", evaluation)
			}
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatal("unknown DPDK candidate is missing from the evaluation")
	}
}

func TestCommonTierUnknownCapabilityLocksInsteadOfGuessing(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKVFIO, 90)
	proof.CapabilityStatus = CapabilityUnknown
	_, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments:         []NativeAssignment{{LinuxInterface: "eth1", Explicit: true, Candidates: []CapabilityProof{proof}}},
	})
	var locked *DataplaneLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("error = %v, want DataplaneLockedError", err)
	}
}

func TestAssessCapabilityProofWithoutDriverVerdictIsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	proof := CapabilityProof{
		Tier: DataplaneTierNative, Hook: NativeHookAFXDP, Mode: NativeModeZeroCopy,
		Source: ProofSourceRuntimeProbe, RuntimeVerified: true,
		ObservedAt: now, ValidUntil: now.Add(time.Hour),
	}
	if got := AssessCapabilityProof(proof).Status; got != CapabilityUnknown {
		t.Fatalf("missing driver verdict = %q, want unknown", got)
	}
	proof.KernelDriver = "vmxnet3"
	if got := AssessCapabilityProof(proof).Status; got != CapabilityUnsupported {
		t.Fatalf("blocked driver = %q, want unsupported", got)
	}
	proof.KernelDriver = "igb"
	if got := AssessCapabilityProof(proof).Status; got != CapabilitySupported {
		t.Fatalf("known driver = %q, want supported", got)
	}
}

func TestNativeProbeDriverVerdictIsTriState(t *testing.T) {
	if _, known := NativeProbeDriverVerdict(NativeHookAFXDP, ""); known {
		t.Fatal("an empty driver must be unknown")
	}
	if _, known := NativeProbeDriverVerdict(NativeHookDPDK, "igb"); known {
		t.Fatal("DPDK has no AF_XDP driver verdict")
	}
	if supported, known := NativeProbeDriverVerdict(NativeHookAFXDP, "vmxnet3"); !known || supported {
		t.Fatal("vmxnet3 must be a known unsupported verdict")
	}
	if supported, known := NativeProbeDriverVerdict(NativeHookAFXDP, "igb"); !known || !supported {
		t.Fatal("igb must be a known supported verdict")
	}
}
