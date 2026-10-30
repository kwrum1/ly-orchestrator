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
	"sync"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
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
	path := dataplaneInventoryPath()
	previous, err := dataplane.LoadInventory(path)
	if err != nil {
		return err
	}
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
	// Deleting a WAN line only unbinds the port. Every data port this host still
	// exposes must stay in the operator inventory, otherwise re-creating the
	// line on the same NIC fails with 422 ("not present in this device's port
	// inventory") and there is no self-heal path. Ports whose hardware really
	// disappeared are dropped below because their identity cannot be recovered.
	for _, previousInterface := range previous.DataInterfaces {
		name := strings.TrimSpace(previousInterface.LinuxInterface)
		if name == "" || seen[name] || strings.EqualFold(name, inventory.Management.LinuxInterface) {
			continue
		}
		item, ok := readInventoryInterface(name)
		if !ok {
			// The netdev can be owned by VPP; accept the remembered identity
			// only while the PCI device still exists on this host.
			item = previousInterface
			pci := strings.TrimSpace(item.PCIAddress)
			if pci == "" {
				continue
			}
			if _, statErr := os.Stat(filepath.Join(sysfsRoot(), "bus", "pci", "devices", pci)); statErr != nil {
				continue
			}
		}
		seen[name] = true
		item.LinuxInterface = name
		item.Role = "data"
		inventory.DataInterfaces = append(inventory.DataInterfaces, item)
	}
	sort.Slice(inventory.DataInterfaces, func(left, right int) bool {
		return inventory.DataInterfaces[left].LinuxInterface < inventory.DataInterfaces[right].LinuxInterface
	})
	if len(inventory.DataInterfaces) == 0 && inventory.Management.PCIAddress == "" {
		return nil
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

// dataplaneBootstrapMu serializes the on-demand ownership preflight between the
// apply handler and the auto-adoption watcher. Two concurrent binds of the same
// PCI device would corrupt both the kernel driver state and the claim file.
var dataplaneBootstrapMu sync.Mutex

// takeoverCapabilityMu serializes read-modify-write of the crash guard.
var takeoverCapabilityMu sync.Mutex

// A failed destructive bootstrap must not be retried on every watcher tick or
// auto-apply. The crash guard handles the permanent cases; this bounded
// cooldown is the last line of defence against a restart storm while the
// operator inspects the device.
const dataplaneBootstrapFailureCooldown = 2 * time.Minute

var dataplaneBootstrapBlockedUntil time.Time

func blockDataplaneBootstrap(reason string) {
	dataplaneBootstrapBlockedUntil = time.Now().Add(dataplaneBootstrapFailureCooldown)
	log.Printf("dataplane ownership bootstrap cooldown: %s; retry after %s", reason, dataplaneBootstrapBlockedUntil.Format(time.RFC3339))
}

func clearDataplaneBootstrapCooldown() {
	dataplaneBootstrapBlockedUntil = time.Time{}
}

type dpdkTakeoverFailure struct {
	LinuxInterface string
	PCIAddress     string
	// Mode is the destructive transport that was attempted, so the journal can
	// name the requested mode even when verification failed.
	Mode   string
	Reason string
	// Crashed marks a verification failure where VPP stopped responding
	// (connection refused, restart or an admin-up that aborted the process).
	// Such a failure must persist the native crash guard, not merely release the
	// port and retry the same destructive path on the next apply.
	Crashed bool
}

// boundTakeoverCandidates lists the ports that are currently owned by a
// destructive PCI binding (VFIO/UIO). These are the only ports whose
// verification can crash VPP, so a VPP restart during verification must guard
// exactly this set.
func (server *Server) boundTakeoverCandidates(interfaces []string, inventory map[string]dataplane.InventoryInterface) []dpdkTakeoverFailure {
	candidates := []dpdkTakeoverFailure{}
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		item, ok := inventory[name]
		if !ok || strings.TrimSpace(item.PCIAddress) == "" {
			continue
		}
		pci := strings.TrimSpace(item.PCIAddress)
		driver, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot(), "bus/pci/devices", pci, "driver"))
		if err != nil {
			continue
		}
		current := filepath.Base(driver)
		if current != "vfio-pci" && current != "uio_pci_generic" {
			continue
		}
		candidates = append(candidates, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current)})
	}
	return candidates
}

