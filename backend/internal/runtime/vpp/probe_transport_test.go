package vpp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelectNativePathApprovesDPDKUIOPath(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO, 75)
	proof.IOMMUGroup = "none"
	proof.IOMMUProtected = false
	proof.VFIOAvailable = false
	proof.UIOPCIAvailable = true
	proof.Transport = "uio"
	proof.VPPInterface = "GigabitEthernet4/0/0"
	proof.LinkState = "up"
	proof.KernelDriver = "igb"

	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	if err != nil {
		t.Fatalf("DPDK/UIO path was not approved: %v", err)
	}
	if path.Tier != DataplaneTierDPDK || len(path.Attachments) != 1 {
		t.Fatalf("path = %#v, want one DPDK attachment", path)
	}
	attachment := path.Attachments[0]
	if attachment.Hook != NativeHookDPDK || attachment.Mode != NativeModeDPDKUIO {
		t.Fatalf("attachment = %#v, want DPDK/UIO", attachment)
	}
	if attachment.VPPInterface != "GigabitEthernet4/0/0" || attachment.PCIAddress != "0000:03:00.0" {
		t.Fatalf("attachment identity = %#v", attachment)
	}
	if attachment.IOMMUGroup != "none" || attachment.KernelDriver != "igb" {
		t.Fatalf("transport identity = %#v, want kernel igb with no IOMMU group", attachment)
	}
}

func TestSelectNativePathRejectsDPDKUIOWithoutUIOOrWithWrongTransport(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		mutate   func(*CapabilityProof)
		expected string
	}{
		{
			name: "uio unavailable",
			mutate: func(proof *CapabilityProof) {
				proof.UIOPCIAvailable = false
			},
			expected: "uio_pci_generic is unavailable",
		},
		{
			name: "transport mismatch",
			mutate: func(proof *CapabilityProof) {
				proof.Transport = "vfio"
			},
			expected: "DPDK transport does not match the PCI binding mode",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO, 75)
			proof.IOMMUGroup = "none"
			proof.IOMMUProtected = false
			proof.VFIOAvailable = false
			proof.UIOPCIAvailable = true
			proof.Transport = "uio"
			proof.KernelDriver = "igb"
			test.mutate(&proof)

			_, err := SelectNativePath(NativePathRequest{
				ManagementInterface: "eth0",
				Now:                 now,
				Assignments: []NativeAssignment{{
					LinuxInterface: "eth1",
					Explicit:       true,
					Candidates:     []CapabilityProof{proof},
				}},
			})
			locked, ok := err.(*DataplaneLockedError)
			if !ok {
				t.Fatalf("error = %T %v, want dataplane_locked", err, err)
			}
			if !candidateReasonContains(locked.Candidates, test.expected) {
				t.Fatalf("candidate reasons = %#v, want %q", locked.Candidates, test.expected)
			}
		})
	}
}

func TestSelectNativePathNeverApprovesAFPacketAsHighPerformance(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	proof := CapabilityProof{
		Tier: DataplaneTierNative, Hook: NativeHookAFPacket, Mode: NativeModeAFPacket,
		Source: ProofSourceRuntimeProbe, RuntimeVerified: true, Native: true,
		HighPerformance: true, AcceptanceOnly: false,
		ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute),
		MACAddress: "00:11:22:33:44:55", KernelDriver: "e1000",
	}
	_, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	locked, ok := err.(*DataplaneLockedError)
	if !ok {
		t.Fatalf("error = %T %v, want dataplane_locked for AF_PACKET", err, err)
	}
	if !candidateReasonContains(locked.Candidates, "candidate is not an approved VPP-native high-performance path") {
		t.Fatalf("candidate reasons = %#v", locked.Candidates)
	}
}

