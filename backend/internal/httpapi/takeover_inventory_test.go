package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ly-route/backend/internal/runtime/dataplane"
)

// A role-less appliance must still register the rule-preferred DPDK ports in the
// operator inventory; the boot preflight derives the devices to keep from it.
func TestRulePreferredInventoryPersistsRolelessPorts(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	inventoryPath := filepath.Join(fixture.root, "inventory.json")
	t.Setenv("LY_ROUTE_DATAPLANE_INVENTORY", inventoryPath)

	existing := dataplane.Inventory{
		Source:     "operator",
		Management: dataplane.InventoryInterface{LinuxInterface: "enp1s0", PCIAddress: "0000:01:00.0", Role: "management"},
		DataInterfaces: []dataplane.InventoryInterface{{
			LinuxInterface: "enp9s9", PCIAddress: "0000:09:00.0", KernelDriver: "igb", Role: "wan",
		}},
	}
	if err := dataplane.SaveInventory(inventoryPath, existing); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The management port is never added as a data interface.
	fixture.server.persistRulePreferredInventory(ctx, []string{"enp2s0", "enp1s0"})
	fixture.server.persistRulePreferredInventory(ctx, []string{"enp2s0"})
	fixture.server.persistRulePreferredInventory(ctx, []string{"enp2s0"})

	inventory, err := dataplane.LoadInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Management.LinuxInterface != "enp1s0" {
		t.Fatalf("management record = %#v", inventory.Management)
	}
	byName := map[string]dataplane.InventoryInterface{}
	for _, entry := range inventory.DataInterfaces {
		if _, duplicate := byName[entry.LinuxInterface]; duplicate {
			t.Fatalf("duplicate inventory entry for %s: %#v", entry.LinuxInterface, inventory.DataInterfaces)
		}
		byName[entry.LinuxInterface] = entry
	}
	if entry, ok := byName["enp9s9"]; !ok || entry.Role != "wan" {
		t.Fatalf("the existing role='wan' record was overwritten: %#v", inventory.DataInterfaces)
	}
	entry, ok := byName["enp2s0"]
	if !ok || entry.Role != "data" || entry.PCIAddress != "0000:02:00.0" || entry.KernelDriver != "igb" {
		t.Fatalf("role-less igb port was not persisted: %#v", inventory.DataInterfaces)
	}
	if _, exists := byName["enp1s0"]; exists {
		t.Fatal("the management port was persisted as a data interface")
	}
}

// Once every data port is bound to DPDK there is no `class/net` entry left.
// The watcher must still enumerate them or it stops persisting the inventory,
// re-attaching and reading the link state.
func TestTakeoverDataInterfacesIncludesDPDKBoundPorts(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	ownership := `{"schema_version":1,"source":"test","state":"active","transport":"uio","devices":[{"pci_address":"0000:02:00.0","linux_interface":"enp2s0","kernel_driver":"igb","transport":"uio"}]}`
	if err := os.WriteFile(filepath.Join(fixture.root, "ownership.json"), []byte(ownership), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeDPDKVerification(filepath.Join(fixture.root, "verification.json"), []dataplane.DPDKVerification{{
		LinuxInterface: "enp2s0", PCIAddress: "0000:02:00.0", Mode: "uio_pci_generic", Verification: "pending_link",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fixture.root, "class/net/enp2s0")); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range fixture.server.takeoverDataInterfaces(context.Background()) {
		if name == "enp2s0" {
			found = true
		}
	}
	if !found {
		t.Fatal("a DPDK-bound port disappeared from the watcher's enumeration")
	}
}

// A DPDK-bound port has no kernel netdev driver. The rule library must still
// match its NIC family through the recorded kernel driver.
func TestRulePreferredTargetUsesTheRecordedDriverForABoundPort(t *testing.T) {
	fixture := newRulePreferredFixture(t, "enp2s0", "0000:02:00.0", "igb", "0x8086", "0x1533")
	device := filepath.Join(fixture.root, "bus/pci/devices/0000:02:00.0")
	uio := filepath.Join(fixture.root, "bus/pci/drivers/uio_pci_generic")
	if err := os.Remove(filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(uio, filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	ownership := `{"schema_version":1,"source":"test","state":"active","transport":"uio","devices":[{"pci_address":"0000:02:00.0","linux_interface":"enp2s0","kernel_driver":"igb","transport":"uio"}]}`
	if err := os.WriteFile(filepath.Join(fixture.root, "ownership.json"), []byte(ownership), 0o600); err != nil {
		t.Fatal(err)
	}
	// The kernel netdev disappears when the device is bound to uio.
	if err := os.RemoveAll(filepath.Join(fixture.root, "class/net/enp2s0")); err != nil {
		t.Fatal(err)
	}
	target, rule, ok := fixture.server.rulePreferredDPDKTarget(context.Background(), "enp2s0")
	if !ok || target != takeoverModeDPDKUIO {
		t.Fatalf("bound port target = %q (ok=%t)", target, ok)
	}
	if rule.ID != "igb-dpdk-uio" {
		t.Fatalf("rule = %q, want igb-dpdk-uio", rule.ID)
	}
}