// vppDownSignature reports whether a CLI error means the VPP socket/service is
// gone rather than a rejected command. Keep this narrow: a syntax error from a
// live VPP must not permanently guard a device.
func vppDownSignature(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"connection refused",
		"connect: no such file or directory",
		"failed to connect",
		"connection reset by peer",
		"broken pipe",
		"connection timed out",
		"vpp is not running",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func vppUnavailableFromError(err error, output []byte) bool {
	if err == nil {
		return false
	}
	return vppDownSignature(err.Error() + " " + string(output))
}

// vppReachable performs one bounded, read-only CLI probe. A dead VPP must be
// detected before an apply issues a batch of commands against it.
func (server *Server) vppReachable(ctx context.Context) bool {
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(probeCtx, vppctl, "show", "version").Run() == nil
}

// vppServiceManageable reports whether this host actually runs VPP under
// systemd. Unit tests and non-appliance builds do not, and must never trigger a
// service restart from an apply.
func (server *Server) vppServiceManageable(ctx context.Context) bool {
	if strings.TrimSpace(os.Getenv("LY_ROUTE_VPP_RESTART_COMMAND")) != "" {
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "systemctl", "show", "vpp.service", "--property=LoadState", "--value").Output()
	return err == nil && strings.TrimSpace(string(output)) == "loaded"
}

// crashGuardReason reports whether a verification failure is evidence that the
// native candidate crashed VPP. The admin-up path is the one that aborts
// vmxnet3/UIO on this hardware, and a connection-refused readback is the
// observable aftermath.
func crashGuardReason(reason string) bool {
	lower := strings.ToLower(strings.TrimSpace(reason))
	if lower == "" {
		return false
	}
	if vppDownSignature(lower) {
		return true
	}
	for _, marker := range []string{
		"set interface state up failed",
		"vpp interface readback failed",
		"vpp hardware readback failed",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// recordTakeoverCrashGuard persists native_unsupported for every driver+PCI
// pair whose verification crashed VPP. It is written immediately, before the
// apply transaction is created or rolled back, so it survives a failed apply
// and the next attempt skips the destructive path.
func (server *Server) recordTakeoverCrashGuard(failures []dpdkTakeoverFailure) bool {
	if len(failures) == 0 {
		return false
	}
	takeoverCapabilityMu.Lock()
	defer takeoverCapabilityMu.Unlock()
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	recorded := false
	for _, failure := range failures {
		pci := strings.TrimSpace(failure.PCIAddress)
		if pci == "" || (!failure.Crashed && !crashGuardReason(failure.Reason)) {
			continue
		}
		driver := ""
		if item, ok := readInventoryInterface(failure.LinuxInterface); ok {
			driver = strings.TrimSpace(item.KernelDriver)
		}
		if driver == "" {
			if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
				for _, device := range state.Devices {
					if strings.TrimSpace(device.PCIAddress) == pci {
						driver = strings.TrimSpace(device.KernelDriver)
						break
					}
				}
			}
		}
		if driver == "" {
			// The ownership record may already have been removed by a release
			// that raced this call; the persisted inventory still names the
			// original kernel driver.
			if inventory, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
				for _, item := range inventory.DataInterfaces {
					if strings.TrimSpace(item.LinuxInterface) == failure.LinuxInterface || strings.TrimSpace(item.PCIAddress) == pci {
						driver = strings.TrimSpace(item.KernelDriver)
						break
					}
				}
			}
		}
		environment := server.capabilityEnvironmentFor(pci, driver)
		capabilities.MarkUnsupportedWithEnvironment(driver, pci, failure.Reason, environment, takeoverGuardTTL())
		recorded = true
		log.Printf("dataplane takeover crash guard: %s (%s, driver %s): %s", failure.LinuxInterface, pci, driver, failure.Reason)
	}
	if recorded {
		if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
			log.Printf("persist takeover crash guard: %v", err)
			return false
		}
	}
	return recorded
}

