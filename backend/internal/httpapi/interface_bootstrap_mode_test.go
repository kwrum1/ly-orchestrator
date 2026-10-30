package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A role edit on a port that VPP already owns must never reach the destructive
// takeover preflight. Losing a persisted proof for one generation is not a
// reason to bind dpdk_uio and restart VPP.
func TestRoleEditDoesNotRebindPortAlreadyOwnedByVPP(t *testing.T) {
	root := t.TempDir()
	netdev := filepath.Join(root, "class", "net", "enp3s0")
	device := filepath.Join(root, "bus", "pci", "devices", "0000:13:00.0")
	driver := filepath.Join(root, "bus", "pci", "drivers", "vmxnet3")
	for _, directory := range []string{netdev, device, driver} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "address"), []byte("00:50:56:cc:00:02\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)

	prepareMarker := filepath.Join(root, "prepared.txt")
	prepare := filepath.Join(root, "prepare.sh")
	if err := os.WriteFile(prepare, []byte("#!/bin/sh\nprintf 'prepared\\n' >> \"$PREPARE_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREPARE_MARKER", prepareMarker)
	t.Setenv("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", prepare)

	restartMarker := filepath.Join(root, "restarted.txt")
	restart := filepath.Join(root, "restart.sh")
	if err := os.WriteFile(restart, []byte("#!/bin/sh\nprintf 'restarted\\n' >> \"$RESTART_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTART_MARKER", restartMarker)
	t.Setenv("LY_ROUTE_VPP_RESTART_COMMAND", restart)

	vppctl := filepath.Join(root, "vppctl")
	script := "#!/bin/sh\ncase \"$*\" in\n  \"show interface\") printf '%s\\n' 'lyroute-enp3s0 1 up 9000/0/0/0' ;;\nesac\nexit 0\n"
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	journalPath := filepath.Join(root, "takeover-state.json")
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", journalPath)

	dataplaneBootstrapBlockedUntil = time.Time{}
	t.Cleanup(func() { dataplaneBootstrapBlockedUntil = time.Time{} })

	server, _ := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp3s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	seedInterfaceContractDocument(t, server, port, interfaceContractPayload(port, "enp3s0", "lan", "192.168.89.1/24"))

	if attempted := server.bootstrapDataplaneOwnership(ctx); attempted {
		t.Fatal("role-owned port was offered to the destructive takeover preflight")
	}
	for _, marker := range []string{prepareMarker, restartMarker} {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("destructive path ran for an already owned port: %s exists", marker)
		}
	}
	// The old implicit "VPP owns the port, so skip" branch is gone: the port is
	// kept by an explicit, recorded decision that names the current mode.
	decision, ok := lastTakeoverTransition("enp3s0")
	if !ok {
		t.Fatalf("no takeover decision was recorded at %s", journalPath)
	}
	if decision.Status != takeoverStatusKept || decision.FromMode != takeoverModeAFPacket {
		t.Fatalf("decision = %#v, want kept in af_packet", decision)
	}
}
