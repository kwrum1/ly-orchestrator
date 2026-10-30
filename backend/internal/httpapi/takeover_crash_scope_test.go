package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ly-route/backend/internal/runtime/dataplane"
)

func TestVerifyDPDKTakeoversGuardsOnlyTheCrashingCandidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_VPP_VERSION", "test-vpp")
	t.Setenv("LY_ROUTE_VPP_RUNTIME_ID", "test-runtime")
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")

	inventory := `{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[` +
		`{"linux_interface":"ens224","pci":"0000:13:00.0","kernel_driver":"igb"},` +
		`{"linux_interface":"ens256","pci":"0000:1b:00.0","kernel_driver":"igb"}]}`
	if err := os.WriteFile(filepath.Join(root, "inventory.json"), []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(root, "bus/pci/drivers/uio_pci_generic")
	if err := os.MkdirAll(driver, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pci := range []string{"0000:13:00.0", "0000:1b:00.0"} {
		device := filepath.Join(root, "bus/pci/devices", pci)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	vppctl := filepath.Join(root, "vppctl")
	calls := filepath.Join(root, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CALL_LOG"
case "$*" in
  "show version") exit 0 ;;
  "show hardware-interfaces") cat <<'OUT'
              Name                Idx   Link  Hardware
GigabitEthernet1/0/0               1    up    GigabitEthernet1/0/0
    pci: device 8086:125c subsystem 8086:0000 address 0000:13:00.0 numa 0
GigabitEthernet2/0/0               2    up    GigabitEthernet2/0/0
    pci: device 8086:125c subsystem 8086:0000 address 0000:1b:00.0 numa 0
OUT
    ;;
  "set interface state GigabitEthernet1/0/0 up") exit 0 ;;
  "set interface state GigabitEthernet2/0/0 up") printf 'connect: Connection refused\n' >&2; exit 1 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CALL_LOG", calls)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)

	server := &Server{}
	verified, failures := server.verifyDPDKTakeovers(context.Background(), []string{"ens224", "ens256"})
	if len(failures) != 1 {
		t.Fatalf("failures = %#v, want only the crashed candidate", failures)
	}
	if !failures[0].Crashed || failures[0].LinuxInterface != "ens256" {
		t.Fatalf("failure = %#v, want the crashing candidate guarded", failures[0])
	}
	if len(verified) != 1 || verified[0].LinuxInterface != "ens224" {
		t.Fatalf("verified = %#v, a healthy port must keep its proof", verified)
	}

	server.recordTakeoverCrashGuard(failures)
	capabilities, err := dataplane.LoadTakeoverCapabilities(filepath.Join(root, "takeover-capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{}
	for _, device := range capabilities.Devices {
		if device.NativeUnsupported {
			guarded[device.PCIAddress] = true
		}
	}
	if !guarded["0000:1b:00.0"] {
		t.Fatalf("guarded = %#v, want the crashing PCI", guarded)
	}
	if guarded["0000:13:00.0"] {
		t.Fatalf("guarded = %#v, the healthy neighbour must stay retryable", guarded)
	}
}