func TestCommittedCapabilityReportRecordsTransport(t *testing.T) {
	for _, test := range []struct {
		mode      NativeMode
		transport string
	}{
		{mode: NativeModeDPDKUIO, transport: "uio"},
		{mode: NativeModeDPDKVFIO, transport: "vfio"},
	} {
		t.Run(test.transport, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "active.json")
			body := `{"path":{"tier":"vpp_dpdk","attachments":[{"linux_interface":"eth1","vpp_interface":"GigabitEthernet3/0/0","tier":"vpp_dpdk","hook":"dpdk","mode":"` + string(test.mode) + `","pci_address":"0000:03:00.0","kernel_driver":"igb","iommu_group":"` + map[NativeMode]string{NativeModeDPDKUIO: "none", NativeModeDPDKVFIO: "17"}[test.mode] + `"}],"prerequisites":[]},"applied_at":"2026-10-25T12:00:00Z"}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			report, ok := committedCapabilityReport(path)
			if !ok || len(report.Proofs) != 1 {
				t.Fatalf("committed report = %#v, ok=%t", report, ok)
			}
			proof := report.Proofs[0].Proof
			if proof.Transport != test.transport {
				t.Fatalf("transport = %q, want %q", proof.Transport, test.transport)
			}
			if !proof.HighPerformance || proof.Tier != DataplaneTierDPDK {
				t.Fatalf("proof = %#v, want high-performance DPDK", proof)
			}
		})
	}
}

func TestSelectNativePathApprovesNonIntelDPDKAttach(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO, 75)
	proof.IOMMUGroup = "none"
	proof.IOMMUProtected = false
	proof.VFIOAvailable = false
	proof.UIOPCIAvailable = true
	proof.Transport = "uio"
	proof.KernelDriver = "vmxnet3"
	proof.LinkState = "up"
	proof.HardwareLink = "up"
	proof.DriverModel = "VMware vmxnet3"
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	if err != nil {
		t.Fatalf("non-Intel VPP DPDK attach was rejected: %v", err)
	}
	if path.Tier != DataplaneTierDPDK || len(path.Attachments) != 1 {
		t.Fatalf("path = %#v", path)
	}
}

func TestSelectNativePathApprovesDPDKAttachWithDownLink(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO, 75)
	proof.IOMMUGroup = "none"
	proof.IOMMUProtected = false
	proof.VFIOAvailable = false
	proof.UIOPCIAvailable = true
	proof.Transport = "uio"
	proof.KernelDriver = "vmxnet3"
	proof.LinkState = "down"
	proof.HardwareLink = "down"
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	if err != nil {
		t.Fatalf("down-link DPDK candidate must still be approved: %v", err)
	}
	if len(path.Attachments) != 1 {
		t.Fatalf("attachments = %#v", path.Attachments)
	}
	if path.Attachments[0].Verification != "pending_link" {
		t.Fatalf("verification = %q, want pending_link", path.Attachments[0].Verification)
	}
}

func TestSelectNativePathApprovesDownLinkAFXDPAsPendingLink(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeZeroCopy, 90)
	proof.LinkState = "down"
	proof.HardwareLink = "down"
	path, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	})
	if err != nil {
		t.Fatalf("down-link AF_XDP candidate must still be approved: %v", err)
	}
	if len(path.Attachments) != 1 || path.Attachments[0].Verification != "pending_link" {
		t.Fatalf("attachments = %#v, want one pending_link attachment", path.Attachments)
	}
}

func TestSelectNativePathApprovesFunctionallyVerifiedDownLink(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierDPDK, NativeHookDPDK, NativeModeDPDKUIO, 75)
	proof.IOMMUGroup = "none"
	proof.IOMMUProtected = false
	proof.VFIOAvailable = false
	proof.UIOPCIAvailable = true
	proof.Transport = "uio"
	proof.KernelDriver = "vmxnet3"
	proof.LinkState = "down"
	proof.HardwareLink = "down"
	proof.LinkSpeedMbps = 10000
	proof.FunctionalVerified = true
	proof.FunctionalMethod = "tx_counter_and_link_speed"
	if _, err := SelectNativePath(NativePathRequest{
		ManagementInterface: "eth0",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "eth1",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	}); err != nil {
		t.Fatalf("functionally verified down-link candidate was rejected: %v", err)
	}
}

func candidateReasonContains(evaluations []CandidateEvaluation, expected string) bool {
	for _, evaluation := range evaluations {
		for _, reason := range evaluation.Reasons {
			if strings.Contains(reason, expected) {
				return true
			}
		}
	}
	return false
}
