package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindVPPInterfaceByPCIUsesHardwareReadback(t *testing.T) {
	output := `              Name                Idx   Link  Hardware
GigabitEthernet13/0/0              1    down  GigabitEthernet13/0/0
  Ethernet address 00:50:56:3a:13:02
  VMware vmxnet3
    pci: device 15ad:07b0 subsystem 15ad:07b0 address 0000:13:00.00 numa 0
GigabitEthernet1b/0/0              2    down  GigabitEthernet1b/0/0
  Ethernet address 00:50:56:3a:13:03
  VMware vmxnet3
    pci: device 15ad:07b0 subsystem 15ad:07b0 address 0000:1b:00.00 numa 0
`
	if got := findVPPInterfaceByPCI(output, "0000:1b:00.0"); got != "GigabitEthernet1b/0/0" {
		t.Fatalf("interface = %q, want GigabitEthernet1b/0/0", got)
	}
	if got := findVPPInterfaceByPCI(output, "0000:99:00.0"); got != "" {
		t.Fatalf("unexpected interface %q", got)
	}
}

func TestVerifyDPDKTakeoversKeepsDownVPPInterfaceAsPendingLink(t *testing.T) {
	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.json")
	if err := os.WriteFile(inventoryPath, []byte(`{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","kernel_driver":"vmxnet3"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(root, "bus/pci/devices/0000:13:00.0")
	driver := filepath.Join(root, "bus/pci/drivers/uio_pci_generic")
	for _, directory := range []string{device, driver} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show hardware-interfaces") cat <<'OUT'
              Name                Idx   Link  Hardware
GigabitEthernet13/0/0              1    down  GigabitEthernet13/0/0
  VMware vmxnet3
    pci: device 15ad:07b0 subsystem 15ad:07b0 address 0000:13:00.00 numa 0
OUT
    ;;
  "show interface GigabitEthernet13/0/0") printf '%s\n' 'GigabitEthernet13/0/0 1 down 9000/0/0/0' ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	server := &Server{}
	verified, failures := server.verifyDPDKTakeovers(context.Background(), []string{"ens224"})
	if len(failures) != 0 {
		t.Fatalf("failures = %#v, want none for a down link", failures)
	}
	if len(verified) != 1 || verified[0].Verification != "pending_link" || verified[0].FunctionalVerified {
		t.Fatalf("verified = %#v, want pending_link without functional proof", verified)
	}
}

func TestReleaseFailedDPDKTakeoversInvokesReleaseHelper(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "released.txt")
	helper := filepath.Join(root, "release.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$RELEASE_MARKER\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_DPDK_RELEASE_COMMAND", helper)
	t.Setenv("RELEASE_MARKER", marker)
	server := &Server{}
	released := server.releaseFailedDPDKTakeovers(context.Background(), []dpdkTakeoverFailure{{LinuxInterface: "ens224", PCIAddress: "0000:13:00.0", Reason: "down"}})
	if !released {
		t.Fatal("release helper was not reported")
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(data)) != "0000:13:00.0" {
		t.Fatalf("release marker = %q, err=%v", data, err)
	}
}

func TestVerifyDPDKTakeoversAcceptsUpInterfaceRegardlessOfHardwareCarrier(t *testing.T) {
	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.json")
	if err := os.WriteFile(inventoryPath, []byte(`{"schema_version":1,"management":{"linux_interface":"ens192","pci":"0000:0b:00.0"},"data_interfaces":[{"linux_interface":"ens224","pci":"0000:13:00.0","kernel_driver":"vmxnet3"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(root, "bus/pci/devices/0000:13:00.0")
	driver := filepath.Join(root, "bus/pci/drivers/uio_pci_generic")
	for _, directory := range []string{device, driver} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	vppctl := filepath.Join(root, "vppctl")
	script := `#!/bin/sh
case "$*" in
  "show hardware-interfaces") cat <<'OUT'
              Name                Idx   Link  Hardware
GigabitEthernet13/0/0              1    down  GigabitEthernet13/0/0
  VMware vmxnet3
    pci: device 15ad:07b0 subsystem 15ad:07b0 address 0000:13:00.00 numa 0
OUT
    ;;
  "show hardware-interfaces GigabitEthernet13/0/0") cat <<'OUT'
GigabitEthernet13/0/0 1 down
  Link speed: 10.000000 Gbps
  VMware VMXNET3
    carrier down
    pci: device 15ad:07b0 address 0000:13:00.00
OUT
    ;;
  "show interface GigabitEthernet13/0/0") printf '%s\n' 'GigabitEthernet13/0/0 1 up 9000/0/0/0' ;;
  "set interface state GigabitEthernet13/0/0 up") exit 0 ;;
esac
`
	if err := os.WriteFile(vppctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_VPPCTL", vppctl)
	server := &Server{}
	verified, failures := server.verifyDPDKTakeovers(context.Background(), []string{"ens224"})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %#v", failures)
	}
	if len(verified) != 1 || !verified[0].FunctionalVerified || verified[0].Method != "link_state_up" {
		t.Fatalf("verified = %#v, want link_state_up functional verification", verified)
	}
	if verified[0].HardwareLink != "down" {
		t.Fatalf("hardware link = %q, want down", verified[0].HardwareLink)
	}
}

func TestVPPFunctionalMetadataParsing(t *testing.T) {
	hardware := `GigabitEthernet13/0/0 1 down
  Link speed: 10.000000 Gbps
  VMware VMXNET3
    carrier down
`
	if got := vppLinkSpeedMbps(hardware); got != 10000 {
		t.Fatalf("link speed = %d, want 10000", got)
	}
	if got := vppHardwareCarrier(hardware); got != "down" {
		t.Fatalf("carrier = %q, want down", got)
	}
	interfaceOutput := `GigabitEthernet13/0/0 1 up
  tx packets 7
  rx packets 3
`
	tx, rx := vppInterfaceCounters(interfaceOutput, "GigabitEthernet13/0/0")
	if tx != 7 || rx != 3 {
		t.Fatalf("counters = tx %d rx %d, want 7/3", tx, rx)
	}
}

func TestVPPInterfaceStateParsesShowInterface(t *testing.T) {
	output := `              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)     Counter          Count
GigabitEthernet13/0/0             1     down         9000/0/0/0
GigabitEthernet1b/0/0             2      up          9000/0/0/0
`
	if got := vppInterfaceState(output, "GigabitEthernet13/0/0"); got != "down" {
		t.Fatalf("state = %q, want down", got)
	}
	if got := vppInterfaceState(output, "GigabitEthernet1b/0/0"); got != "up" {
		t.Fatalf("state = %q, want up", got)
	}
}
