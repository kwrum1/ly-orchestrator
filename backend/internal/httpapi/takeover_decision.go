package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
)

// This file bridges the backend-only decision package to the appliance: it
// enumerates the physical ports, reads the facts the decision needs and never
// consults a client-supplied mode.

// takeoverVirtualNamePrefixes are netdev families that are never "physical data
// ports": the product's own VPP/LCP interfaces, taps, bridges, bonds, VLANs and
// the probe pairs.
var takeoverVirtualNamePrefixes = []string{
	"lyroute-", "lylan-", "lyprobe", "lypxh", "lydns", "lypx",
	"veth", "tap", "tun", "br-", "bond", "vlan", "docker", "virbr", "wg", "dummy", "ifb",
}

func isVirtualNetdevName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "lo" {
		return true
	}
	for _, prefix := range takeoverVirtualNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// physicalDataInterfaces enumerates the physical data ports of this appliance:
// every kernel netdev backed by a device (PCI or SoC platform device), minus the
// management port and the product's own virtual interfaces. It is the input to
// the automatic takeover; LAN/WAN are logical ports and never appear here.
func (server *Server) physicalDataInterfaces(ctx context.Context) []string {
	root := filepath.Join(sysfsRoot(), "class", "net")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	management := strings.TrimSpace(server.managementInterfaceID(ctx))
	managementPCI := ""
	if management != "" {
		managementPCI = inventoryPCI(management)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name())
		if isVirtualNetdevName(name) || name == management {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, name, "device")); err != nil {
			// No device link: a pure software interface.
			continue
		}
		if pci := inventoryPCI(name); pci != "" && managementPCI != "" && pci == managementPCI {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// takeoverDataInterfaces is the union of the role-assigned ports and every
// physical port. The product takes over all physical ports automatically; the
// role list only adds ports whose netdev may be named differently.
func (server *Server) takeoverDataInterfaces(ctx context.Context) []string {
	seen := map[string]bool{}
	names := make([]string, 0, 8)
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || name == server.managementInterfaceID(ctx) {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, name := range server.runtimeDataInterfaces(ctx) {
		appendName(name)
	}
	for _, name := range server.physicalDataInterfaces(ctx) {
		appendName(name)
	}
	// A DPDK-bound port has no Linux netdev, so it is absent from class/net and
	// from every role list on a role-less appliance. The ownership and
	// verification records are the only remaining enumeration source; without
	// them the watcher stops seeing the port after the first successful bind
	// (no inventory persistence, no re-attach, no health readback).
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, device := range state.Devices {
			if takeoverModeFromDriver(device.Transport) == "" {
				continue
			}
			appendName(device.LinuxInterface)
		}
	}
	if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
		for _, entry := range state.Interfaces {
			if isDPDKVerificationMode(entry.Mode) {
				appendName(entry.LinuxInterface)
			}
		}
	}
	sort.Strings(names)
	return names
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// rdmaPluginAvailable reports whether the VPP RDMA plugin is shipped and
// enabled. The verified RDMA rule is only used when the plugin can actually
// create the interface.
func rdmaPluginAvailable() bool {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_VPP_RDMA_PLUGIN")); value != "" {
		return strings.EqualFold(value, "true") || value == "1"
	}
	directories := []string{
		strings.TrimSpace(os.Getenv("LY_ROUTE_VPP_PLUGIN_DIR")),
		"/usr/lib/x86_64-linux-gnu/vpp_plugins",
		"/usr/lib/vpp_plugins",
		"/usr/lib64/vpp_plugins",
	}
	for _, directory := range directories {
		if directory == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(directory, "rdma_plugin.so")); err == nil {
			return true
		}
	}
	return false
}

// dpdkPluginEnabled reports whether the managed startup configuration enables
// the DPDK plugin. A missing/different config means DPDK cannot attach.
func dpdkPluginEnabled() bool {
	startup := envOrDefault("LY_ROUTE_VPP_STARTUP_CONFIG", "/etc/vpp/startup.conf")
	data, err := os.ReadFile(startup)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.Contains(text, "dpdk_plugin.so { enable }") && !strings.Contains(text, "dpdk_plugin.so { disable }")
}

// vfioPolicyAllowed is the internal transport policy. UIO is the default
// transport; VFIO is only allowed when policy does not force UIO.
func vfioPolicyAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_TRANSPORT"))) {
	case "uio":
		return false
	case "vfio":
		return true
	default:
		return true
	}
}

// portRecordedPCI recovers the PCI address of a port whose kernel netdev no
// longer exists because the device is bound to uio/vfio. The ownership record
// is the product's own statement of which device it took over; the operator
// inventory is the fallback.
func portRecordedPCI(name string) string {
	if pci := inventoryPCI(name); pci != "" {
		return pci
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, device := range state.Devices {
			if strings.TrimSpace(device.LinuxInterface) == name && strings.TrimSpace(device.PCIAddress) != "" {
				return strings.TrimSpace(device.PCIAddress)
			}
		}
	}
	if inventory, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
		for _, item := range inventory.DataInterfaces {
			if strings.TrimSpace(item.LinuxInterface) == name && strings.TrimSpace(item.PCIAddress) != "" {
				return strings.TrimSpace(item.PCIAddress)
			}
		}
	}
	return ""
}

