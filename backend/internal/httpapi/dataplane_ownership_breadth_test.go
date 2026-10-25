package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestAFXDPZeroCopyProvenReadsCachedProbe(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "capabilities.json")
	t.Setenv("LY_ROUTE_VPP_CAPABILITY_PROOF", report)
	if afXDPZeroCopyProven("eth1") {
		t.Fatal("empty/missing report must not claim zero-copy")
	}
	body := `{"proofs":[{"linux_interface":"eth1","candidates":[{"hook":"af_xdp","mode":"copy","runtime_verified":true}]}]}`
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if afXDPZeroCopyProven("eth1") {
		t.Fatal("copy mode must not be treated as zero-copy")
	}
	body = `{"proofs":[{"linux_interface":"eth1","candidates":[{"hook":"af_xdp","mode":"zero_copy","runtime_verified":true}]}]}`
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !afXDPZeroCopyProven("eth1") {
		t.Fatal("verified zero-copy candidate was not recognized")
	}
	if !afXDPOutranksDestructive("eth1") {
		t.Fatal("zero-copy should outrank the destructive UIO path")
	}
	if afXDPZeroCopyProven("eth2") {
		t.Fatal("proof leaked to another interface")
	}
	// IOMMU-protected VFIO is faster and must win over AF_XDP zero-copy.
	body = `{"proofs":[{"linux_interface":"eth1","candidates":[{"tier":"vpp_dpdk","hook":"dpdk","mode":"vfio_pci","runtime_verified":true},{"hook":"af_xdp","mode":"zero_copy","runtime_verified":true}]}]}`
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if afXDPOutranksDestructive("eth1") {
		t.Fatal("AF_XDP must not block an available VFIO bind")
	}
}

func TestNonDestructiveTakeoverActiveReadsActiveState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	body := `{"path":{"tier":"vpp_native","attachments":[{"linux_interface":"eth1","tier":"vpp_native","hook":"af_xdp","mode":"zero_copy"}]}}`
	if err := os.WriteFile(filepath.Join(root, "active.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !nonDestructiveTakeoverActive("eth1") {
		t.Fatal("AF_XDP active attachment must block a destructive re-bind")
	}
	if nonDestructiveTakeoverActive("eth2") {
		t.Fatal("unrelated port reported as owned")
	}
}

func TestPersistNativeTakeoverProofsRecordsModeAndLink(t *testing.T) {
	fixture := newTakeoverFixture(t, "eth1", "0000:13:00.0", "vmxnet3", "0", 0, true)
	fixture.writeVerification(t, `{"linux_interface":"eth2","pci_address":"0000:1b:00.0","mode":"dpdk_uio","verification":"verified"}`)
	server := &Server{}
	path := vpp.NativePath{Attachments: []vpp.NativeAttachment{{
		LinuxInterface: "eth1",
		VPPInterface:   "lyroute-eth1",
		Tier:           vpp.DataplaneTierNative,
		Hook:           vpp.NativeHookAFXDP,
		Mode:           vpp.NativeModeZeroCopy,
	}}}
	server.persistNativeTakeoverProofs(context.Background(), path)

	data, err := os.ReadFile(filepath.Join(fixture.root, "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Interfaces []struct {
			LinuxInterface string `json:"linux_interface"`
			Mode           string `json:"mode"`
			LinkState      string `json:"link_state"`
			Verification   string `json:"verification"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Interfaces) != 2 {
		t.Fatalf("interfaces = %#v, want the new proof and the preserved one", state.Interfaces)
	}
	found := false
	for _, entry := range state.Interfaces {
		if entry.LinuxInterface != "eth1" {
			continue
		}
		found = true
		if entry.Mode != "af_xdp_zero_copy" || entry.LinkState != "down" || entry.Verification != "pending_link" {
			t.Fatalf("entry = %#v", entry)
		}
	}
	if !found {
		t.Fatal("native proof not persisted")
	}
}

func TestNativeTakeoverModesAreRecognized(t *testing.T) {
	for _, mode := range []string{"af_packet", "af_xdp_zero_copy", "af_xdp_copy", "rdma"} {
		if !isNativeTakeoverMode(mode) {
			t.Fatalf("%s must be a native takeover mode", mode)
		}
	}
	for _, mode := range []string{"", "vfio", "uio", "dpdk_uio"} {
		if isNativeTakeoverMode(mode) {
			t.Fatalf("%s must not be treated as a native takeover mode", mode)
		}
	}
}
