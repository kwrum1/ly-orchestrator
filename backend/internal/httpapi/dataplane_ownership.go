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
	"ly-route/backend/internal/runtime/vpp"
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

func takeoverCapabilityPath() string {
	if value := strings.TrimSpace(os.Getenv("LY_ROUTE_TAKEOVER_CAPABILITY_STATE")); value != "" {
		return value
	}
	stateDir := strings.TrimSpace(os.Getenv("LY_ROUTE_DATAPLANE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "/var/lib/ly-route/dataplane"
	}
	return filepath.Join(stateDir, "takeover-capabilities.json")
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
			Mode:           current,
			LinkState:      state,
			HardwareLink:   hardwareLink,
			LinkSpeedMbps:  linkSpeed,
			ObservedAt:     time.Now().UTC(),
		}
		if state == "up" {
			verification.FunctionalVerified = true
			verification.Verification = "verified"
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
				verification.Verification = "verified"
				verification.Method = "tx_counter_and_link_speed"
			} else {
				verification.Verification = "pending_link"
				verification.Reason = "VPP status is down and no TX counter increment was observed"
			}
		} else {
			// Link is down, but ownership must remain. This is an approved
			// pending_link takeover; the watcher promotes it when the link rises.
			verification.Verification = "pending_link"
			verification.Reason = "link is down; takeover retained and awaiting link"
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
	hasReleasable := false
	for _, failure := range failures {
		if strings.TrimSpace(failure.PCIAddress) != "" {
			hasReleasable = true
			break
		}
	}
	if hasReleasable {
		// VPP holds the PCI device; stop it before restoring the kernel driver.
		stop := envOrDefault("LY_ROUTE_VPP_STOP_COMMAND", "systemctl stop vpp.service")
		_ = exec.CommandContext(ctx, "/bin/sh", "-c", stop).Run()
	}
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
		dropDPDKVerification(failure.LinuxInterface)
	}
	return released
}

func dropDPDKVerification(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath())
	if err != nil {
		return
	}
	kept := state.Interfaces[:0]
	for _, entry := range state.Interfaces {
		if strings.TrimSpace(entry.LinuxInterface) == name {
			continue
		}
		kept = append(kept, entry)
	}
	state.Interfaces = kept
	if err := dataplane.SaveDPDKVerification(dataplaneVerificationPath(), state); err != nil {
		log.Printf("drop DPDK verification for %s: %v", name, err)
	}
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
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	skips := []string{}
	for _, name := range interfaces {
		item, ok := readInventoryInterface(name)
		if !ok {
			continue
		}
		if nonDestructiveTakeoverActive(name) {
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			log.Printf("dataplane takeover skipped for %s (%s): non-destructive takeover keeps the kernel netdev", name, item.PCIAddress)
			continue
		}
		if afXDPOutranksDestructive(name) {
			// The ordered policy ranks a verified AF_XDP zero-copy path above
			// the destructive no-IOMMU DPDK path. Never destroy a netdev that
			// already has a faster, non-destructive, link-independent path.
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			log.Printf("dataplane takeover skipped for %s (%s): AF_XDP zero-copy is proven and preferred over destructive DPDK", name, item.PCIAddress)
			continue
		}
		if capability, unsupported := capabilities.NativeUnsupported(strings.TrimSpace(item.KernelDriver), strings.TrimSpace(item.PCIAddress)); unsupported && capability.Reason != "" {
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			log.Printf("dataplane takeover skipped for %s (%s): native path marked unsupported: %s", name, item.PCIAddress, capability.Reason)
		}
	}
	bootstrap := exec.CommandContext(ctx, command)
	bootstrap.Env = append(os.Environ(), "LY_ROUTE_VFIO_BOOTSTRAP=apply", "LY_ROUTE_DPDK_SKIP_PCIS="+strings.Join(skips, " "))
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
		for _, failure := range failures {
			if !strings.Contains(failure.Reason, "set interface state up failed") && !strings.Contains(failure.Reason, "readback failed") {
				continue
			}
			driver := ""
			if item, ok := readInventoryInterface(failure.LinuxInterface); ok {
				driver = strings.TrimSpace(item.KernelDriver)
			}
			capabilities.MarkUnsupported(driver, failure.PCIAddress, failure.Reason)
		}
		if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
			log.Printf("persist takeover crash guard: %v", err)
		}
		server.releaseFailedDPDKTakeovers(ctx, failures)
		// The release path stops VPP to unbind the device. Restart it even if a
		// release failed, otherwise a bad candidate would leave no dataplane.
		if !server.restartVPPAndWait(ctx) {
			return false
		}
	}
	if len(verified) > 0 {
		if err := mergeDPDKVerification(dataplaneVerificationPath(), verified); err != nil {
			log.Printf("persist DPDK functional verification: %v", err)
		}
	}
	return true
}