// dataplaneRecoveryExpected reports whether a locked/degraded plan can still be
// repaired by waiting: a port is destructively owned, already has a takeover
// proof, or carries a crash guard. Such a plan must use the bounded retry
// window instead of rejecting the operator's first apply immediately.
func (server *Server) dataplaneRecoveryExpected(ctx context.Context) bool {
	interfaces := server.runtimeDataInterfaces(ctx)
	if len(interfaces) == 0 {
		return false
	}
	if !server.vppReachable(ctx) {
		return true
	}
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	ownership := map[string]bool{}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, device := range state.Devices {
			ownership[strings.TrimSpace(device.LinuxInterface)] = true
		}
	}
	verification := map[string]bool{}
	if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
		for _, entry := range state.Interfaces {
			verification[strings.TrimSpace(entry.LinuxInterface)] = true
		}
	}
	for _, name := range interfaces {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if ownership[name] || verification[name] {
			return true
		}
		if _, ok := activeDataplaneMode(name); ok {
			return true
		}
		if item, ok := readInventoryInterface(name); ok {
			if server.takeoverGuardActive(capabilities, strings.TrimSpace(item.KernelDriver), strings.TrimSpace(item.PCIAddress), name) {
				return true
			}
		}
	}
	return false
}

// suppressCrashGuardedCandidates removes DPDK candidates for driver+PCI pairs
// that already crashed VPP. A stale capability report must never send the next
// apply back into the same crash; selection then falls through to AF_XDP or the
// consented AF_PACKET fallback.
func (server *Server) suppressCrashGuardedCandidates(nativePath *vpp.NativePathRequest) {
	if nativePath == nil {
		return
	}
	capabilities, err := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	if err != nil {
		return
	}
	for index := range nativePath.Assignments {
		assignment := &nativePath.Assignments[index]
		if len(assignment.Candidates) > 0 {
			kept := make([]vpp.CapabilityProof, 0, len(assignment.Candidates))
			for _, candidate := range assignment.Candidates {
				if candidate.Tier != vpp.DataplaneTierDPDK && candidate.Hook != vpp.NativeHookDPDK {
					kept = append(kept, candidate)
					continue
				}
				if proof, ok := server.validatedDPDKOwnershipProof(assignment.LinuxInterface, candidate.PCIAddress); ok {
					log.Printf("dataplane selection: trusting the %s proof for %s (%s, mode=%s); the device is already bound, so no probe or crash-guard skip applies", proof.Source, assignment.LinuxInterface, candidate.PCIAddress, proof.Mode)
					server.markTakeoverGuardSupported(candidate.PCIAddress, candidate.KernelDriver, "trusted "+proof.Source+" proof")
					kept = append(kept, candidate)
					continue
				}
				if server.takeoverGuardActive(capabilities, candidate.KernelDriver, candidate.PCIAddress, assignment.LinuxInterface) {
					continue
				}
				kept = append(kept, candidate)
			}
			assignment.Candidates = kept
		}
		proof := assignment.Proof
		if proof.Tier == vpp.DataplaneTierDPDK || proof.Hook == vpp.NativeHookDPDK {
			if ownership, ok := server.validatedDPDKOwnershipProof(assignment.LinuxInterface, proof.PCIAddress); ok {
				log.Printf("dataplane selection: keeping the DPDK proof for %s (%s) from trusted %s ownership", assignment.LinuxInterface, proof.PCIAddress, ownership.Source)
				server.markTakeoverGuardSupported(proof.PCIAddress, proof.KernelDriver, "trusted "+ownership.Source+" proof")
			} else if server.takeoverGuardActive(capabilities, proof.KernelDriver, proof.PCIAddress, assignment.LinuxInterface) {
				assignment.Proof = vpp.CapabilityProof{}
			}
		}
	}
}

