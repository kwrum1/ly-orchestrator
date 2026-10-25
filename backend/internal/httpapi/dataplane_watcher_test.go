package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/dataplane"
)

func TestDemoteOnLinkDownKeepsOwnershipAndOnlyWeakensProof(t *testing.T) {
	entry := dataplane.DPDKVerification{
		LinuxInterface:     "eth1",
		Mode:               "dpdk_uio",
		PCIAddress:         "0000:13:00.0",
		LinkState:          "up",
		Verification:       "verified",
		FunctionalVerified: true,
	}
	updated, changed := demoteOnLinkDown(entry, "down")
	if !changed {
		t.Fatal("expected a pending_link demotion")
	}
	if updated.Verification != "pending_link" || updated.FunctionalVerified {
		t.Fatalf("updated = %#v", updated)
	}
	if updated.Mode != "dpdk_uio" || updated.PCIAddress != "0000:13:00.0" {
		t.Fatalf("ownership changed: %#v", updated)
	}
	if _, changed := demoteOnLinkDown(updated, "down"); changed {
		t.Fatal("repeated down events must be idempotent")
	}
	up, changed := demoteOnLinkDown(updated, "up")
	if changed || up.Verification != "pending_link" {
		t.Fatalf("link up must not demote: %#v changed=%v", up, changed)
	}
}

func TestFunctionalProbePromotesPendingLinkWithoutApply(t *testing.T) {
	root := t.TempDir()
	netdev := filepath.Join(root, "class/net/eth1")
	if err := os.MkdirAll(netdev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DPDK_FUNCTIONAL_PEER", "")
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show interface lyroute-eth1") printf '%s\n' 'lyroute-eth1 1 up 9000/0/0/0' ;;
  "set interface state lyroute-eth1 up") exit 0 ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	server := &Server{}
	entry := dataplane.DPDKVerification{LinuxInterface: "eth1", Mode: "af_packet", Verification: "pending_link", LinkState: "down"}
	probed := server.functionalProbe(context.Background(), "eth1", entry, map[string]bool{"lyroute-eth1": true})
	if probed.Verification != "verified" || !probed.FunctionalVerified {
		t.Fatalf("probed = %#v, want verified", probed)
	}
	if probed.Method != "watcher_link_state_up" {
		t.Fatalf("method = %q", probed.Method)
	}
}

func TestFunctionalProbeKeepsPendingWhenVPPInterfaceIsDown(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show interface lyroute-eth1") printf '%s\n' 'lyroute-eth1 1 down 9000/0/0/0' ;;
  "set interface state lyroute-eth1 up") exit 1 ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	server := &Server{}
	probed := server.functionalProbe(context.Background(), "eth1", dataplane.DPDKVerification{LinuxInterface: "eth1", Mode: "af_packet", Verification: "pending_link"}, map[string]bool{"lyroute-eth1": true})
	if probed.Verification != "pending_link" {
		t.Fatalf("probed = %#v, want pending_link", probed)
	}
}

