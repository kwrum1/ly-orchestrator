package httpapi

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// dataplaneInventoryPath is the persisted operator interface mapping the boot
// ownership preflight reads when there is no installer mapping.
func dataplaneInventoryPath() string {
	return envOrDefault("LY_ROUTE_DATAPLANE_INVENTORY", "/etc/ly-route/dataplane-inventory.json")
}

func dataplaneOwnershipPath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_OWNERSHIP_STATE")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "ownership.json")
}

func sysfsRoot() string {
	return envOrDefault("LY_ROUTE_SYSFS_ROOT", "/sys")
}

func readInventoryInterface(name string) (dataplane.InventoryInterface, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return dataplane.InventoryInterface{}, false
	}
	item := dataplane.InventoryInterface{LinuxInterface: name, Role: "data"}
	device := filepath.Join(sysfsRoot(), "class/net", name, "device")
	if resolved, err := filepath.EvalSymlinks(device); err == nil {
		item.PCIAddress = filepath.Base(resolved)
		if driver, err := filepath.EvalSymlinks(filepath.Join(device, "driver")); err == nil {
			item.KernelDriver = filepath.Base(driver)
		}
		if mac, err := os.ReadFile(filepath.Join(sysfsRoot(), "class/net", name, "address")); err == nil {
			item.MACAddress = strings.ToLower(strings.TrimSpace(string(mac)))
		}
	}
	// The NIC may already be owned by VPP. Recover the identity from the
	// ownership record written by prepare-vfio.sh or the dataplane transaction.
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, record := range state.Devices {
			if strings.TrimSpace(record.LinuxInterface) != name {
				continue
			}
			if item.PCIAddress == "" {
				item.PCIAddress = strings.TrimSpace(record.PCIAddress)
			}
			item.KernelDriver = strings.TrimSpace(record.KernelDriver)
			if item.MACAddress == "" {
				item.MACAddress = strings.ToLower(strings.TrimSpace(record.MACAddress))
			}
			break
		}
	}
	if item.PCIAddress == "" {
		return item, false
	}
	return item, true
}

func (server *Server) writeDataplaneInventory(ctx context.Context, interfaces []string) error {
	inventory := dataplane.Inventory{Source: "ly-route-control-api"}
	management, managementFound := readInventoryInterface(server.managementInterfaceID(ctx))
	if !managementFound {
		management = dataplane.InventoryInterface{LinuxInterface: server.managementInterfaceID(ctx), Role: "management"}
	}
	management.Role = "management"
	inventory.Management = management
	seen := map[string]bool{}
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		item, ok := readInventoryInterface(name)
		if !ok {
			continue
		}
		item.Role = "data"
		inventory.DataInterfaces = append(inventory.DataInterfaces, item)
	}
	sort.Slice(inventory.DataInterfaces, func(left, right int) bool {
		return inventory.DataInterfaces[left].LinuxInterface < inventory.DataInterfaces[right].LinuxInterface
	})
	if len(inventory.DataInterfaces) == 0 && inventory.Management.PCIAddress == "" {
		return nil
	}
	path := dataplaneInventoryPath()
	previous, err := dataplane.LoadInventory(path)
	if err != nil {
		return err
	}
	if inventoryEquivalent(previous, inventory) {
		return nil
	}
	inventory.UpdatedAt = server.now().UTC()
	return dataplane.SaveInventory(path, inventory)
}

func (server *Server) persistDataplaneInventory(ctx context.Context, interfaces []string) {
	if err := server.writeDataplaneInventory(ctx, interfaces); err != nil {
		log.Printf("persist dataplane interface inventory: %v", err)
	}
}

func inventoryEquivalent(previous, next dataplane.Inventory) bool {
	previous.UpdatedAt = time.Time{}
	next.UpdatedAt = time.Time{}
	previous.SchemaVersion = next.SchemaVersion
	return reflect.DeepEqual(previous.Management, next.Management) && reflect.DeepEqual(previous.DataInterfaces, next.DataInterfaces)
}

func dataInterfaceOnKernelDriver(name string) bool {
	device := filepath.Join(sysfsRoot(), "class/net", strings.TrimSpace(name), "device")
	driver, err := filepath.EvalSymlinks(filepath.Join(device, "driver"))
	if err != nil {
		return false
	}
	current := filepath.Base(driver)
	return current != "" && current != "." && current != "vfio-pci" && current != "uio_pci_generic"
}

// bootstrapDataplaneOwnership performs the same ownership preflight the boot
// service runs, but on demand before an apply. It exists because an operator
// can configure interfaces through the product API on a freshly installed
// image: there is no installer mapping and the boot service had nothing to
// bind. The command is a no-op unless a configured data NIC is still owned by
// its kernel driver, so a repeated apply and a VPP-owned device never re-bind.
func (server *Server) bootstrapDataplaneOwnership(ctx context.Context) bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_BOOTSTRAP")), "false") {
		return false
	}
	command := envOrDefault("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", "/usr/lib/ly-route/prepare-vfio.sh")
	info, err := os.Stat(command)
	if err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	interfaces := server.runtimeDataInterfaces(ctx)
	if len(interfaces) == 0 {
		return false
	}
	needsOwnership := false
	for _, name := range interfaces {
		if dataInterfaceOnKernelDriver(name) {
			needsOwnership = true
			break
		}
	}
	if !needsOwnership {
		return false
	}
	if err := server.writeDataplaneInventory(ctx, interfaces); err != nil {
		log.Printf("dataplane ownership bootstrap inventory: %v", err)
		return false
	}
	bootstrap := exec.CommandContext(ctx, command)
	bootstrap.Env = append(os.Environ(), "LY_ROUTE_VFIO_BOOTSTRAP=apply")
	if output, err := bootstrap.CombinedOutput(); err != nil {
		log.Printf("dataplane ownership bootstrap failed: %v: %s", err, strings.TrimSpace(string(output)))
		return false
	}
	restart := envOrDefault("LY_ROUTE_VPP_RESTART_COMMAND", "systemctl restart vpp.service")
	if output, err := exec.CommandContext(ctx, "/bin/sh", "-c", restart).CombinedOutput(); err != nil {
		log.Printf("dataplane ownership VPP restart failed: %v: %s", err, strings.TrimSpace(string(output)))
		return false
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := exec.CommandContext(ctx, vppctl, "show", "version").Run(); err == nil {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			log.Printf("dataplane ownership bootstrap: VPP did not become ready within 30s")
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}