// verifyDPDKTakeovers checks the destructive candidates after VPP has started.
// A takeover is only verified when the VPP interface for the PCI exists and its
// show-interface state is up. The hardware model is deliberately not a gate:
// any NIC that VPP attaches and brings up is a valid native path.
//
// Verification is also the crash detector. On vmxnet3/UIO the admin-up aborts
// VPP with SIGSEGV; every following vppctl call returns "connect: Connection
// refused". That must become a crash-guard event for the whole bound candidate
// set, not a reason to keep issuing commands against a dead socket.
func (server *Server) verifyDPDKTakeovers(ctx context.Context, interfaces []string) ([]dataplane.DPDKVerification, []dpdkTakeoverFailure) {
	inventory, err := dataplane.LoadInventory(dataplaneInventoryPath())
	if err != nil {
		return nil, []dpdkTakeoverFailure{{Reason: "dataplane inventory is unreadable: " + err.Error()}}
	}
	byInterface := map[string]dataplane.InventoryInterface{}
	for _, item := range inventory.DataInterfaces {
		byInterface[strings.TrimSpace(item.LinuxInterface)] = item
	}
	candidates := server.boundTakeoverCandidates(interfaces, byInterface)
	verified := []dataplane.DPDKVerification{}
	failures := []dpdkTakeoverFailure{}
	// failAll is the conservative fallback used only when VPP is already dead
	// before any per-device command: the crashing device cannot be identified,
	// so every bound candidate is guarded and the reason says the scope is
	// uncertain.
	failAll := func(reason string) ([]dataplane.DPDKVerification, []dpdkTakeoverFailure) {
		failures := make([]dpdkTakeoverFailure, 0, len(candidates))
		for _, candidate := range candidates {
			candidate.Reason = reason + " (guard scope uncertain: the crashing device could not be identified)"
			candidate.Crashed = true
			failures = append(failures, candidate)
		}
		return nil, failures
	}
	// failCurrent guards only the candidate whose command killed VPP. Other
	// candidates are reported as retryable failures: a healthy port must not be
	// banned because its neighbour crashed the PMD.
	failCurrent := func(current dpdkTakeoverFailure, reason string) ([]dataplane.DPDKVerification, []dpdkTakeoverFailure) {
		current.Reason = reason
		current.Crashed = true
		alreadyFailed := map[string]bool{}
		for _, failure := range failures {
			alreadyFailed[strings.TrimSpace(failure.LinuxInterface)] = true
		}
		verifiedNames := map[string]bool{}
		for _, entry := range verified {
			verifiedNames[strings.TrimSpace(entry.LinuxInterface)] = true
		}
		for _, candidate := range candidates {
			name := strings.TrimSpace(candidate.LinuxInterface)
			if name == "" || name == current.LinuxInterface || alreadyFailed[name] || verifiedNames[name] {
				// A port that already verified must not be released because a
				// neighbour crashed; only the unverified remainder is retryable.
				continue
			}
			candidate.Reason = "not verified: VPP stopped responding while verifying " + current.LinuxInterface
			candidate.Crashed = false
			failures = append(failures, candidate)
		}
		failures = append(failures, current)
		return verified, failures
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	if !server.vppReachable(ctx) {
		reason := "VPP is not reachable during attach verification: connect: Connection refused (the candidate likely crashed VPP)"
		log.Printf("dataplane takeover verification: %s", reason)
		return failAll(reason)
	}
	hardwareOutput, hardwareErr := exec.CommandContext(ctx, vppctl, "show", "hardware-interfaces").CombinedOutput()
	if hardwareErr != nil {
		if vppUnavailableFromError(hardwareErr, hardwareOutput) {
			reason := "vppctl show hardware-interfaces failed during attach verification: " + hardwareErr.Error() + ": " + strings.TrimSpace(string(hardwareOutput))
			log.Printf("dataplane takeover verification: %s", reason)
			return failAll(reason)
		}
		return nil, []dpdkTakeoverFailure{{Reason: "vppctl show hardware-interfaces failed: " + hardwareErr.Error()}}
	}
	// LY_ROUTE_DPDK_FUNCTIONAL_PEER is intentionally ignored: a functional
	// proof must not use `vppctl ping` (nat44-ed-out2in SIGSEGV on this image).
	// VPP carrier plus a live interface state is the non-destructive proof.
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
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current), Reason: "VPP did not create an interface for the PCI device"})
			continue
		}
		// Admin-up is part of the verification. On some PMDs (vmxnet3 in this
		// environment) this can abort VPP; that is a failed native candidate,
		// not a reason to trust a stale status string.
		upCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		upOutput, upErr := exec.CommandContext(upCtx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
		cancel()
		if upErr != nil {
			reason := "set interface state up failed: " + upErr.Error() + ": " + strings.TrimSpace(string(upOutput))
			if vppUnavailableFromError(upErr, upOutput) || vppDownSignature(reason) {
				reason = "VPP stopped responding after set interface state up: " + reason
				log.Printf("dataplane takeover verification: %s", reason)
				return failCurrent(dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current)}, reason)
			}
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current), Reason: reason})
			continue
		}
		readCtx, cancelRead := context.WithTimeout(ctx, 10*time.Second)
		interfaceOutput, interfaceErr := exec.CommandContext(readCtx, vppctl, "show", "interface", vppInterface).CombinedOutput()
		cancelRead()
		if interfaceErr != nil {
			reason := "VPP interface readback failed: " + interfaceErr.Error() + ": " + strings.TrimSpace(string(interfaceOutput))
			if vppUnavailableFromError(interfaceErr, interfaceOutput) || vppDownSignature(reason) {
				reason = "VPP stopped responding during interface readback: " + reason
				log.Printf("dataplane takeover verification: %s", reason)
				return failCurrent(dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current)}, reason)
			}
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current), Reason: reason})
			continue
		}
		hardwareInterface, hardwareInterfaceErr := exec.CommandContext(ctx, vppctl, "show", "hardware-interfaces", vppInterface).CombinedOutput()
		if hardwareInterfaceErr != nil {
			reason := "VPP hardware readback failed: " + hardwareInterfaceErr.Error() + ": " + strings.TrimSpace(string(hardwareInterface))
			if vppUnavailableFromError(hardwareInterfaceErr, hardwareInterface) || vppDownSignature(reason) {
				reason = "VPP stopped responding during hardware readback: " + reason
				log.Printf("dataplane takeover verification: %s", reason)
				return failCurrent(dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current)}, reason)
			}
			failures = append(failures, dpdkTakeoverFailure{LinuxInterface: name, PCIAddress: pci, Mode: takeoverModeFromDriver(current), Reason: reason})
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
		} else if linkSpeed > 0 && hardwareLink == "up" {
			// The PHY has carrier but the interface is admin-down. Never use
			// `vppctl ping` here: it SIGSEGVs nat44_ed_out2in with a
			// self-referential session. The watcher sets the interface up and
			// promotes the proof on the next tick.
			verification.Verification = "pending_link"
			verification.Reason = "VPP interface is admin-down while the PHY has carrier; the watcher brings it up without ping"
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
	released := false
	for _, ok := range server.releaseFailedDPDKTakeoversDetailed(ctx, failures) {
		if ok {
			released = true
		}
	}
	return released
}

