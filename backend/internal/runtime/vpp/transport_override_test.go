package vpp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCandidateRejectionRejectsVFIOUnderUIOConsent(t *testing.T) {
	t.Setenv("LY_ROUTE_DPDK_TRANSPORT", "uio")
	proof := CapabilityProof{
		Tier:                DataplaneTierDPDK,
		Hook:                NativeHookDPDK,
		Mode:                NativeModeDPDKVFIO,
		Transport:           "vfio",
		PCIAddress:          "0000:03:00.0",
		KernelDriver:        "igc",
		IOMMUGroup:          "12",
		IOMMUProtected:      true,
		VFIOAvailable:       true,
		HugepagesAvailable:  true,
		DPDKPluginAvailable: true,
	}
	reasons := candidateRejectionReasons(proof, time.Now().UTC(), DataplaneTierDPDK, false, false)
	for _, reason := range reasons {
		if strings.Contains(reason, "VFIO is disabled by LY_ROUTE_DPDK_TRANSPORT=uio") {
			return
		}
	}
	t.Fatalf("VFIO candidate was not rejected under UIO consent: %v", reasons)
}

func TestCommittedReportNormalisesVFIOToUIOUnderConsent(t *testing.T) {
	t.Setenv("LY_ROUTE_DPDK_TRANSPORT", "uio")
	path := filepath.Join(t.TempDir(), "active.json")
	state := `{"path":{"tier":"vpp_dpdk","attachments":[{"linux_interface":"enp3s0","vpp_interface":"lyroute-enp3s0","tier":"vpp_dpdk","hook":"dpdk","mode":"vfio_pci","pci_address":"0000:03:00.0","kernel_driver":"igc","iommu_group":"12","verification":"verified"}]}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	report, ok := committedCapabilityReport(path)
	if !ok {
		t.Fatal("committed state was not reconstructed")
	}
	proof := report.Proofs[0].Proof
	if proof.Mode != NativeModeDPDKUIO {
		t.Fatalf("mode = %q, want uio_pci_generic", proof.Mode)
	}
	if proof.Transport != "uio" {
		t.Fatalf("transport = %q, want uio", proof.Transport)
	}
	if proof.IOMMUGroup != "none" || proof.IOMMUProtected {
		t.Fatalf("UIO normalisation kept IOMMU identity: %#v", proof)
	}
}
