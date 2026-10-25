package httpapi

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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

func dataplaneVerificationPath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_VERIFICATION_STATE")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "dpdk-verification.json")
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

type dpdkTakeoverFailure struct {
	LinuxInterface string
	PCIAddress     string
	Reason         string
}

// verifyDPDKTakeovers checks the destructive candidates after VPP has started.
// A takeover is only verified when the VPP interface for the PCI exists and its
// show-interface state is up. The hardware model is deliberately not a gate:
// any NIC that VPP attaches and brings up is a valid native path.
func (server *Server) verifyDPDKTakeovers(ctx context.Context, interfaces []string) ([]dataplane.DPDKVerification, []dpdkTakeoverFailure) {
	inventory, err := dataplane.LoadInventory(dataplaneInventoryPath())
	if err != nil {
		return nil, []dpdkTakeoverFailure{{Reason: "dataplane inventory is unreadable: " + err.Error()}}
	}
	byInterface := map[string]dataplane.InventoryInterface{}
	for _, item := range inventory.DataInterfaces {
		byInterface[strings.TrimSpace(item.LinuxInterface)] = item
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	hardwareOutput, hardwareErr := exec.CommandContext(ctx, vppctl, "show", "hardware-interfaces").Output()
	if hardwareErr != nil {
		return nil, []dpdkTakeoverFailure{{Reason: "vppctl show hardware-interfaces failed: " + hardwareErr.Error()}}
	}
	peer := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_FUNCTIONAL_PEER"))
	verified := []dataplane.DPDKVerification{}
	failures := []dpdkTakeoverFailure{}
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		item, ok := byInterface[name]
		if !ok || strings.TrimSpace(item.PCIAddress) == "" {
			continue
		}
		pci := strings.TrimSpace(item.PCIAddress)
		driverPath := filepath.Join(sysfsRoot(), "bus/pci/devices", pci, "driver")
		driver, driverErr := filepath.EvalSymlinks(driverPath)
		if driverErr != nil {
			continue
		}
		current := filepath.Base(driver)
		if current != "vfio-pci" && current != "uio_pci_generic" {
			continue
		}
		vppInterface := findVPPInterfaceByPCI(string(hardwareOutput), pci)
		if vppInterface == "" {
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Reason: "VPP did not create an interface for the PCI device"})
			continue
		}
		// Admin-up is part of the verification. On some PMDs (vmxnet3 in this
		// environment) this can abort VPP; that is a failed native candidate,
		// not a reason to trust a stale status string.
		upCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		upOutput, upErr := exec.CommandContext(upCtx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
		cancel()
		if upErr != nil {
			failures = append(failures, dpdkTakeoverFailure{
				LinuxInterface: name,
				PCIAddress:     pci,
				Reason:         "set interface state up failed: " + upErr.Error() + ": " + strings.TrimSpace(string(upOutput)),
			})
			continue
		}
		interfaceOutput, interfaceErr := exec.CommandContext(ctx, vppctl, "show", "interface", vppInterface).Output()
		if interfaceErr != nil {
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Reason: "VPP interface readback failed: " + interfaceErr.Error()})
			continue
		}
		hardwareInterface, hardwareInterfaceErr := exec.CommandContext(ctx, vppctl, "show", "hardware-interfaces", vppInterface).Output()
		if hardwareInterfaceErr != nil {
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Reason: "VPP hardware readback failed: " + hardwareInterfaceErr.Error()})
			continue
		}
		state := vppInterfaceState(string(interfaceOutput), vppInterface)
		hardwareLink := vppHardwareCarrier(string(hardwareInterface))
		linkSpeed := vppLinkSpeedMbps(string(hardwareInterface))
		verification := dataplane.DPDKVerification{
			LinuxInterface: name,
			PCIAddress:     pci,
			LinkState:      state,
			HardwareLink:   hardwareLink,
			LinkSpeedMbps:  linkSpeed,
			ObservedAt:     time.Now().UTC(),
		}
		if state == "up" {
			verification.FunctionalVerified = true
			verification.Method = "link_state_up"
		} else if linkSpeed > 0 && peer != "" {
			before, _ := vppInterfaceCounters(string(interfaceOutput), vppInterface)
			pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
			_, _ = exec.CommandContext(pingCtx, vppctl, "ping", peer, "source", vppInterface, "repeat", "1").CombinedOutput()
			cancelPing()
			afterOutput, _ := exec.CommandContext(ctx, vppctl, "show", "interface", vppInterface).Output()
			after, _ := vppInterfaceCounters(string(afterOutput), vppInterface)
			verification.TXBefore = before
			verification.TXAfter = after
			if after > before {
				verification.FunctionalVerified = true
				verification.Method = "tx_counter_and_link_speed"
			} else {
				verification.Reason = "VPP status is down and no TX counter increment was observed"
			}
		} else if state != "up" {
			verification.Reason = "VPP status is down and no functional traffic/link-speed proof is available"
		}
		if !verification.FunctionalVerified {
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Reason: nonEmpty(verification.Reason, "functional verification failed")})
			continue
		}
		verified = append(verified, verification)
	}
	return verified, failures
}

