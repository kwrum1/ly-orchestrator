package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ly-route/backend/internal/runtime/takeover"
)

// takeoverIdentityFixture installs a predictable-name host: the API product id
// is eth1 while the kernel netdev is enp1s0.
func takeoverIdentityFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "ens192")
	t.Setenv("LY_ROUTE_SYSFS_ROOT", root)
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", filepath.Join(root, "inventory.json"))
	t.Setenv("LY_ROUTE_DPDK_OWNERSHIP_STATE", filepath.Join(root, "ownership.json"))
	t.Setenv("LY_ROUTE_DPDK_VERIFICATION_STATE", filepath.Join(root, "verification.json"))
	t.Setenv("LY_ROUTE_DATAPLANE_ACTIVE_STATE", filepath.Join(root, "active.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE", filepath.Join(root, "takeover-capabilities.json"))
	t.Setenv("LY_ROUTE_TAKEOVER_STATE", filepath.Join(root, "takeover-state.json"))
	device := filepath.Join(root, "bus/pci/devices/0000:13:00.0")
	driver := filepath.Join(root, "bus/pci/drivers/igc")
	netdev := filepath.Join(root, "class/net/enp1s0")
	for _, directory := range []string{device, driver, netdev} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driver, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(netdev, "address"), []byte("00:50:56:aa:bb:01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{
			{"id": "ens192", "name": "ens192", "pci": "0000:0b:00.0", "mac": "00:50:56:aa:bb:00"},
			{"id": "enp1s0", "name": "enp1s0", "pci": "0000:13:00.0", "mac": "00:50:56:aa:bb:01"},
		}
	}
	t.Cleanup(func() { hostInterfaceInventory = previous })
	return root
}

// addTakeoverFixturePort creates a sysfs netdev backed by a PCI device.
func addTakeoverFixturePort(t *testing.T, root, name, pci, driver string) {
	t.Helper()
	device := filepath.Join(root, "bus/pci/devices", pci)
	driverDir := filepath.Join(root, "bus/pci/drivers", driver)
	netdev := filepath.Join(root, "class/net", name)
	for _, directory := range []string{device, driverDir, netdev} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(driverDir, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(netdev, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestTakeoverIdentityResolvesProductIDToLinuxName(t *testing.T) {
	takeoverIdentityFixture(t)
	server := &Server{}
	snapshot := server.detectTakeoverSnapshot(context.Background(), "eth1", nil)
	if snapshot.LinuxInterface != "enp1s0" || snapshot.PCIAddress != "0000:13:00.0" || snapshot.KernelDriver != "igc" {
		t.Fatalf("snapshot = %#v, want the enp1s0 device", snapshot)
	}
}

func TestPhysicalPortEnumerationExcludesManagementAndVirtualInterfaces(t *testing.T) {
	root := takeoverIdentityFixture(t)
	addTakeoverFixturePort(t, root, "ens192", "0000:0b:00.0", "igb") // management
	addTakeoverFixturePort(t, root, "enp3s0", "0000:03:00.0", "igc")
	// Virtual netdevs: a bridge, an LCP host interface and a bond.
	for _, name := range []string{"br-lan", "lylan-enp3s0", "bond0"} {
		if err := os.MkdirAll(filepath.Join(root, "class/net", name, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{}
	ports := server.physicalDataInterfaces(context.Background())
	want := map[string]bool{"enp1s0": true, "enp3s0": true}
	if len(ports) != len(want) {
		t.Fatalf("physical ports = %v, want %v", ports, want)
	}
	for _, port := range ports {
		if !want[port] {
			t.Fatalf("physical ports = %v, unexpected %s", ports, port)
		}
	}
	for _, port := range ports {
		if plan := server.takeoverPlanForPort(context.Background(), port); plan.Skipped != "" {
			t.Fatalf("physical port %s was skipped: %s", port, plan.Skipped)
		}
	}
	// The management port is never part of the takeover input.
	for _, port := range server.takeoverDataInterfaces(context.Background()) {
		if port == "ens192" {
			t.Fatal("management port leaked into the takeover input")
		}
	}
}

func TestManagementPortPlanIsSkipped(t *testing.T) {
	root := takeoverIdentityFixture(t)
	addTakeoverFixturePort(t, root, "ens192", "0000:0b:00.0", "igb")
	server := &Server{}
	plan := server.takeoverPlanForPort(context.Background(), "ens192")
	if plan.Skipped == "" || len(plan.Candidates) != 0 {
		t.Fatalf("management plan = %#v, want a skipped plan", plan)
	}
}

func TestAutomaticPlanForIGCPortIsDPDKUIOFirst(t *testing.T) {
	root := takeoverIdentityFixture(t)
	startup := filepath.Join(root, "startup.conf")
	if err := os.WriteFile(startup, []byte("plugin dpdk_plugin.so { enable }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		filepath.Join(root, "bus/pci/drivers/uio_pci_generic"),
		filepath.Join(root, "bus/pci/drivers/vfio-pci"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LY_ROUTE_VPP_STARTUP_CONFIG", startup)
	t.Setenv("LY_ROUTE_DPDK_HUGEPAGES", "512")
	t.Setenv("LY_ROUTE_DPDK_TRANSPORT", "uio")

	server := &Server{}
	plan := server.takeoverPlanForPort(context.Background(), "enp1s0")
	want := []takeover.Mode{takeover.ModeDPDKUIO, takeover.ModeAFXDPCopy, takeover.ModeAFPacket}
	got := plan.Modes()
	if len(got) != len(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("plan = %v, want %v", got, want)
		}
	}
	if plan.RuleID != "igc-dpdk-uio" {
		t.Fatalf("rule = %q, want igc-dpdk-uio", plan.RuleID)
	}
}
