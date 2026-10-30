package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

const (
	// minimumDPDKHugepages is the established 2 MiB hugepage baseline for the
	// production VPP configuration. A DPDK probe must not start a candidate VPP
	// with fewer pages: it would fail for an environmental reason and the
	// failure must be reported as a degraded prerequisite, never cached as a
	// native_unsupported device verdict.
	minimumDPDKHugepages = 256
	// capabilityEnvironmentCacheTTL keeps status reads from invoking vppctl for
	// every physical interface in one request.
	capabilityEnvironmentCacheTTL = 2 * time.Second
)

type dpdkOwnershipProof struct {
	Mode   string
	Source string
}

type vppIdentity struct {
	Version   string
	RuntimeID string
}

var (
	vppIdentityMu    sync.Mutex
	vppIdentityAt    time.Time
	vppIdentityValue vppIdentity
)

func serverNow(server *Server) time.Time {
	if server != nil && server.now != nil {
		return server.now().UTC()
	}
	return time.Now().UTC()
}

func takeoverGuardTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LY_ROUTE_TAKEOVER_GUARD_TTL_SECONDS"))
	if raw == "" {
		return dataplane.DefaultTakeoverGuardTTL
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return dataplane.DefaultTakeoverGuardTTL
	}
	return time.Duration(seconds) * time.Second
}

// pciBoundDriver returns the driver currently bound to a PCI device. It is the
// evidence that the device is actually held by VFIO/UIO rather than only being
// named in a stale inventory record.
func pciBoundDriver(pci string) string {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return ""
	}
	driver, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot(), "bus/pci/devices", pci, "driver"))
	if err != nil {
		return ""
	}
	current := filepath.Base(driver)
	if current == "." || current == string(filepath.Separator) {
		return ""
	}
	return current
}

func dpdkHugepagesCurrent() int {
	if raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_HUGEPAGES")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value >= 0 {
			return value
		}
	}
	data, err := os.ReadFile(filepath.Join(sysfsRoot(), "kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"))
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func dpdkHugepagesRequired() int {
	raw := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_HUGEPAGES_REQUIRED"))
	if raw == "" {
		return minimumDPDKHugepages
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return minimumDPDKHugepages
	}
	return value
}

// hugepagesShortfall reports the DPDK hugepage prerequisite. ok is false when
// a candidate probe would start without enough 2 MiB pages.
func hugepagesShortfall() (needed, current int, ok bool) {
	needed = dpdkHugepagesRequired()
	current = dpdkHugepagesCurrent()
	return needed, current, current >= needed
}

// dpdkProbeAllowedByHugepages is the gate in front of any candidate VPP start.
// A shortfall is logged as an explicit degraded prerequisite and never written
// to the native_unsupported cache: the NIC is fine, the environment is not.
func (server *Server) dpdkProbeAllowedByHugepages(eligible []string) bool {
	needed, current, enough := hugepagesShortfall()
	if enough {
		return true
	}
	log.Printf("dataplane takeover degraded for %s: hugepages 不足：需要 %d，当前 %d; Linux keeps the kernel driver and no candidate VPP is started", strings.Join(eligible, ","), needed, current)
	return false
}

func currentVPPIdentity() vppIdentity {
	version := strings.TrimSpace(os.Getenv("LY_ROUTE_VPP_VERSION"))
	runtimeID := strings.TrimSpace(os.Getenv("LY_ROUTE_VPP_RUNTIME_ID"))
	if version != "" && runtimeID != "" {
		return vppIdentity{Version: version, RuntimeID: runtimeID}
	}
	vppIdentityMu.Lock()
	defer vppIdentityMu.Unlock()
	if !vppIdentityAt.IsZero() && time.Since(vppIdentityAt) < capabilityEnvironmentCacheTTL {
		identity := vppIdentityValue
		if version != "" {
			identity.Version = version
		}
		if runtimeID != "" {
			identity.RuntimeID = runtimeID
		}
		return identity
	}
	identity := vppIdentity{RuntimeID: vppProcessRuntimeID()}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	output, err := exec.CommandContext(ctx, vppctl, "show", "version").Output()
	cancel()
	if err == nil {
		line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
		identity.Version = line
	}
	vppIdentityAt = time.Now()
	vppIdentityValue = identity
	if version != "" {
		identity.Version = version
	}
	if runtimeID != "" {
		identity.RuntimeID = runtimeID
	}
	return identity
}

// vppProcessRuntimeID changes when VPP restarts. A native verdict from before
// a restart must be retried against the new VPP instance even when the version
// string is unchanged.
func vppProcessRuntimeID() string {
	pidFile := envOrDefault("LY_ROUTE_VPP_PID_FILE", "/run/vpp/vpp.pid")
	data, err := os.ReadFile(pidFile)
	pid := ""
	if err == nil {
		pid = strings.TrimSpace(string(data))
	}
	if pid == "" {
		pid = vppServiceMainPID()
	}
	if pid == "" {
		return ""
	}
	stat, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(stat))
	if len(fields) < 22 {
		return ""
	}
	return fields[21]
}