func findVPPInterfaceByPCI(output, pci string) string {
	padded := pci
	if dot := strings.LastIndexByte(pci, '.'); dot >= 0 && len(pci)-dot == 2 {
		padded = pci[:dot+1] + "0" + pci[dot+1:]
	}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if line != "" && line[0] != ' ' && line[0] != '\t' && trimmed != "" {
			current = strings.Fields(trimmed)[0]
			continue
		}
		if strings.Contains(line, "address "+pci) || strings.Contains(line, "address "+padded) {
			return current
		}
	}
	return ""
}

func vppInterfaceState(output, name string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		for _, field := range fields {
			if field == "up" || field == "down" {
				return field
			}
		}
	}
	return ""
}

func vppHardwareCarrier(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field == "carrier" && index+1 < len(fields) {
				return fields[index+1]
			}
		}
	}
	return "unknown"
}

func vppLinkSpeedMbps(output string) int64 {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Link speed:") {
			continue
		}
		fields := strings.Fields(strings.SplitN(line, "Link speed:", 2)[1])
		if len(fields) == 0 {
			return 0
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0
		}
		unit := ""
		if len(fields) > 1 {
			unit = strings.ToLower(fields[1])
		}
		switch {
		case strings.HasPrefix(unit, "gbps"):
			return int64(value * 1000)
		case strings.HasPrefix(unit, "mbps"):
			return int64(value)
		case strings.HasPrefix(unit, "kbps"):
			return int64(value / 1000)
		default:
			return int64(value)
		}
	}
	return 0
}

func vppInterfaceCounters(output, name string) (int64, int64) {
	var tx, rx int64
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "tx" && len(fields) >= 3 && fields[1] == "packets" {
			if value, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				tx = value
			}
		}
		if fields[0] == "rx" && len(fields) >= 3 && fields[1] == "packets" {
			if value, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				rx = value
			}
		}
	}
	return tx, rx
}

func (server *Server) releaseFailedDPDKTakeovers(ctx context.Context, failures []dpdkTakeoverFailure) bool {
	if len(failures) == 0 {
		return false
	}
	command := envOrDefault("LY_ROUTE_DPDK_RELEASE_COMMAND", "/usr/lib/ly-route/release-dpdk-port.sh")
	released := false
	for _, failure := range failures {
		if strings.TrimSpace(failure.PCIAddress) == "" {
			log.Printf("dataplane takeover rejected: %s: %s", failure.LinuxInterface, failure.Reason)
			continue
		}
		output, err := exec.CommandContext(ctx, command, failure.PCIAddress).CombinedOutput()
		if err != nil {
			log.Printf("dataplane takeover release failed for %s (%s): %v: %s", failure.LinuxInterface, failure.PCIAddress, err, strings.TrimSpace(string(output)))
			continue
		}
		log.Printf("dataplane takeover rejected: %s (%s): %s; restored kernel driver", failure.LinuxInterface, failure.PCIAddress, failure.Reason)
		released = true
	}
	return released
}

func (server *Server) restartVPPAndWait(ctx context.Context) bool {
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
	if !server.restartVPPAndWait(ctx) {
		// The candidate VPP start failed. Return every candidate port to its
		// kernel driver so the AF fallback can still be probed.
		failures := []dpdkTakeoverFailure{}
		for _, name := range interfaces {
			if item, ok := readInventoryInterface(name); ok {
				failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: item.PCIAddress, Reason: "VPP failed to start with the candidate DPDK devices"})
			}
		}
		_ = server.releaseFailedDPDKTakeovers(ctx, failures)
		_ = server.restartVPPAndWait(ctx)
		return true
	}
	verified, failures := server.verifyDPDKTakeovers(ctx, interfaces)
	if len(failures) > 0 {
		if server.releaseFailedDPDKTakeovers(ctx, failures) {
			if !server.restartVPPAndWait(ctx) {
				return false
			}
		}
	}
	if err := dataplane.SaveDPDKVerification(dataplaneVerificationPath(), dataplane.DPDKVerificationState{Interfaces: verified}); err != nil {
		log.Printf("persist DPDK functional verification: %v", err)
	}
	return true
}