// releaseFailedDPDKTakeoversDetailed returns the per-interface release outcome
// so the takeover journal records what actually happened instead of a blanket
// "restored". Context cancellation must not kill systemctl or the release
// helper: both are lifecycle operations.
func (server *Server) releaseFailedDPDKTakeoversDetailed(ctx context.Context, failures []dpdkTakeoverFailure) map[string]bool {
	outcome := map[string]bool{}
	if len(failures) == 0 {
		return outcome
	}
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
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
		_ = exec.CommandContext(lifecycleCtx, "/bin/sh", "-c", stop).Run()
	}
	for _, failure := range failures {
		name := strings.TrimSpace(failure.LinuxInterface)
		if strings.TrimSpace(failure.PCIAddress) == "" {
			outcome[name] = false
			log.Printf("dataplane takeover rejected: %s: %s", failure.LinuxInterface, failure.Reason)
			continue
		}
		output, err := exec.CommandContext(lifecycleCtx, command, failure.PCIAddress).CombinedOutput()
		if err != nil {
			outcome[name] = false
			log.Printf("dataplane takeover release failed for %s (%s): %v: %s", failure.LinuxInterface, failure.PCIAddress, err, strings.TrimSpace(string(output)))
			continue
		}
		outcome[name] = true
		log.Printf("dataplane takeover rejected: %s (%s): %s; restored kernel driver", failure.LinuxInterface, failure.PCIAddress, failure.Reason)
		dropDPDKVerification(failure.LinuxInterface)
	}
	return outcome
}

