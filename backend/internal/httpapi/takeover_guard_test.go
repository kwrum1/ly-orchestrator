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

func probeContentionFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "capabilities.json"))
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", filepath.Join(root, "proof.json"))
	t.Setenv("LY_ROUTE_VPP_VERSION", "vpp v26.06-release")
	t.Setenv("LY_ROUTE_VPP_RUNTIME_ID", "1000")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")
	return root
}

func bindTestPCIDevice(t *testing.T, root, pci, driver string) {
	t.Helper()
	device := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers", driver)
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(driverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(device, "driver")
	_ = os.Remove(link)
	if err := os.Symlink(driverDir, link); err != nil {
		t.Fatal(err)
	}
}

func writeOwnershipProof(t *testing.T, path, iface, pci, kernelDriver, transport string) {
	t.Helper()
	body := `{"schema_version":1,"source":"prepare-vfio","state":"active","transport":"` + transport + `","devices":[{"pci_address":"` + pci + `","linux_interface":"` + iface + `","mac_address":"00:11:22:33:44:55","kernel_driver":"` + kernelDriver + `","transport":"` + transport + `","created_by_product":true}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidatedDPDKOwnershipProofTrustsActiveBinding(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "uio_pci_generic")
	writeOwnershipProof(t, filepath.Join(root, "ownership.json"), "enp4s0", pci, "igb", "uio")

	server := &Server{}
	proof, ok := server.validatedDPDKOwnershipProof("enp4s0", pci)
	if !ok {
		t.Fatal("active UIO ownership was not trusted")
	}
	if proof.Mode != "dpdk_uio" || proof.Source != "ownership" {
		t.Fatalf("proof = %#v", proof)
	}
	if _, ok := server.validatedDPDKOwnershipProof("enp5s0", pci); ok {
		t.Fatal("ownership for enp4s0 was trusted for a different interface")
	}

	pending := `{"schema_version":1,"state":"pending","transport":"uio","devices":[{"pci_address":"` + pci + `","linux_interface":"enp4s0","kernel_driver":"igb","transport":"uio"}]}`
	if err := os.WriteFile(filepath.Join(root, "ownership.json"), []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.validatedDPDKOwnershipProof("enp4s0", pci); ok {
		t.Fatal("a pending ownership write-ahead record was trusted")
	}
	writeOwnershipProof(t, filepath.Join(root, "ownership.json"), "enp4s0", pci, "igb", "uio")

	// A device that is back on its kernel driver is not owned any more.
	bindTestPCIDevice(t, root, pci, "igb")
	if _, ok := server.validatedDPDKOwnershipProof("enp4s0", pci); ok {
		t.Fatal("kernel-bound device was reported as DPDK-owned")
	}
}

func TestBestAvailableTakeoverModeTrustsOwnershipWithoutProbe(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "uio_pci_generic")
	writeOwnershipProof(t, filepath.Join(root, "ownership.json"), "enp4s0", pci, "igb", "uio")

	server := &Server{}
	capabilities := dataplane.TakeoverCapabilityState{}
	environment := server.capabilityEnvironmentFor(pci, "igb")
	capabilities.MarkUnsupportedWithEnvironment("igb", pci, "VPP failed to start with the candidate DPDK device: connect: Connection refused", environment, time.Minute)

	mode, assessment, reason := server.bestAvailableTakeoverMode(context.Background(), "enp4s0", pci, DataplanePolicy{}, capabilities)
	if mode != "dpdk_uio" || reason != "" {
		t.Fatalf("mode=%q reason=%q, want dpdk_uio with no degradation", mode, reason)
	}
	if !assessment.Status.DestructiveAllowed() {
		t.Fatalf("capability = %#v, want a support verdict for the committed binding", assessment)
	}
}

func TestSuppressCrashGuardedCandidatesTrustsBoundOwnershipOverGuard(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "uio_pci_generic")
	writeOwnershipProof(t, filepath.Join(root, "ownership.json"), "enp4s0", pci, "igb", "uio")
	capabilityPath := filepath.Join(root, "capabilities.json")
	server := &Server{}
	capabilities := dataplane.TakeoverCapabilityState{}
	environment := server.capabilityEnvironmentFor(pci, "igb")
	capabilities.MarkUnsupportedWithEnvironment("igb", pci, "VPP failed to start with the candidate DPDK device: connect: Connection refused", environment, time.Hour)
	if err := dataplane.SaveTakeoverCapabilities(capabilityPath, capabilities); err != nil {
		t.Fatal(err)
	}

	pathRequest := vpp.NativePathRequest{Assignments: []vpp.NativeAssignment{{
		LinuxInterface: "enp4s0",
		Proof:          vpp.CapabilityProof{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: pci, KernelDriver: "igb"},
		Candidates: []vpp.CapabilityProof{
			{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: pci, KernelDriver: "igb"},
			{Tier: vpp.DataplaneTierNative, Hook: vpp.NativeHookAFPacket, Mode: vpp.NativeModeAFPacket, AcceptanceOnly: true},
		},
	}}}
	server.suppressCrashGuardedCandidates(&pathRequest)
	if len(pathRequest.Assignments[0].Candidates) != 2 {
		t.Fatalf("candidates = %#v, want the DPDK candidate retained", pathRequest.Assignments[0].Candidates)
	}
	if pathRequest.Assignments[0].Candidates[0].Mode != vpp.NativeModeDPDKUIO {
		t.Fatalf("DPDK candidate was suppressed: %#v", pathRequest.Assignments[0].Candidates)
	}

	reloaded, err := dataplane.LoadTakeoverCapabilities(capabilityPath)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, device := range reloaded.Devices {
		if device.PCIAddress != pci {
			continue
		}
		found = true
		if device.NativeUnsupported {
			t.Fatalf("successful ownership proof did not retire the guard: %#v", device)
		}
		if device.EnvironmentFingerprint == "" || !device.ValidUntil.After(time.Now().UTC()) {
			t.Fatalf("positive cache entry lacks fingerprint/TTL: %#v", device)
		}
	}
	if !found {
		t.Fatal("positive cache entry was not persisted")
	}
}

func TestTakeoverGuardInvalidatesWhenEnvironmentChanges(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "uio_pci_generic")
	server := &Server{}
	environment := server.capabilityEnvironmentFor(pci, "igb")
	capabilities := dataplane.TakeoverCapabilityState{}
	capabilities.MarkUnsupportedWithEnvironment("igb", pci, "transient connection refused", environment, time.Hour)

	if !server.takeoverGuardActive(capabilities, "igb", pci, "enp4s0") {
		t.Fatal("guard in the same environment must still suppress the retry")
	}

	t.Setenv("LY_ROUTE_VPP_VERSION", "vpp v26.10-release")
	if server.takeoverGuardActive(capabilities, "igb", pci, "enp4s0") {
		t.Fatal("guard survived a VPP upgrade")
	}

	t.Setenv("LY_ROUTE_VPP_VERSION", "vpp v26.06-release")
	bindTestPCIDevice(t, root, pci, "vfio-pci")
	if server.takeoverGuardActive(capabilities, "igb", pci, "enp4s0") {
		t.Fatal("guard survived a transport re-bind")
	}
}

func TestRecordTakeoverCrashGuardWritesEnvironmentFingerprint(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "uio_pci_generic")
	writeOwnershipProof(t, filepath.Join(root, "ownership.json"), "enp4s0", pci, "igb", "uio")
	server := &Server{}

	if !server.recordTakeoverCrashGuard([]dpdkTakeoverFailure{{
		LinuxInterface: "enp4s0",
		PCIAddress:     pci,
		Reason:         "VPP failed to start with the candidate DPDK device: connect: Connection refused",
		Crashed:        true,
	}}) {
		t.Fatal("crash guard was not recorded")
	}
	state, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entry dataplane.TakeoverCapability
	for _, device := range state.Devices {
		if device.PCIAddress == pci {
			entry = device
		}
	}
	if !entry.NativeUnsupported {
		t.Fatalf("guard = %#v, want native_unsupported", entry)
	}
	environment := server.capabilityEnvironmentFor(pci, "igb")
	if entry.EnvironmentFingerprint != environment.Fingerprint() {
		t.Fatalf("fingerprint = %q, want %q", entry.EnvironmentFingerprint, environment.Fingerprint())
	}
	if !entry.ValidUntil.After(time.Now().UTC()) {
		t.Fatalf("valid_until = %v, want a future TTL", entry.ValidUntil)
	}
	if _, active, why := state.ActiveUnsupported("igb", pci, environment, time.Now().UTC()); !active {
		t.Fatalf("fresh guard is not active: %s", why)
	}
}

func TestSuppressCrashGuardedCandidatesRetriesLegacyPermanentGuard(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	// A pre-fix cache has no environment fingerprint. It must not suppress the
	// next probe, even though the file still exists.
	capabilityPath := filepath.Join(root, "capabilities.json")
	body := `{"schema_version":1,"devices":[{"driver":"igb","pci_address":"` + pci + `","native_unsupported":true,"reason":"VPP failed to start with the candidate DPDK device: connect: Connection refused"}]}`
	if err := os.WriteFile(capabilityPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pathRequest := vpp.NativePathRequest{Assignments: []vpp.NativeAssignment{{
		LinuxInterface: "enp4s0",
		Candidates: []vpp.CapabilityProof{
			{Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: pci, KernelDriver: "igb"},
			{Tier: vpp.DataplaneTierNative, Hook: vpp.NativeHookAFPacket, Mode: vpp.NativeModeAFPacket, AcceptanceOnly: true},
		},
	}}}
	server := &Server{}
	server.suppressCrashGuardedCandidates(&pathRequest)
	if len(pathRequest.Assignments[0].Candidates) != 2 {
		t.Fatalf("legacy guard still suppressed the DPDK candidate: %#v", pathRequest.Assignments[0].Candidates)
	}
}

func TestHugepagesShortfallReportsRequiredAndCurrent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES_REQUIRED", "256")
	pageFile := filepath.Join(root, "kernel/mm/hugepages/hugepages-2048kB/nr_hugepages")
	if err := os.MkdirAll(filepath.Dir(pageFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageFile, []byte("128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	needed, current, enough := hugepagesShortfall()
	if enough || needed != 256 || current != 128 {
		t.Fatalf("shortfall = needed %d current %d enough %v, want 256/128/false", needed, current, enough)
	}
	if err := os.WriteFile(pageFile, []byte("512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if needed, current, enough = hugepagesShortfall(); !enough || needed != 256 || current != 512 {
		t.Fatalf("satisfied = needed %d current %d enough %v, want 256/512/true", needed, current, enough)
	}
}

func TestHugepagesShortfallDoesNotWriteUnsupportedCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES_REQUIRED", "256")
	capabilityPath := filepath.Join(root, "capabilities.json")
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", capabilityPath)
	pageFile := filepath.Join(root, "kernel/mm/hugepages/hugepages-2048kB/nr_hugepages")
	if err := os.MkdirAll(filepath.Dir(pageFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageFile, []byte("128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	if server.dpdkProbeAllowedByHugepages([]string{"enp5s0"}) {
		t.Fatal("candidate probe was allowed with insufficient hugepages")
	}
	if _, err := os.Stat(capabilityPath); !os.IsNotExist(err) {
		t.Fatalf("a hugepage shortfall wrote a native_unsupported cache: %v", err)
	}
}

func TestValidatedOwnershipProofUsesCommittedStateWithoutNetdev(t *testing.T) {
	root := probeContentionFixture(t)
	pci := "0000:04:00.0"
	bindTestPCIDevice(t, root, pci, "vfio-pci")
	body := `{"path":{"tier":"vpp_dpdk","attachments":[{"linux_interface":"enp4s0","pci_address":"` + pci + `","tier":"vpp_dpdk","hook":"dpdk","mode":"vfio_pci","verification":"verified"}]}}`
	if err := os.WriteFile(filepath.Join(root, "active.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	proof, ok := server.validatedDPDKOwnershipProof("enp4s0", pci)
	if !ok || proof.Mode != "dpdk_vfio" || proof.Source != "committed_active_state" {
		t.Fatalf("proof = %#v ok=%v", proof, ok)
	}
	if !strings.Contains(proof.Source, "active") {
		t.Fatalf("source = %q", proof.Source)
	}
}