// mergeDPDKVerification upserts verification facts without erasing proofs for
// ports that this probe did not inspect. An empty probe keeps the last valid
// proof instead of turning a running port into "unknown".
func mergeDPDKVerification(path string, verified []dataplane.DPDKVerification) error {
	state, err := dataplane.LoadDPDKVerification(path)
	if err != nil {
		return err
	}
	for _, entry := range verified {
		name := strings.TrimSpace(entry.LinuxInterface)
		if name == "" {
			continue
		}
		replaced := false
		for index := range state.Interfaces {
			if strings.TrimSpace(state.Interfaces[index].LinuxInterface) == name {
				state.Interfaces[index] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			state.Interfaces = append(state.Interfaces, entry)
		}
	}
	return dataplane.SaveDPDKVerification(path, state)
}

// nonDestructiveTakeoverActive reports whether the port is currently owned by a
// path that keeps the Linux netdev (AF_XDP, AF_PACKET, RDMA). Such a port must
// never be silently destroyed by a DPDK re-bind, especially not by the watcher.
func nonDestructiveTakeoverActive(name string) bool {
	name = strings.TrimSpace(name)
	if mode, ok := activeDataplaneMode(name); ok && nonDestructiveMode(mode) {
		return true
	}
	state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath())
	if err != nil {
		return false
	}
	for _, entry := range state.Interfaces {
		if strings.TrimSpace(entry.LinuxInterface) != name {
			continue
		}
		if nonDestructiveMode(takeoverModeFromDriver(entry.Mode)) {
			return true
		}
	}
	return false
}

// afXDPOutranksDestructive reports whether a verified AF_XDP zero-copy path
// should block a destructive VFIO/UIO bind. VFIO wins when it is available
// because it is IOMMU-protected and faster; AF_XDP copy and AF_PACKET never
// outrank UIO.
func afXDPOutranksDestructive(name string) bool {
	report, err := loadCapabilityReport(envOrDefault("LY_ROUTE_VPP_CAPABILITY_PROOF", "/var/lib/ly-route/vpp-native-capabilities.json"))
	if err != nil {
		return false
	}
	foundZeroCopy := false
	for _, proof := range report.Proofs {
		if strings.TrimSpace(proof.LinuxInterface) != strings.TrimSpace(name) {
			continue
		}
		candidates := proof.Candidates
		if len(candidates) == 0 {
			candidates = []vpp.CapabilityProof{proof.Proof}
		}
		for _, candidate := range candidates {
			if !candidate.RuntimeVerified {
				continue
			}
			if candidate.Tier == vpp.DataplaneTierDPDK && candidate.Mode == vpp.NativeModeDPDKVFIO {
				return false
			}
			if candidate.Hook == vpp.NativeHookAFXDP && candidate.Mode == vpp.NativeModeZeroCopy {
				foundZeroCopy = true
			}
		}
	}
	return foundZeroCopy
}

// afXDPZeroCopyProven reads the cached, non-destructive runtime probe. Only a
// verified zero-copy candidate outranks the destructive UIO path; an AF_XDP
// copy or AF_PACKET proof does not, because UIO is still faster.
func afXDPZeroCopyProven(name string) bool {
	report, err := loadCapabilityReport(envOrDefault("LY_ROUTE_VPP_CAPABILITY_PROOF", "/var/lib/ly-route/vpp-native-capabilities.json"))
	if err != nil {
		return false
	}
	for _, proof := range report.Proofs {
		if strings.TrimSpace(proof.LinuxInterface) != strings.TrimSpace(name) {
			continue
		}
		candidates := proof.Candidates
		if len(candidates) == 0 {
			candidates = []vpp.CapabilityProof{proof.Proof}
		}
		for _, candidate := range candidates {
			if !candidate.RuntimeVerified {
				continue
			}
			if candidate.Hook == vpp.NativeHookAFXDP && candidate.Mode == vpp.NativeModeZeroCopy {
				return true
			}
		}
	}
	return false
}

func nonDestructiveMode(mode string) bool {
	switch mode {
	case "af_packet", "af_xdp_zero_copy", "af_xdp_copy", "rdma":
		return true
	}
	return false
}
