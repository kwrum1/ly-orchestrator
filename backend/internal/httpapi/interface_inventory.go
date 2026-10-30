package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"ly-route/backend/internal/runtime/dataplane"
)

// Port inventory used by write-time validation.
//
// The dataplane preflight persists the operator-visible port mapping on the
// appliance (/etc/ly-route/dataplane-inventory.json). That file is the authority
// for "does this port exist?", but only while it still describes this host:
// build machines carry a stale copy from earlier local runs, and unit tests
// describe fixtures that have nothing to do with the build host's NICs. The
// gate is therefore the management port named by the inventory still existing
// in sysfs (and matching its MAC when the file records one). If it does not,
// validation stays out of the way instead of rejecting every synthetic port.

func (server *Server) operatorInterfaceInventory(ctx context.Context) (dataplane.Inventory, bool) {
	inventory, err := dataplane.LoadInventory(dataplaneInventoryPath())
	if err != nil {
		return dataplane.Inventory{}, false
	}
	management := strings.TrimSpace(inventory.Management.LinuxInterface)
	if management == "" || len(inventory.DataInterfaces) == 0 {
		return dataplane.Inventory{}, false
	}
	if _, statErr := os.Stat(filepath.Join(sysfsRoot(), "class", "net", management)); statErr != nil {
		return dataplane.Inventory{}, false
	}
	if recorded := strings.ToLower(strings.TrimSpace(inventory.Management.MACAddress)); recorded != "" {
		if live := sysfsMAC(management); live != "" && live != recorded {
			return dataplane.Inventory{}, false
		}
	}
	return inventory, true
}

// livePhysicalDataPort reports whether the kernel still exposes this netdev as
// a physical data port. Write-time validation treats the persisted inventory as
// the authority only while it describes this host, but the inventory can lag a
// binding change (deleting a WAN line used to drop the port from it). A live
// physical NIC therefore also satisfies the check, so the operator can
// re-create the line without an out-of-band inventory repair; the next apply
// rewrites the inventory from the interface table.
func (server *Server) livePhysicalDataPort(linuxInterface string) bool {
	linuxInterface = strings.TrimSpace(linuxInterface)
	if linuxInterface == "" || !vppInterfaceNameSafe(linuxInterface) {
		return false
	}
	if _, err := os.Stat(filepath.Join(sysfsRoot(), "class", "net", linuxInterface)); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(sysfsRoot(), "class", "net", linuxInterface, "device")); err != nil {
		return false
	}
	return true
}

// inventoryHasInterface matches a resolved Linux netdev name against the
// inventory, including the management port.
func inventoryHasInterface(inventory dataplane.Inventory, linuxInterface string) bool {
	linuxInterface = strings.TrimSpace(linuxInterface)
	if linuxInterface == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(inventory.Management.LinuxInterface), linuxInterface) {
		return true
	}
	for _, item := range inventory.DataInterfaces {
		if candidate := strings.TrimSpace(item.LinuxInterface); candidate != "" && strings.EqualFold(candidate, linuxInterface) {
			return true
		}
	}
	return false
}