func TestMergeDPDKVerificationPreservesUnrelatedProof(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "verification.json")
	existing := `{"schema_version":1,"interfaces":[{"linux_interface":"eth2","pci_address":"0000:1b:00.0","mode":"af_packet","verification":"verified"}]}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeDPDKVerification(path, nil); err != nil {
		t.Fatal(err)
	}
	state, err := dataplane.LoadDPDKVerification(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Interfaces) != 1 || state.Interfaces[0].LinuxInterface != "eth2" {
		t.Fatalf("empty probe erased the proof: %#v", state.Interfaces)
	}
	if err := mergeDPDKVerification(path, []dataplane.DPDKVerification{{LinuxInterface: "eth1", Mode: "dpdk_uio", Verification: "pending_link"}}); err != nil {
		t.Fatal(err)
	}
	state, _ = dataplane.LoadDPDKVerification(path)
	if len(state.Interfaces) != 2 {
		t.Fatalf("merge = %#v, want both ports", state.Interfaces)
	}
}

func TestDownLinkPersistsPendingLinkWithoutOwnershipChurn(t *testing.T) {
	root := t.TempDir()
	netdev := filepath.Join(root, "class/net/eth1")
	if err := os.MkdirAll(netdev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "carrier"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))

	entry := dataplane.DPDKVerification{LinuxInterface: "eth1", PCIAddress: "0000:13:00.0", Mode: "dpdk_uio", LinkState: "up", Verification: "verified", FunctionalVerified: true}
	updated, changed := demoteOnLinkDown(entry, "down")
	if !changed {
		t.Fatal("expected demotion")
	}
	if err := mergeDPDKVerification(filepath.Join(root, "verification.json"), []dataplane.DPDKVerification{updated}); err != nil {
		t.Fatal(err)
	}
	state, _ := dataplane.LoadDPDKVerification(filepath.Join(root, "verification.json"))
	if len(state.Interfaces) != 1 || state.Interfaces[0].Verification != "pending_link" || state.Interfaces[0].Mode != "dpdk_uio" || state.Interfaces[0].FunctionalVerified {
		t.Fatalf("state = %#v", state.Interfaces)
	}
	if _, err := os.Stat(filepath.Join(root, "ownership.json")); !os.IsNotExist(err) {
		t.Fatalf("down link touched ownership: %v", err)
	}
}

func TestVPPInterfacePresentIgnoresCLIErrorEcho(t *testing.T) {
	if vppCLIInterfacePresent("show interface: unknown input `lyroute-eth1'", "lyroute-eth1") {
		t.Fatal("CLI error text must not count as a present interface")
	}
	if vppCLIInterfacePresent("set interface state: unknown interface `lyroute-eth1 up'", "lyroute-eth1") {
		t.Fatal("unknown-interface error text must not count as present")
	}
	output := "              Name               Idx    State\nlyroute-eth1                    1      up  9000/0/0/0\n"
	if !vppCLIInterfacePresent(output, "lyroute-eth1") {
		t.Fatal("real interface row was not recognized")
	}
}

func TestDPDKVerificationModes(t *testing.T) {
	for _, mode := range []string{"vfio-pci", "uio_pci_generic", "vfio", "uio"} {
		if !isDPDKVerificationMode(mode) {
			t.Fatalf("%s must be a destructive DPDK mode", mode)
		}
	}
	for _, mode := range []string{"", "af_packet", "af_xdp_zero_copy", "rdma"} {
		if isDPDKVerificationMode(mode) {
			t.Fatalf("%s must not be treated as a destructive DPDK mode", mode)
		}
	}
}

func TestFunctionalProbeCrashGuardReleasesDPDKPort(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class/net/eth1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "class/net/eth1/carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show interface lyroute-eth1") printf '%s\n' 'lyroute-eth1 1 down 9000/0/0/0' ;;
  "set interface state lyroute-eth1 up") printf '%s\n' 'crash' >&2; exit 1 ;;
  "show version") exit 0 ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "released.txt")
	release := filepath.Join(root, "release.sh")
	if err := os.WriteFile(release, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$RELEASE_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_DPDK_RELEASE_COMMAND", release)
	t.Setenv("RELEASE_MARKER", marker)
	t.Setenv("LY_ROUTE_VPP_STOP_COMMAND", "true")
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", "true")
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "capabilities.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_STATE_DIR", root)

	server := &Server{}
	entry := dataplane.DPDKVerification{LinuxInterface: "eth1", PCIAddress: "0000:13:00.0", Mode: "uio_pci_generic", Verification: "pending_link"}
	probed := server.functionalProbe(context.Background(), "eth1", entry, map[string]bool{"lyroute-eth1": true})
	if probed.Verification != "failed" {
		t.Fatalf("probed = %#v, want failed", probed)
	}
	capabilities, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, unsupported := capabilities.NativeUnsupported("", "0000:13:00.0"); !unsupported {
		t.Fatal("crash guard was not persisted by the watcher")
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(data)) != "0000:13:00.0" {
		t.Fatalf("release marker = %q, err=%v", data, err)
	}
}