func failurePCIAddresses(failures []dpdkTakeoverFailure) []string {
	addresses := make([]string, 0, len(failures))
	for _, failure := range failures {
		if pci := strings.TrimSpace(failure.PCIAddress); pci != "" {
			addresses = append(addresses, pci)
		}
	}
	return addresses
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
	// A cancelled request must not kill systemctl mid-restart. The command runs
	// on a lifecycle context; only the readiness wait consults the caller.
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
	restart := envOrDefault("LY_ROUTE_VPP_RESTART_COMMAND", "systemctl restart vpp.service")
	if output, err := exec.CommandContext(lifecycleCtx, "/bin/sh", "-c", restart).CombinedOutput(); err != nil {
		log.Printf("dataplane ownership VPP restart failed: %v: %s", err, strings.TrimSpace(string(output)))
		return false
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := exec.CommandContext(lifecycleCtx, vppctl, "show", "version").Run(); err == nil {
			return true
		}
		if lifecycleCtx.Err() != nil || time.Now().After(deadline) {
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
	dataplaneBootstrapMu.Lock()
	defer dataplaneBootstrapMu.Unlock()
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_BOOTSTRAP")), "false") {
		return false
	}
	if !dataplaneBootstrapBlockedUntil.IsZero() && time.Now().Before(dataplaneBootstrapBlockedUntil) {
		log.Printf("dataplane ownership bootstrap skipped: previous destructive attempt failed; cooldown until %s", dataplaneBootstrapBlockedUntil.Format(time.RFC3339))
		return false
	}
	command := envOrDefault("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", "/usr/lib/ly-route/prepare-vfio.sh")
	info, err := os.Stat(command)
	if err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	interfaces := server.takeoverDataInterfaces(ctx)
	if len(interfaces) == 0 {
		return false
	}
	vppNames := server.vppInterfaceNames(ctx)
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	// Only a port still on its kernel driver can be tested for a native
	// takeover. A port already bound to VFIO/UIO is trusted when a device-level
	// ownership proof exists; starting a candidate VPP for it would contend with
	// the running instance that already holds the PCI device. VPP cannot share
	// a device, so there is nothing useful to re-probe.
	//
	// The target mode is decided by the backend rule library, never by an
	// operator: every physical port is taken over automatically and the
	// management port is excluded by the plan.
	kernelPorts := []string{}
	trustedPorts := []string{}
	switched := false
	for _, name := range interfaces {
		item, _ := readInventoryInterface(name)
		pci := strings.TrimSpace(item.PCIAddress)
		plan := server.takeoverPlanForPort(ctx, name)
		if plan.Skipped != "" {
			continue
		}
		primary, planned := plan.Primary()
		if !planned {
			continue
		}
		if snapshot := server.detectTakeoverSnapshot(ctx, name, vppNames); snapshot.Owned {
			target, upgrade := server.automaticTakeoverUpgrade(ctx, name, snapshot.CurrentMode, capabilities)
			if !upgrade {
				// The port already carries the mode the backend plan selects (or
				// the plan has no destructive step). Record the deliberate keep
				// decision so the journal still explains why nothing changed.
				if err := recordTakeoverTransition(takeoverKeptTransition(snapshot, serverNow(server))); err != nil {
					log.Printf("record takeover decision for %s: %v", name, err)
				}
				continue
			}
			if retryAfter, blocked := dataplaneSwitchRetryAfter[name]; blocked && time.Now().Before(retryAfter) {
				continue
			}
			if !claimAutomaticModeSwitch(name) {
				// At most one automatic switch per boot per port; a failed
				// attempt must not be replayed by a later apply.
				continue
			}
			// An automatic upgrade restarts VPP even when it later fails and
			// rolls back, so the apply must wait for readiness afterwards.
			switched = true
			if err := server.applyTakeoverSwitch(ctx, snapshot, target, true); err != nil {
				log.Printf("dataplane takeover upgrade for %s (%s -> %s) failed: %v", name, takeoverModeLabel(snapshot.CurrentMode), takeoverModeLabel(target), err)
				dataplaneSwitchRetryAfter[name] = time.Now().Add(switchFailureCooldown(err))
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_upgrade", "failed", "target="+target, "")
			} else {
				delete(dataplaneSwitchRetryAfter, name)
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_upgrade", "success", "target="+target, "")
			}
			continue
		}
		if !dataInterfaceOnKernelDriver(name) {
			if proof, ok := server.validatedDPDKOwnershipProof(name, pci); ok {
				trustedPorts = append(trustedPorts, name)
				log.Printf("dataplane takeover skipped for %s (%s): trusted %s ownership; the device is bound to %s and the running VPP keeps it (no candidate probe)", name, pci, proof.Source, pciBoundDriver(pci))
				server.markTakeoverGuardSupported(pci, item.KernelDriver, "trusted "+proof.Source+" ownership")
			}
			continue
		}
		if !takeoverModeDestructive(string(primary.Mode)) {
			// The plan does not start with DPDK (paravirtual NIC, no PMD or a
			// missing prerequisite). Nothing to bind; the native path in the
			// apply creates the attachment on the kernel netdev.
			continue
		}
		kernelPorts = append(kernelPorts, name)
	}
	if len(kernelPorts) == 0 {
		if len(trustedPorts) > 0 {
			log.Printf("dataplane ownership bootstrap: %d port(s) already have a trusted DPDK ownership proof; VPP is not restarted", len(trustedPorts))
		}
		return switched
	}
	if err := server.writeDataplaneInventory(ctx, interfaces); err != nil {
		log.Printf("dataplane ownership bootstrap inventory: %v", err)
		return false
	}
	skips := []string{}
	eligible := []string{}
	for _, name := range kernelPorts {
		item, ok := readInventoryInterface(name)
		if !ok || strings.TrimSpace(item.PCIAddress) == "" {
			continue
		}
		if nonDestructiveTakeoverActive(name) {
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			log.Printf("dataplane takeover skipped for %s (%s): non-destructive takeover keeps the kernel netdev", name, item.PCIAddress)
			continue
		}
		if afXDPOutranksDestructive(name) {
			// The rule library only promotes a verified AF_XDP zero-copy path
			// above DPDK. Never destroy a netdev that already has a faster,
			// non-destructive, link-independent path.
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			log.Printf("dataplane takeover skipped for %s (%s): AF_XDP zero-copy is proven and preferred over destructive DPDK", name, item.PCIAddress)
			continue
		}
		if server.takeoverGuardActive(capabilities, strings.TrimSpace(item.KernelDriver), strings.TrimSpace(item.PCIAddress), name) {
			skips = append(skips, strings.TrimSpace(item.PCIAddress))
			continue
		}
		eligible = append(eligible, name)
	}
	if len(eligible) == 0 {
		// Every candidate is crash-guarded or already owned through a
		// non-destructive path. Do not restart VPP just to re-prove that.
		log.Printf("dataplane ownership bootstrap: all %d data port(s) are crash-guarded or non-destructive; Linux keeps the kernel drivers", len(kernelPorts))
		return false
	}
	if !server.dpdkProbeAllowedByHugepages(eligible) {
		return false
	}
	bootstrap := exec.CommandContext(ctx, command)
	bootstrap.Env = append(os.Environ(), "LY_ROUTE_VFIO_BOOTSTRAP=apply", "LY_ROUTE_DPDK_SKIP_PCIS="+strings.Join(skips, " "))
	if output, err := bootstrap.CombinedOutput(); err != nil {
		log.Printf("dataplane ownership bootstrap failed: %v: %s", err, strings.TrimSpace(string(output)))
		blockDataplaneBootstrap("prepare-vfio failed")
		return false
	}
	// prepare-vfio exits successfully when the port is not part of any data
	// mapping (a freshly installed appliance whose LAN/WAN roles are still
	// unset has an inventory with no data interfaces). That is not a VPP
	// restart and it is not a successful DPDK takeover: record the failed
	// rule-preferred attempt so the watcher adopts AF_PACKET instead of
	// leaving the port without any dataplane.
	unbound := []string{}
	boundCount := 0
	for _, name := range eligible {
		item, ok := readInventoryInterface(name)
		if !ok {
			continue
		}
		if takeoverModeDestructive(takeoverModeFromDriver(pciBoundDriver(item.PCIAddress))) {
			boundCount++
			continue
		}
		primary, _ := server.takeoverPlanForPort(ctx, name).Primary()
		recordDPDKAttemptFailed(name, item.PCIAddress, string(primary.Mode), "the ownership preflight did not bind the device; the port is not part of the data interface mapping")
		unbound = append(unbound, name)
	}
	if boundCount == 0 {
		log.Printf("dataplane ownership bootstrap: prepare-vfio bound no candidate (%s); the rule-preferred DPDK attempt is recorded as failed", strings.Join(unbound, ","))
		return false
	}
	if !server.restartVPPAndWait(ctx) {
		// The candidate VPP start failed. That is still a crash-guard event for
		// the devices we just bound: persist the guard, return every candidate
		// port to its kernel driver, then let the AF fallback be probed.
		failures := make([]dpdkTakeoverFailure, 0, len(eligible))
		for _, name := range eligible {
			if item, ok := readInventoryInterface(name); ok {
				failures = append(failures, dpdkTakeoverFailure{
					LinuxInterface: name,
					PCIAddress:     item.PCIAddress,
					Mode:           takeoverModeFromDriver(pciBoundDriver(item.PCIAddress)),
					Reason:         "VPP failed to start with the candidate DPDK device: connect: Connection refused",
					Crashed:        true,
				})
			}
		}
		server.recordTakeoverCrashGuard(failures)
		released := server.releaseFailedDPDKTakeoversDetailed(ctx, failures)
		server.recordTakeoverFirstBindFailures(failures, released)
		if server.restartVPPAndWait(ctx) {
			server.refreshTakeoverGuardEnvironments(failurePCIAddresses(failures))
		} else {
			// VPP must not be left down after releasing the candidates; if the
			// recovery start also fails, back off instead of restarting in a loop.
			blockDataplaneBootstrap("VPP did not start with the candidate DPDK device")
		}
		return true
	}
	verified, failures := server.verifyDPDKTakeovers(ctx, interfaces)
	if len(failures) > 0 {
		// Persist the guard before releasing/restarting so it survives even if
		// this apply later fails. Only crash-like failures are guarded; a
		// missing VPP interface is an ordinary candidate rejection.
		server.recordTakeoverCrashGuard(failures)
		released := server.releaseFailedDPDKTakeoversDetailed(ctx, failures)
		server.recordTakeoverFirstBindFailures(failures, released)
		// The release path stops VPP to unbind the device. Restart it even if a
		// release failed, otherwise a bad candidate would leave no dataplane.
		if !server.restartVPPAndWait(ctx) {
			blockDataplaneBootstrap("VPP did not recover after releasing a failed DPDK candidate")
			return false
		}
		server.refreshTakeoverGuardEnvironments(failurePCIAddresses(failures))
	}
	clearDataplaneBootstrapCooldown()
	if len(verified) > 0 {
		for _, entry := range verified {
			server.recordTakeoverProbeSuccess(entry.PCIAddress, recordedKernelDriver(entry.PCIAddress), "DPDK takeover verified")
			server.recordTakeoverFirstBindSuccess(ctx, entry)
		}
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
	allowed := func(mode string) bool {
		return nonDestructiveMode(mode) && takeover.ModeAllowed(portKernelDriver(name), portVendorDevice(name), takeover.Mode(mode))
	}
	if mode, ok := activeDataplaneMode(name); ok && allowed(mode) {
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
		if mode := takeoverModeFromDriver(entry.Mode); allowed(mode) {
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
	if !takeover.ModeAllowed(portKernelDriver(name), portVendorDevice(name), takeover.ModeAFXDPZeroCopy) {
		return false
	}
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
	if !takeover.ModeAllowed(portKernelDriver(name), portVendorDevice(name), takeover.ModeAFXDPZeroCopy) {
		return false
	}
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