// vppServiceMainPID is the fallback when this VPP build has no pid file. The
// main PID plus the /proc start time is restart-stable evidence: a manual
// restart changes it even when the version string stays the same.
func vppServiceMainPID() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, envOrDefault("LY_ROUTE_SYSTEMCTL", "systemctl"), "show", "vpp.service", "--property=MainPID", "--value").Output()
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(output))
	if pid == "" || pid == "0" {
		return ""
	}
	for _, char := range pid {
		if char < '0' || char > '9' {
			return ""
		}
	}
	return pid
}

func (server *Server) capabilityEnvironmentFor(pci, kernelDriver string) dataplane.CapabilityEnvironment {
	pci = strings.TrimSpace(pci)
	if strings.TrimSpace(kernelDriver) == "" {
		kernelDriver = recordedKernelDriver(pci)
	}
	identity := currentVPPIdentity()
	return dataplane.CapabilityEnvironment{
		KernelDriver: strings.TrimSpace(kernelDriver),
		BoundDriver:  pciBoundDriver(pci),
		VPPVersion:   identity.Version,
		VPPRuntimeID: identity.RuntimeID,
		Hugepages:    dpdkHugepagesCurrent(),
		PCIAddress:   pci,
	}
}

func recordedKernelDriver(pci string) string {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return ""
	}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, device := range state.Devices {
			if strings.TrimSpace(device.PCIAddress) == pci {
				return strings.TrimSpace(device.KernelDriver)
			}
		}
	}
	if inventory, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
		for _, item := range inventory.DataInterfaces {
			if strings.TrimSpace(item.PCIAddress) == pci {
				return strings.TrimSpace(item.KernelDriver)
			}
		}
	}
	return ""
}

// validatedDPDKOwnershipProof answers "is this port already owned by the
// product and bound to a DPDK driver?" without starting VPP or touching the
// device. The proof sources are deliberately device-level (ownership, committed
// active state, verification, inventory), never a takeover-mode string.
func (server *Server) validatedDPDKOwnershipProof(name, pci string) (dpdkOwnershipProof, bool) {
	name = strings.TrimSpace(name)
	pci = strings.TrimSpace(pci)
	if name == "" || pci == "" {
		return dpdkOwnershipProof{}, false
	}
	bound := pciBoundDriver(pci)
	mode := takeoverModeFromDriver(bound)
	if mode != "dpdk_uio" && mode != "dpdk_vfio" {
		return dpdkOwnershipProof{}, false
	}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		ownershipState := strings.ToLower(strings.TrimSpace(state.State))
		if ownershipState == "" || ownershipState == "active" {
			for _, device := range state.Devices {
				if strings.TrimSpace(device.PCIAddress) != pci {
					continue
				}
				if recorded := strings.TrimSpace(device.LinuxInterface); recorded != "" && recorded != name {
					continue
				}
				if transport := strings.ToLower(strings.TrimSpace(device.Transport)); transport != "" && transport != transportForMode(mode) {
					continue
				}
				return dpdkOwnershipProof{Mode: mode, Source: "ownership"}, true
			}
		}
	}
	if mode, ok := activeDPDKOwnershipProof(name, pci); ok {
		return dpdkOwnershipProof{Mode: mode, Source: "committed_active_state"}, true
	}
	if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
		for _, entry := range state.Interfaces {
			if strings.TrimSpace(entry.LinuxInterface) != name {
				continue
			}
			if recorded := strings.TrimSpace(entry.PCIAddress); recorded != "" && recorded != pci {
				continue
			}
			entryMode := takeoverModeFromDriver(entry.Mode)
			if entryMode != "dpdk_uio" && entryMode != "dpdk_vfio" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(entry.Verification)) {
			case "verified", "pending_link":
				return dpdkOwnershipProof{Mode: entryMode, Source: "dpdk_verification"}, true
			}
		}
	}
	if inventory, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
		for _, item := range inventory.DataInterfaces {
			if strings.TrimSpace(item.LinuxInterface) != name || strings.TrimSpace(item.PCIAddress) != pci {
				continue
			}
			return dpdkOwnershipProof{Mode: mode, Source: "inventory"}, true
		}
	}
	return dpdkOwnershipProof{}, false
}

func transportForMode(mode string) string {
	if mode == "dpdk_vfio" {
		return "vfio"
	}
	return "uio"
}

func activeDPDKOwnershipProof(name, pci string) (string, bool) {
	data, err := os.ReadFile(envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json"))
	if err != nil {
		return "", false
	}
	var state activeDataplaneFile
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false
	}
	for _, attachment := range state.Path.Attachments {
		if strings.TrimSpace(attachment.LinuxInterface) != name {
			continue
		}
		if recorded := strings.TrimSpace(attachment.PCIAddress); recorded != "" && recorded != pci {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(attachment.Verification), "failed") {
			continue
		}
		mode := takeoverModeFromAttachment(attachment.Tier, attachment.Hook, attachment.Mode)
		if mode == "dpdk_uio" || mode == "dpdk_vfio" {
			return mode, true
		}
	}
	return "", false
}

