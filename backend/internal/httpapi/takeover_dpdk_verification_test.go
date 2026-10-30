package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// A DPDK port has no Linux netdev and no product-named VPP interface, so the
// generic live readback can never confirm it. The proof must come from the VPP
// hardware table, otherwise the old AF_PACKET record survives a DPDK migration.
func TestDPDKMigrationPersistsTheDPDKVerification(t *testing.T) {
	root := t.TempDir()
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show version") exit 0 ;;
  "show interface") printf '%s\n' 'GigabitEthernet1/0/0 5 up 9000/0/0/0' ;;
  "show hardware-interfaces") printf '%s\n' 'GigabitEthernet1/0/0 5 up GigabitEthernet1/0/0' '  pci: device 8086:1533 subsystem 8086:0000 address 0000:02:00.0 numa 0' ;;
  "show hardware-interfaces GigabitEthernet1/0/0") printf '%s\n' 'GigabitEthernet1/0/0 5 up GigabitEthernet1/0/0' '  carrier up full duplex max-frame-size 9022' ;;
esac
exit 0
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	verification := filepath.Join(root, "verification.json")
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", verification)

	server := &Server{now: time.Now}
	snapshot := takeoverSnapshot{LinuxInterface: "enp2s0", Netdev: "enp2s0", PCIAddress: "0000:02:00.0", VPPInterface: "lyroute-enp2s0"}
	if !server.persistDestructiveVerification(context.Background(), snapshot, takeoverModeDPDKUIO, "switch_readback") {
		t.Fatal("the DPDK attachment was not persisted")
	}
	state, err := dataplane.LoadDPDKVerification(verification)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Interfaces) != 1 {
		t.Fatalf("verification = %#v", state.Interfaces)
	}
	entry := state.Interfaces[0]
	if takeoverModeFromDriver(entry.Mode) != takeoverModeDPDKUIO {
		t.Fatalf("entry mode = %q, want the uio binding driver", entry.Mode)
	}
	if !isDPDKVerificationMode(entry.Mode) || entry.Verification != "verified" || entry.LinkState != "up" {
		t.Fatalf("entry = %#v, want dpdk_uio verified with link up", entry)
	}
}