// takeoverFactsForPort reads the observed facts for one netdev.
func (server *Server) takeoverFactsForPort(ctx context.Context, name string) takeover.Facts {
	name = strings.TrimSpace(name)
	facts := takeover.Facts{Interface: name}
	device := filepath.Join(sysfsRoot(), "class", "net", name, "device")
	physical := false
	if _, err := os.Stat(device); err == nil {
		physical = true
	} else if pci := portRecordedPCI(name); pci != "" {
		// A DPDK-bound device has no netdev; its PCI device is the only proof
		// that this is still a physical data port.
		if _, statErr := os.Stat(filepath.Join(sysfsRoot(), "bus", "pci", "devices", pci)); statErr == nil {
			physical = true
		}
	}
	if !physical {
		return facts
	}
	facts.Physical = true
	pci := portRecordedPCI(name)
	if driver, err := filepath.EvalSymlinks(filepath.Join(device, "driver")); err == nil {
		facts.Driver = filepath.Base(driver)
	}
	if facts.Driver == "" || takeoverModeDestructive(takeoverModeFromDriver(facts.Driver)) {
		// A port bound to uio_pci_generic/vfio-pci is not evidence about the
		// NIC family. The rule library matches on the original kernel driver
		// recorded by the ownership preflight, otherwise a DPDK port would
		// look like unknown hardware and lose its rule.
		facts.Driver = firstNonEmptyTrimmed(recordedKernelDriver(pci), facts.Driver)
	}
	facts.VendorDevice = takeover.ParseDeviceID(
		readTrimmedFile(filepath.Join(device, "vendor")),
		readTrimmedFile(filepath.Join(device, "device")),
	)
	management := strings.TrimSpace(server.managementInterfaceID(ctx))
	facts.Management = management != "" && name == management
	facts.SharedManagement = facts.Management && server.managementNetworkShared(ctx)

	facts.RDMAReady = rdmaPluginAvailable()
	facts.UIOAvailable = transportDriverPresent("uio_pci_generic")
	facts.VFIOAvailable = transportDriverPresent("vfio-pci")
	facts.IOMMUIsolated = pciHasIOMMUGroup(pci)
	facts.VFIOPolicyAllowed = vfioPolicyAllowed()
	facts.DPDKPlugin = dpdkPluginEnabled()
	_, _, facts.Hugepages = hugepagesShortfall()
	return facts
}

// transportDriverPresent reports whether the kernel exposes the DPDK binding
// driver. A missing driver directory means the transport cannot be used.
func transportDriverPresent(driver string) bool {
	_, err := os.Stat(filepath.Join(sysfsRoot(), "bus", "pci", "drivers", driver))
	return err == nil
}

// takeoverPlanForPort is the single decision entry point used by the API, the
// status contract and the automatic adoption paths.
func (server *Server) takeoverPlanForPort(ctx context.Context, name string) takeover.Plan {
	return takeover.BuildPlan(server.takeoverFactsForPort(ctx, name))
}

// automaticTakeoverTarget returns the first candidate the engine should try for
// a port. It is backend-owned and idempotent.
func (server *Server) automaticTakeoverTarget(ctx context.Context, name string) string {
	plan := server.takeoverPlanForPort(ctx, name)
	primary, ok := plan.Primary()
	if !ok {
		return ""
	}
	return string(primary.Mode)
}

// autoTakeoverPending reports whether any physical data port is still expected
// to be upgraded to the DPDK path by the backend plan. The apply uses it to run
// the ownership preflight on a healthy appliance instead of waiting for a
// degraded plan.
func (server *Server) autoTakeoverPending(ctx context.Context) bool {
	capabilities := loadTakeoverCapabilitiesQuietly()
	vppNames := server.vppInterfaceNames(ctx)
	for _, name := range server.takeoverDataInterfaces(ctx) {
		plan := server.takeoverPlanForPort(ctx, name)
		primary, ok := plan.Primary()
		if !ok || !takeoverModeDestructive(string(primary.Mode)) {
			continue
		}
		if mode, recorded := activeDataplaneMode(name); recorded && mode == string(primary.Mode) {
			continue
		}
		// An existing live attachment (or a recorded ownership decision) means
		// this port has already been taken over. An apply reconciles it and must
		// not start a destructive first-bind just because the plan's preferred
		// mode differs. This is what made an empty/old configuration apply
		// replay an AF_XDP switch on E37.
		//
		// The single exception is the rule-preferred migration (WS-AK): an
		// AF_PACKET port whose rule selects DPDK still owes one attempt, and
		// the apply is the documented trigger for it.
		if snapshot := server.detectTakeoverSnapshot(ctx, name, vppNames); snapshot.Owned {
			if _, pending := server.rulePreferredMigrationCandidate(ctx, name, snapshot.CurrentMode, capabilities); pending {
				return true
			}
			continue
		}
		if server.takeoverGuardActive(capabilities, strings.TrimSpace(inventoryDriver(name)), inventoryPCI(name), name) {
			continue
		}
		return true
	}
	return false
}