// takeoverGuardActive evaluates the persisted native_unsupported cache against
// the current environment. It logs both the decision and the reason so every
// skip says why it was skipped and every retry says why the cache was not
// reused.
func (server *Server) takeoverGuardActive(capabilities dataplane.TakeoverCapabilityState, driver, pci, name string) bool {
	if _, exists := capabilities.NativeUnsupported(driver, pci); !exists {
		return false
	}
	environment := server.capabilityEnvironmentFor(pci, driver)
	capability, active, inactiveReason := capabilities.ActiveUnsupported(driver, pci, environment, serverNow(server))
	if active {
		validUntil := capability.ValidUntil.UTC().Format(time.RFC3339)
		log.Printf("dataplane capability: %s (%s) native path marked unsupported until %s: %s; not probing", name, pci, validUntil, capability.Reason)
		return true
	}
	if capability.NativeUnsupported {
		log.Printf("dataplane capability: %s (%s) cached native_unsupported no longer applies (%s); retrying the probe", name, pci, inactiveReason)
	}
	return false
}

// markTakeoverGuardSupported retires a stale crash guard after a positive
// device-level proof. The record is updated in place so the cache carries the
// new environment fingerprint instead of looking permanently negative.
func (server *Server) markTakeoverGuardSupported(pci, driver, reason string) {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return
	}
	takeoverCapabilityMu.Lock()
	defer takeoverCapabilityMu.Unlock()
	capabilities, err := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	if err != nil {
		return
	}
	environment := server.capabilityEnvironmentFor(pci, driver)
	if !capabilities.MarkSupported(driver, pci, reason, environment, takeoverGuardTTL()) {
		return
	}
	if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
		log.Printf("persist positive takeover capability proof for %s: %v", pci, err)
		return
	}
	log.Printf("dataplane capability: %s (%s) probe/cache now positive (%s, fingerprint %s)", pci, strings.TrimSpace(environment.BoundDriver), reason, environment.Fingerprint())
}

// recordTakeoverProbeSuccess persists the positive side of the capability
// cache after a probe actually attached and verified the port. The previous
// guard (if any) is replaced by a fingerprinted observation, so an operator can
// see why the port is trusted and until when.
func (server *Server) recordTakeoverProbeSuccess(pci, driver, reason string) {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return
	}
	takeoverCapabilityMu.Lock()
	defer takeoverCapabilityMu.Unlock()
	capabilities, err := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	if err != nil {
		return
	}
	environment := server.capabilityEnvironmentFor(pci, driver)
	capabilities.RecordSupported(driver, pci, reason, environment, takeoverGuardTTL())
	if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
		log.Printf("persist positive takeover capability proof for %s: %v", pci, err)
		return
	}
	log.Printf("dataplane capability: %s (%s) probe succeeded (%s, fingerprint %s)", pci, strings.TrimSpace(environment.BoundDriver), reason, environment.Fingerprint())
}

// refreshTakeoverGuardEnvironments re-stamps crash guards after the recovery
// restart that follows a failed candidate. Without this the guard written
// before the restart would immediately look stale (the VPP runtime id changed),
// and the next tick would retry a device that genuinely crashed the PMD.
func (server *Server) refreshTakeoverGuardEnvironments(pcis []string) {
	if len(pcis) == 0 {
		return
	}
	wanted := map[string]bool{}
	for _, pci := range pcis {
		if pci = strings.TrimSpace(pci); pci != "" {
			wanted[pci] = true
		}
	}
	if len(wanted) == 0 {
		return
	}
	takeoverCapabilityMu.Lock()
	defer takeoverCapabilityMu.Unlock()
	capabilities, err := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	if err != nil {
		return
	}
	changed := false
	for index := range capabilities.Devices {
		device := &capabilities.Devices[index]
		if !device.NativeUnsupported || !wanted[strings.TrimSpace(device.PCIAddress)] {
			continue
		}
		environment := server.capabilityEnvironmentFor(device.PCIAddress, device.Driver)
		device.EnvironmentFingerprint = environment.Fingerprint()
		device.BoundDriver = strings.TrimSpace(environment.BoundDriver)
		device.VPPVersion = strings.TrimSpace(environment.VPPVersion)
		device.VPPRuntimeID = strings.TrimSpace(environment.VPPRuntimeID)
		device.Hugepages = environment.Hugepages
		device.ValidUntil = time.Now().UTC().Add(takeoverGuardTTL())
		changed = true
	}
	if !changed {
		return
	}
	if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
		log.Printf("refresh takeover guard environment: %v", err)
	}
}
