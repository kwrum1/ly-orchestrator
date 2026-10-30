package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
	"ly-route/backend/internal/runtime/vpp"
)

// takeoverLifecycleTimeout bounds one privileged lifecycle command. The
// command itself uses context.WithoutCancel so an aborted HTTP request cannot
// kill systemctl in the middle of a VPP restart.
const takeoverLifecycleTimeout = 90 * time.Second

func takeoverLifecycleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), takeoverLifecycleTimeout)
}

// pciHasIOMMUGroup reports whether the device exposes a numeric IOMMU group.
// It is the cheap save-time gate; prepare-vfio.sh performs the full isolation
// check against the group's other members before it binds anything.
func pciHasIOMMUGroup(pci string) bool {
	pci = normalizePCICandidate(pci)
	if pci == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot(), "bus/pci/devices", pci, "iommu_group"))
	if err != nil {
		return false
	}
	group := filepath.Base(resolved)
	if group == "" || group == "." || group == string(filepath.Separator) {
		return false
	}
	for _, char := range group {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// automaticTakeoverUpgrade answers "should the backend change the mode of an
// already-owned port?". The answer is no: a port that already has a mode is
// reconciled, never upgraded by an apply. This is the guard that stops an
// empty/old configuration apply from replaying a stale plan (the E37 incident:
// an apply re-selected af_xdp_copy for an igc port because a verification file
// still claimed it was verified).
//
// A real mode change requires an explicit ownership/role event. The first
// takeover of a kernel-owned port is handled by bootstrapDataplaneOwnership,
// and a forbidden live mode is repaired by migrateForbiddenTakeoverModes; both
// paths are separate from this function.
func (server *Server) automaticTakeoverUpgrade(ctx context.Context, name, currentMode string, capabilities dataplane.TakeoverCapabilityState) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == server.managementInterfaceID(ctx) {
		return "", false
	}
	if strings.TrimSpace(currentMode) != "" {
		// Existing mode: reconcile, never switch -- with one exception. A port
		// that was adopted as AF_PACKET before the rule's DPDK preference was
		// tried gets that one attempt now (WS-AK). The attempt is bounded by
		// the rule library, the hugepage prerequisite, the crash guard, the
		// durable dpdk_attempt_failed marker and the once-per-boot budget, so
		// it cannot become an apply-time retry loop.
		if target, migrate := server.rulePreferredMigration(ctx, name, currentMode, capabilities); migrate {
			return target, true
		}
		return "", false
	}
	// No mode is recorded. This is the only case where a first takeover can be
	// planned, and it is still gated by the rule library and the crash guard.
	plan := server.takeoverPlanForPort(ctx, name)
	primary, ok := plan.Primary()
	if !ok || primary.Mode == takeover.ModeAFPacket {
		return "", false
	}
	if takeoverModeDestructive(string(primary.Mode)) && server.takeoverGuardActive(capabilities, strings.TrimSpace(inventoryDriver(name)), inventoryPCI(name), name) {
		return "", false
	}
	return string(primary.Mode), true
}

// detectTakeoverSnapshot identifies the current takeover mode from runtime
// evidence, never from an operator-supplied value. Destructive bindings are
// confirmed by the PCI driver; non-destructive modes by the verification,
// committed active state or a live VPP interface.
func (server *Server) detectTakeoverSnapshot(ctx context.Context, name string, vppNames map[string]bool) takeoverSnapshot {
	// Accept a product id (eth1), Linux name, PCI or MAC. The state machine and
	// the capability report speak Linux names; the policy speaks product ids.
	identity := server.resolveTakeoverIdentity(ctx, name)
	linuxName := strings.TrimSpace(identity.LinuxInterface)
	if linuxName == "" {
		linuxName = strings.TrimSpace(name)
	}
	snapshot := takeoverSnapshot{LinuxInterface: linuxName, Netdev: linuxName, VPPInterface: "lyroute-" + linuxName}
	if linuxName == "" {
		return snapshot
	}
	snapshot.PCIAddress = strings.TrimSpace(identity.PCI)
	if item, ok := readInventoryInterface(linuxName); ok {
		if snapshot.PCIAddress == "" {
			snapshot.PCIAddress = strings.TrimSpace(item.PCIAddress)
		}
		snapshot.KernelDriver = strings.TrimSpace(item.KernelDriver)
	}
	if snapshot.PCIAddress == "" {
		snapshot.PCIAddress = inventoryPCI(linuxName)
	}
	if snapshot.PCIAddress != "" {
		snapshot.BoundDriver = pciBoundDriver(snapshot.PCIAddress)
	}
	if snapshot.KernelDriver == "" {
		snapshot.KernelDriver = recordedKernelDriver(snapshot.PCIAddress)
	}
	if snapshot.KernelDriver == "" && snapshot.BoundDriver != "" && !takeoverModeDestructive(takeoverModeFromDriver(snapshot.BoundDriver)) {
		snapshot.KernelDriver = snapshot.BoundDriver
	}
	if mode := takeoverModeFromDriver(snapshot.BoundDriver); takeoverModeDestructive(mode) {
		snapshot.CurrentMode = mode
		snapshot.ModeConfirmed = true
		snapshot.Evidence = append(snapshot.Evidence, "pci bound to "+snapshot.BoundDriver)
	}
	// The live VPP attachment outranks every recorded hint. A stale verification
	// (or a proof the watcher rewrote) otherwise makes the switch delete the
	// wrong interface and hit EBUSY on the create.
	if live := server.observeNativeAttachment(ctx, linuxName, snapshot.VPPInterface); live.Reachable && live.present() {
		if mode := live.mode(); mode != "" && !live.ambiguous() {
			if snapshot.CurrentMode != "" && snapshot.CurrentMode != mode {
				snapshot.Evidence = append(snapshot.Evidence, "live VPP attachment is "+mode+"; the recorded "+snapshot.CurrentMode+" is stale")
			}
			snapshot.CurrentMode = mode
			snapshot.ModeConfirmed = true
			snapshot.InterfaceNameOnly = false
			snapshot.SWIfIndex = live.SWIfIndex
			snapshot.XDPProgram = live.XDPProgram
			snapshot.Evidence = append(snapshot.Evidence, "live VPP "+live.Hook+" interface for "+linuxName)
		} else {
			// Present but ambiguous (two rows with the product name) or with an
			// unreadable plugin. It is an attachment, but not one that can be
			// planned against; the switch has to tear it down explicitly.
			snapshot.CurrentMode = takeoverModeAFPacket
			snapshot.ModeConfirmed = false
			snapshot.InterfaceNameOnly = true
			snapshot.Duplicate = live.ambiguous()
			snapshot.Evidence = append(snapshot.Evidence, "live VPP attachment is ambiguous or unreadable: "+live.summary())
		}
	}
	// The verification record is the committed proof of a non-destructive
	// attachment. Ownership is a destructive-intent record, active.json is the
	// committed generation; the VPP interface name is only a weak fallback.
	if snapshot.CurrentMode == "" {
		if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
			for _, entry := range state.Interfaces {
				if strings.TrimSpace(entry.LinuxInterface) != linuxName {
					continue
				}
				if pciMismatch(snapshot.PCIAddress, entry.PCIAddress) {
					snapshot.Evidence = append(snapshot.Evidence, "stale verification for a different PCI device")
					continue
				}
				if mode := takeoverModeFromDriver(entry.Mode); mode != "" {
					if !server.takeoverPortModeAllowed(ctx, linuxName, firstNonEmptyTrimmed(snapshot.PCIAddress, entry.PCIAddress), mode) {
						recordStaleModeDropped(linuxName, firstNonEmptyTrimmed(snapshot.PCIAddress, entry.PCIAddress), firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(linuxName)), mode, "dpdk_verification", "the rule library forbids this mode; the record is ignored")
						dropForbiddenModeRecords(linuxName, snapshot.PCIAddress, firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(linuxName)), portVendorDevice(linuxName))
						snapshot.Evidence = append(snapshot.Evidence, "stale verification mode="+mode+" dropped by the rule library")
						continue
					}
					snapshot.CurrentMode = mode
					snapshot.ModeConfirmed = true
					snapshot.Evidence = append(snapshot.Evidence, "dpdk-verification mode="+entry.Mode)
					break
				}
			}
		}
	}
	if snapshot.CurrentMode == "" {
		if mode, ok := activeDataplaneModeFor(linuxName, snapshot.PCIAddress); ok && mode != "" {
			if !server.takeoverPortModeAllowed(ctx, linuxName, snapshot.PCIAddress, mode) {
				recordStaleModeDropped(linuxName, snapshot.PCIAddress, firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(linuxName)), mode, "active_generation", "the rule library forbids this mode; the record is ignored")
				dropForbiddenModeRecords(linuxName, snapshot.PCIAddress, firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(linuxName)), portVendorDevice(linuxName))
				snapshot.Evidence = append(snapshot.Evidence, "active generation mode="+mode+" dropped by the rule library")
			} else {
				snapshot.CurrentMode = mode
				snapshot.ModeConfirmed = true
				snapshot.Evidence = append(snapshot.Evidence, "active generation mode="+mode)
			}
		}
	}
	if snapshot.CurrentMode == "" {
		if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
			for _, device := range state.Devices {
				if strings.TrimSpace(device.LinuxInterface) != linuxName && !samePCI(device.PCIAddress, snapshot.PCIAddress) {
					continue
				}
				if value := strings.TrimSpace(device.PreviousTransport); value != "" {
					snapshot.PreviousDriver = firstNonEmptyTrimmed(snapshot.PreviousDriver, takeoverModeDriver(takeoverModeFromDriver(value)))
					snapshot.Evidence = append(snapshot.Evidence, "ownership previous transport="+value)
				}
				if mode := takeoverModeFromDriver(device.Transport); mode != "" {
					if !server.takeoverPortModeAllowed(ctx, linuxName, snapshot.PCIAddress, mode) {
						recordStaleModeDropped(linuxName, snapshot.PCIAddress, firstNonEmptyTrimmed(snapshot.KernelDriver, portKernelDriver(linuxName)), mode, "ownership_transport", "the rule library forbids this transport; the record is ignored")
						snapshot.Evidence = append(snapshot.Evidence, "ownership transport="+mode+" dropped by the rule library")
						continue
					}
					snapshot.CurrentMode = mode
					snapshot.ModeConfirmed = true
					snapshot.Evidence = append(snapshot.Evidence, "ownership transport="+device.Transport)
					break
				}
			}
		}
	}
	if snapshot.CurrentMode == "" && (vppNames[snapshot.VPPInterface] || vppNames[linuxName]) {
		// VPP holds an interface with the product name. The concrete hook is not
		// recorded, so the mode is a guess and cannot be rolled back blindly.
		// Callers must treat this as a stale/unknown attachment, not proof of a
		// working takeover.
		snapshot.CurrentMode = takeoverModeAFPacket
		snapshot.ModeConfirmed = false
		snapshot.InterfaceNameOnly = true
		snapshot.Evidence = append(snapshot.Evidence, "VPP owns the product interface name without ownership evidence")
	}
	snapshot.Owned = snapshot.CurrentMode != ""
	if snapshot.Owned {
		server.fillTakeoverRuntimeFacts(ctx, &snapshot)
	}
	return snapshot
}

// resolveTakeoverIdentity maps a product id, Linux name, PCI address or MAC to
// the runtime identity. An unresolvable value is kept as its own Linux name so
// capability lookups still work on builds where the identity service is absent.
func (server *Server) resolveTakeoverIdentity(ctx context.Context, value string) interfaceIdentity {
	value = strings.TrimSpace(value)
	if value == "" {
		return interfaceIdentity{}
	}
	for _, identity := range server.interfaceIdentities(ctx) {
		if strings.EqualFold(identity.ID, value) ||
			strings.EqualFold(identity.LinuxInterface, value) ||
			samePCI(identity.PCI, value) ||
			(identity.MAC != "" && strings.EqualFold(identity.MAC, value)) {
			return identity
		}
	}
	return interfaceIdentity{ID: value, LinuxInterface: value}
}

// samePCI compares two PCI addresses. An empty side is never a match: the
// caller uses the Linux name as the fallback identity instead.
func samePCI(left, right string) bool {
	left = normalizePCICandidate(left)
	right = normalizePCICandidate(right)
	return left != "" && left == right
}

// pciMismatch reports whether two known PCI addresses differ.
func pciMismatch(left, right string) bool {
	left = normalizePCICandidate(left)
	right = normalizePCICandidate(right)
	return left != "" && right != "" && left != right
}

// activeDataplaneModeFor returns the committed mode only when the attachment
// belongs to the same PCI device. A reused Linux name after a NIC swap must not
// resurrect the previous generation's mode.
func activeDataplaneModeFor(name, pci string) (string, bool) {
	data, err := os.ReadFile(envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json"))
	if err != nil {
		return "", false
	}
	var state activeDataplaneFile
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false
	}
	for _, attachment := range state.Path.Attachments {
		if strings.TrimSpace(attachment.LinuxInterface) != strings.TrimSpace(name) {
			continue
		}
		if pciMismatch(pci, attachment.PCIAddress) {
			continue
		}
		if mode := takeoverModeFromAttachment(attachment.Tier, attachment.Hook, attachment.Mode); mode != "" {
			return mode, true
		}
	}
	return "", false
}

// fillTakeoverRuntimeFacts records the runtime handles that make a transition
// auditable: sw_if_index, AF_XDP queue fds and the attached XDP program. Every
// read is best effort; an unavailable fact stays empty rather than guessed.
func (server *Server) fillTakeoverRuntimeFacts(ctx context.Context, snapshot *takeoverSnapshot) {
	if snapshot == nil || !server.vppReachable(ctx) {
		return
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if output, err := execCommandOutput(readCtx, vppctl, "show", "interface", snapshot.VPPInterface); err == nil {
		snapshot.SWIfIndex = parseVPPInterfaceIndex(output, snapshot.VPPInterface)
	}
	if output, err := execCommandOutput(readCtx, vppctl, "show", "af_xdp"); err == nil {
		snapshot.QueueFDs = parseAFXDPQueueFDs(output)
	}
	if snapshot.Netdev != "" {
		ip := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
		if output, err := exec.CommandContext(readCtx, ip, "-details", "link", "show", "dev", snapshot.Netdev).Output(); err == nil {
			snapshot.XDPProgram = parseXDPProgramID(string(output))
		}
	}
}

// takeoverCapabilityForMode produces the four-state assessment for one switch
// target. A missing report is unknown, not unsupported and certainly not
// supported.
func (server *Server) takeoverCapabilityForMode(name, pci, mode string, capabilities dataplane.TakeoverCapabilityState) vpp.CapabilityAssessment {
	name = strings.TrimSpace(name)
	mode = strings.TrimSpace(mode)
	if mode == takeoverModeAFPacket {
		return vpp.CapabilityAssessment{
			Hook:        vpp.NativeHookAFPacket,
			Mode:        vpp.NativeModeAFPacket,
			Status:      vpp.CapabilitySupportedWithConstraints,
			Constraints: []string{"compatibility mode (performance limited)"},
			Source:      "product_policy",
		}
	}
	if takeoverModeDestructive(mode) && server.takeoverGuardActive(capabilities, recordedKernelDriver(pci), pci, name) {
		return vpp.CapabilityAssessment{
			Hook:   vpp.NativeHookDPDK,
			Mode:   vppNativeModeForTakeover(mode),
			Status: vpp.CapabilityUnsupported,
			Reason: "the native path is crash-guarded for this device and environment",
			Source: "takeover_guard",
		}
	}
	if takeoverModeDestructive(mode) && pci != "" {
		// A successful transactional probe is positive evidence that outranks a
		// stale capability report ("no live VPP DPDK attach"): the probe actually
		// bound, started VPP and read the interface back.
		driver := recordedKernelDriver(pci)
		environment := server.capabilityEnvironmentFor(pci, driver)
		if proof, ok := capabilities.ActiveSupported(driver, pci, environment, serverNow(server)); ok {
			recordedMode := takeoverModeFromDriver(proof.BoundDriver)
			if proof.BoundDriver == "" || recordedMode == mode {
				assessment := vpp.CapabilityAssessment{
					Hook:   vpp.NativeHookDPDK,
					Mode:   vppNativeModeForTakeover(mode),
					Status: vpp.CapabilitySupported,
					Reason: "verified takeover probe: " + strings.TrimSpace(proof.Reason),
					Source: "takeover_proof",
				}
				if mode == takeoverModeDPDKUIO {
					assessment.Status = vpp.CapabilitySupportedWithConstraints
					assessment.Constraints = []string{"no IOMMU protection (uio_pci_generic)"}
				}
				return assessment
			}
		}
	}
	report, err := loadCapabilityReport(envOrDefault("LY_ROUTE_VPP_CAPABILITY_PROOF", "/var/lib/ly-route/vpp-native-capabilities.json"))
	if err != nil {
		// A missing report is the normal state on a fresh appliance; the
		// answer is unknown and the conservative path applies.
		if os.IsNotExist(err) {
			return vpp.CapabilityAssessment{Status: vpp.CapabilityUnknown, Reason: "runtime capability report is unavailable", Source: "capability_report"}
		}
		return vpp.CapabilityAssessment{Status: vpp.CapabilityUnknown, Reason: "runtime capability report is unreadable: " + err.Error(), Source: "capability_report"}
	}
	for _, proof := range report.Proofs {
		if strings.TrimSpace(proof.LinuxInterface) != name {
			continue
		}
		candidates := proof.Candidates
		if len(candidates) == 0 {
			candidates = []vpp.CapabilityProof{proof.Proof}
		}
		for _, candidate := range candidates {
			if takeoverModeFromAttachment(candidate.Tier, candidate.Hook, candidate.Mode) != mode {
				continue
			}
			if !candidate.ValidUntil.IsZero() && candidate.ValidUntil.Before(serverNow(server)) {
				return vpp.CapabilityAssessment{
					Hook:   vppNativeHookForTakeover(mode),
					Mode:   vppNativeModeForTakeover(mode),
					Status: vpp.CapabilityUnknown,
					Reason: "the runtime capability proof for this mode is stale",
					Source: "capability_report",
				}
			}
			assessment := vpp.AssessCapabilityProof(candidate)
			assessment.Source = "capability_report"
			return assessment
		}
	}
	return vpp.CapabilityAssessment{
		Hook:   vppNativeHookForTakeover(mode),
		Mode:   vppNativeModeForTakeover(mode),
		Status: vpp.CapabilityUnknown,
		Reason: "no runtime capability proof exists for " + takeoverModeLabel(mode),
		Source: "capability_report",
	}
}

func vppNativeHookForTakeover(mode string) vpp.NativeHook {
	switch mode {
	case takeoverModeDPDKVFIO, takeoverModeDPDKUIO:
		return vpp.NativeHookDPDK
	case takeoverModeAFPacket:
		return vpp.NativeHookAFPacket
	case takeoverModeRDMA:
		return vpp.NativeHookRDMA
	}
	return vpp.NativeHookAFXDP
}

func vppNativeModeForTakeover(mode string) vpp.NativeMode {
	switch mode {
	case takeoverModeDPDKVFIO:
		return vpp.NativeModeDPDKVFIO
	case takeoverModeDPDKUIO:
		return vpp.NativeModeDPDKUIO
	case takeoverModeAFPacket:
		return vpp.NativeModeAFPacket
	case takeoverModeAFXDPZeroCopy:
		return vpp.NativeModeZeroCopy
	case takeoverModeAFXDPCopy:
		return vpp.NativeModeCopy
	case takeoverModeRDMA:
		return vpp.NativeModeRDMADV
	}
	return vpp.NativeModeCopy
}

// systemTakeoverExecutor performs the privileged side effects of a switch. It
// is deliberately thin: all decisions live in planTakeoverTransition.
type systemTakeoverExecutor struct {
	server *Server
}

func (executor systemTakeoverExecutor) Execute(ctx context.Context, transition *takeoverTransition, step takeoverStep) (takeoverStepResult, error) {
	switch step.Kind {
	case "detach":
		return takeoverStepResult{}, executor.detach(ctx, transition, step.Mode)
	case "bind":
		return takeoverStepResult{}, executor.attach(ctx, transition, step.Mode)
	case "verify":
		return executor.verify(ctx, transition)
	case "record":
		return takeoverStepResult{}, nil
	}
	return takeoverStepResult{}, fmt.Errorf("unknown takeover step %q", step.Kind)
}

func (executor systemTakeoverExecutor) Rollback(ctx context.Context, transition *takeoverTransition, step takeoverStep) error {
	// The engine replays the applied forward steps in reverse; "bind" is undone
	// by restoring the previous attachment (or removing the new one when there
	// was no previous owner), "detach" by restoring it as well.
	switch step.Kind {
	case "bind", "remove":
		// A DPDK device must be stopped before it is unbound. A native
		// attachment is removed through VPP and must not stop the process:
		// the previous attachment is restored right after this step.
		if takeoverModeDestructive(step.Mode) {
			if err := executor.stopVPP(ctx); err != nil {
				return err
			}
			return executor.releaseDPDK(ctx, transition)
		}
		// Undoing a live AF_PACKET <-> AF_XDP replace means rebuilding the
		// previous attachment, not merely deleting the target: the port must
		// end up carrying traffic again.
		if from := strings.TrimSpace(transition.FromMode); from != "" && !takeoverModeDestructive(from) {
			return executor.restoreNativeAttachmentLive(ctx, transition, from)
		}
		return executor.removeNativeAttachmentLive(ctx, transition)
	case "detach", "restore":
		if !takeoverModeDestructive(step.Mode) && takeoverModeDestructive(transition.ToMode) && !executor.server.vppReachable(ctx) {
			// The forward path stopped VPP to release the DPDK device. The
			// previous native attachment cannot be rebuilt before the process
			// is running again; Recover restarts VPP and re-adopts it. Failing
			// here would mislabel a recoverable rollback as fatal and block the
			// bootstrap path for two minutes.
			return nil
		}
		return executor.restore(ctx, transition, step.Mode)
	}
	return nil
}

// Recover is the final rollback step. A transition that stopped VPP is not
// rolled back until the process is running again; a native replace never stops
// VPP, so recovering it must not restart it (a restart would drop every port).
func (executor systemTakeoverExecutor) Recover(ctx context.Context, transition *takeoverTransition) error {
	if !takeoverModeDestructive(transition.FromMode) && !takeoverModeDestructive(transition.ToMode) {
		if !executor.server.vppReachable(ctx) {
			return fmt.Errorf("VPP is not reachable after the native switch")
		}
		if mode := strings.TrimSpace(transition.FromMode); mode != "" {
			transition.RollbackDetail = "the previous " + takeoverModeLabel(mode) + " attachment was recreated and VPP is running"
		}
		return nil
	}
	if !executor.server.vppReachable(ctx) {
		if !executor.server.restartVPPAndWait(ctx) {
			return fmt.Errorf("VPP did not become ready after the rollback")
		}
		transition.RollbackDetail = "VPP was restarted after the rollback"
	} else {
		transition.RollbackDetail = firstNonEmptyTrimmed(transition.RollbackDetail, "VPP is running after the rollback")
	}
	if mode := strings.TrimSpace(transition.FromMode); mode != "" && !takeoverModeDestructive(mode) {
		lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
		defer cancel()
		if entry, ok := executor.server.adoptNativeAttachmentLocked(lifecycleCtx, transition.Interface, mode); ok {
			transition.RollbackDetail = "the previous " + takeoverModeLabel(mode) + " attachment was recreated and VPP is running (verification=" + entry.Verification + ")"
		} else {
			// VPP itself is usable; the attachment is reconciled by the next
			// apply. Do not turn this into a rollback failure.
			transition.RollbackDetail = "VPP is running; the previous " + takeoverModeLabel(mode) + " attachment will be recreated by the next apply"
		}
	}
	return nil
}

func (executor systemTakeoverExecutor) stopVPP(ctx context.Context) error {
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
	stop := envOrDefault("LY_ROUTE_VPP_STOP_COMMAND", "systemctl stop vpp.service")
	if output, err := exec.CommandContext(lifecycleCtx, "/bin/sh", "-c", stop).CombinedOutput(); err != nil {
		// A VPP that is already stopped is exactly the state a detach wants.
		if vppDownSignature(err.Error() + " " + string(output)) {
			return nil
		}
		return fmt.Errorf("stop VPP: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (executor systemTakeoverExecutor) releaseDPDK(ctx context.Context, transition *takeoverTransition) error {
	pci := strings.TrimSpace(transition.PCIAddress)
	if pci == "" {
		return fmt.Errorf("the PCI address of %s is unknown; cannot release the DPDK binding", transition.Interface)
	}
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
	command := envOrDefault("LY_ROUTE_DPDK_RELEASE_COMMAND", "/usr/lib/ly-route/release-dpdk-port.sh")
	if output, err := exec.CommandContext(lifecycleCtx, command, pci).CombinedOutput(); err != nil {
		return fmt.Errorf("release %s: %w: %s", pci, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (executor systemTakeoverExecutor) detach(ctx context.Context, transition *takeoverTransition, mode string) error {
	if mode == "" {
		return nil
	}
	// A native attachment is owned by VPP, not by the kernel. For a destructive
	// target it must be gone (and read back) before the PCI device is unbound.
	// For a native target the teardown happens inside the bind step, so that a
	// failed create can still be rolled back to this attachment.
	if !takeoverModeDestructive(mode) {
		if err := executor.removeNative(ctx, transition, mode); err != nil {
			return fmt.Errorf("detach %s before switching %s: %w", takeoverModeLabel(mode), transition.Interface, err)
		}
		// A destructive target needs VPP stopped before the PCI device is
		// unbound; a native-to-native replace must keep VPP (and every other
		// port) running.
		if !takeoverModeDestructive(transition.ToMode) {
			return nil
		}
		return executor.stopVPP(ctx)
	}
	// VPP must not hold the device while the PCI device is unbound.
	if err := executor.stopVPP(ctx); err != nil {
		return err
	}
	return executor.releaseDPDK(ctx, transition)
}

func (executor systemTakeoverExecutor) attach(ctx context.Context, transition *takeoverTransition, mode string) error {
	if takeoverModeDestructive(mode) {
		if err := executor.bindMode(ctx, transition, mode); err != nil {
			return err
		}
		if !executor.server.restartVPPAndWait(ctx) {
			return fmt.Errorf("VPP did not become ready after switching %s to %s", transition.Interface, takeoverModeLabel(mode))
		}
		return nil
	}
	// A native target is created live. Restarting VPP here (the previous
	// behaviour) drops every other port and every LCP pair, and it cannot clear
	// the XDP program left on the netdev: that is the EBUSY loop.
	return executor.convergeNativeAttachment(ctx, transition)
}

// bindMode performs the binding half of a switch without restarting VPP. The
// forward path restarts before verification; the rollback path restarts once,
// after every undo step, in Recover.
func (executor systemTakeoverExecutor) bindMode(ctx context.Context, transition *takeoverTransition, mode string) error {
	if !takeoverModeDestructive(mode) {
		return nil
	}
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
	command := envOrDefault("LY_ROUTE_VFIO_PREFLIGHT_COMMAND", "/usr/lib/ly-route/prepare-vfio.sh")
	cmd := exec.CommandContext(lifecycleCtx, command)
	cmd.Env = append(os.Environ(),
		"LY_ROUTE_VFIO_BOOTSTRAP=apply",
		"LY_ROUTE_TAKEOVER_SWITCH=1",
		"LY_ROUTE_TAKEOVER_PCI="+strings.TrimSpace(transition.PCIAddress),
		"LY_ROUTE_TAKEOVER_TARGET_TRANSPORT="+takeoverModeTransport(mode),
		"LY_ROUTE_TAKEOVER_FROM_MODE="+strings.TrimSpace(transition.FromMode),
		// An automatic switch is decided by the rule library, which covers
		// every physical non-management port -- including one the installer
		// mapping never listed because its roles are still unset. Without this
		// the bind is refused and the port can never leave AF_PACKET (WS-AK).
		"LY_ROUTE_TAKEOVER_ALLOW_UNMAPPED_PHYSICAL=1",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("prepare-vfio %s: %w: %s", transition.PCIAddress, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (executor systemTakeoverExecutor) restore(ctx context.Context, transition *takeoverTransition, mode string) error {
	if mode == "" {
		return nil
	}
	if takeoverModeDestructive(mode) {
		return executor.bindMode(ctx, transition, mode)
	}
	// Native modes are rebuilt live and read back: the port must carry traffic
	// again before the rollback is reported as restored.
	return executor.restoreNativeAttachmentLive(ctx, transition, mode)
}

// restoreNativeAttachmentLive rebuilds a native attachment after a failed
// switch. It is the same verified teardown/build as a forward switch, so the
// rollback cannot report "restored" while the netdev is empty.
func (executor systemTakeoverExecutor) restoreNativeAttachmentLive(ctx context.Context, transition *takeoverTransition, mode string) error {
	if err := executor.convergeNativeAttachmentTo(ctx, transition, mode); err != nil {
		return fmt.Errorf("restore %s attachment for %s: %w", takeoverModeLabel(mode), transition.Interface, err)
	}
	if !executor.server.persistLiveVerification(ctx, takeoverSnapshot{
		LinuxInterface: transition.Interface,
		Netdev:         transition.Netdev,
		PCIAddress:     transition.PCIAddress,
		VPPInterface:   transition.VPPInterface,
	}, mode, "switch_rollback") {
		return fmt.Errorf("restore %s attachment for %s: the live readback did not confirm %s", takeoverModeLabel(mode), transition.Interface, takeoverModeLabel(mode))
	}
	return nil
}

// vppCommand runs one vppctl command on a lifecycle context and never hides
// the CLI text: a plugin error such as "Device or resource busy" must reach the
// journal instead of being flattened into a generic failure.
func (executor systemTakeoverExecutor) vppCommand(ctx context.Context, args ...string) (string, error) {
	lifecycleCtx, cancel := takeoverLifecycleContext(ctx)
	defer cancel()
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	output, err := exec.CommandContext(lifecycleCtx, vppctl, args...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, fmt.Errorf("vppctl %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

func (executor systemTakeoverExecutor) vppInterfacePresent(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	output, err := executor.vppCommand(ctx, "show", "interface")
	if err != nil {
		return false, err
	}
	return vppCLIInterfacePresent(output, name), nil
}

// removeNative tears down a non-destructive attachment with a VPP readback. It
// is the only deletion path: af_packet/cli.c ignores af_packet_delete_if's
// return value, so the command result is not evidence that the interface is
// gone.
func (executor systemTakeoverExecutor) removeNative(ctx context.Context, transition *takeoverTransition, mode string) error {
	netdev := strings.TrimSpace(transition.Netdev)
	if netdev == "" {
		return fmt.Errorf("the Linux netdev is unknown; cannot remove the native attachment")
	}
	vppName := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+netdev)
	live := executor.server.observeNativeAttachment(ctx, netdev, vppName)
	if !live.present() {
		return nil
	}
	if _, err := executor.teardownNativeAttachment(ctx, transition, live); err != nil {
		return err
	}
	return nil
}

// attachNative creates the requested native attachment. It is used by the
// restore paths; the forward switch goes through convergeNativeAttachment so
// teardown, create and readback stay one operation.
func (executor systemTakeoverExecutor) attachNative(ctx context.Context, transition *takeoverTransition, mode string) error {
	return executor.convergeNativeAttachmentTo(ctx, transition, mode)
}

// takeoverHardwareError marks a switch that failed because the device cannot
// do the requested mode (VPP reported a device error on the new attachment).
// Retrying it every few minutes would blip the port forever, so the watcher
// backs off much longer than for a transient failure.
type takeoverHardwareError struct {
	Interface string
	Mode      string
	Reason    string
}

func (err *takeoverHardwareError) Error() string {
	return fmt.Sprintf("%s does not support %s: %s", err.Interface, takeoverModeLabel(err.Mode), err.Reason)
}

// countVPPInterfaceRows was replaced by nativeAttachmentLive.ambiguous(), which
// counts product-named rows and host rows together.

func (executor systemTakeoverExecutor) verify(ctx context.Context, transition *takeoverTransition) (takeoverStepResult, error) {
	result := takeoverStepResult{}
	if !executor.server.vppReachable(ctx) {
		return result, fmt.Errorf("VPP is not reachable after the switch")
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if takeoverModeDestructive(transition.ToMode) {
		hardware, hardwareErr := execCommandOutput(readCtx, vppctl, "show", "hardware-interfaces")
		if hardwareErr != nil {
			return result, fmt.Errorf("vppctl show hardware-interfaces: %w", hardwareErr)
		}
		found := findVPPInterfaceByPCI(hardware, transition.PCIAddress)
		if found == "" {
			return result, fmt.Errorf("VPP did not create an interface for %s", transition.PCIAddress)
		}
		result.SWIfIndex = parseVPPInterfaceIndex(hardware, found)
		// A freshly created DPDK interface is administratively down. The port
		// it replaces was carrying traffic, so bring it up with the same
		// transactional readback: a PMD that cannot do admin-up fails the
		// switch and is rolled back to AF_PACKET. No operator consent is
		// involved and no other port is touched.
		interfaceState, _ := execCommandOutput(readCtx, vppctl, "show", "interface")
		if vppInterfaceState(interfaceState, found) != "up" {
			if _, err := executor.vppCommand(ctx, "set", "interface", "state", found, "up"); err != nil {
				return result, fmt.Errorf("set %s admin up: %w", found, err)
			}
			interfaceState, _ = execCommandOutput(readCtx, vppctl, "show", "interface")
			if vppInterfaceState(interfaceState, found) != "up" {
				return result, fmt.Errorf("VPP interface %s is still down after set interface state up", found)
			}
		}
		if carrierOutput, carrierErr := execCommandOutput(readCtx, vppctl, "show", "hardware-interfaces", found); carrierErr == nil {
			result.Output = "verified " + takeoverModeLabel(transition.ToMode) + " carrier=" + firstNonEmptyTrimmed(vppHardwareCarrier(carrierOutput), "unknown")
		}
		// Queue fds are an audit hint, never the proof: `show af_xdp` does not
		// exist in every build, so a failure here must not fail the switch.
		if afxdp, afxdpErr := execCommandOutput(readCtx, vppctl, "show", "af_xdp"); afxdpErr == nil {
			result.QueueFDs = parseAFXDPQueueFDs(afxdp)
		}
		if netdev := strings.TrimSpace(transition.Netdev); netdev != "" {
			result.XDPProgram = netdevXDPProgram(readCtx, netdev)
		}
		result.Output = "verified " + takeoverModeLabel(transition.ToMode)
		return result, nil
	}
	// A native switch is only applied when the live attachment is the plugin
	// that was requested. "exists and up" is not enough: an af_packet interface
	// that still carried the product name used to be recorded as a verified
	// af_xdp switch, and the next apply then trusted that proof.
	netdev := firstNonEmptyTrimmed(transition.Netdev, transition.Interface)
	vppInterface := firstNonEmptyTrimmed(transition.VPPInterface, "lyroute-"+transition.Interface)
	live := executor.server.observeNativeAttachment(ctx, netdev, vppInterface)
	if !live.Reachable {
		return result, fmt.Errorf("VPP is not reachable after the switch")
	}
	if live.ambiguous() {
		return result, fmt.Errorf("VPP interface %s is ambiguous (%d rows with the same name, %d host rows); refusing to report the switch as applied", vppInterface, len(live.Rows), len(live.HostRows))
	}
	if !live.matches(transition.ToMode) {
		return result, fmt.Errorf("VPP interface %s is not a live %s attachment after the switch (%s)", vppInterface, takeoverModeLabel(transition.ToMode), live.summary())
	}
	if live.State != "up" {
		return result, fmt.Errorf("VPP interface %s is %q after switching to %s, want up", vppInterface, live.State, takeoverModeLabel(transition.ToMode))
	}
	if live.SWIfIndex == 0 {
		return result, fmt.Errorf("VPP interface %s has no sw_if_index after switching to %s", vppInterface, takeoverModeLabel(transition.ToMode))
	}
	if live.Hook == nativeHookAFXDP && live.NetdevSeen != "" && netdev != "" && !strings.EqualFold(live.NetdevSeen, netdev) {
		return result, fmt.Errorf("AF_XDP interface %s is bound to netdev %q, want %q", vppInterface, live.NetdevSeen, netdev)
	}
	// A device in VPP's error state is attached but not forwarding. On the lab
	// igc port a copy-mode AF_XDP socket reported
	// "error tx poll() failed: Device or resource busy" and dropped its carrier
	// for a moment, but only after the first transmit attempt. Give the driver a
	// short, bounded settle window before deciding.
	if err := executor.awaitNativeAttachmentSettled(ctx, transition, netdev, vppInterface, &live); err != nil {
		return result, err
	}
	if transition.LCPHostInterface != "" && live.LCPHost == "" {
		return result, fmt.Errorf("LCP pair %s host-if %s is missing after the switch", vppInterface, transition.LCPHostInterface)
	}
	result.SWIfIndex = live.SWIfIndex
	result.XDPProgram = live.XDPProgram
	if afxdp, afxdpErr := execCommandOutput(readCtx, vppctl, "show", "af_xdp"); afxdpErr == nil {
		result.QueueFDs = parseAFXDPQueueFDs(afxdp)
	}
	result.Output = "verified " + takeoverModeLabel(transition.ToMode) + " (" + live.summary() + ")"
	return result, nil
}

// preferCommittedNativeCandidates makes the attachment that is *actually live*
// authoritative for the next generation; the verification file is only a hint.
//
// Without the live readback a stale proof decides the plan: a verified AF_XDP
// port falls back to the probe report's af_packet candidate and the apply
// creates a second interface with the same product name on the same netdev, or
// a stale af_xdp proof makes a port that is really AF_PACKET ask for af_xdp
// again. A committed proof is replaced by a single synthetic proof, so selection
// can only choose the mode that is attached (or, when nothing is attached, the
// mode that must be rebuilt). Changing modes stays an explicit switch.
func (server *Server) preferCommittedNativeCandidates(ctx context.Context, nativePath *vpp.NativePathRequest) {
	if nativePath == nil || len(nativePath.Assignments) == 0 {
		return
	}
	state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath())
	if err != nil {
		return
	}
	byName := map[string]dataplane.DPDKVerification{}
	for _, entry := range state.Interfaces {
		byName[strings.TrimSpace(entry.LinuxInterface)] = entry
	}
	now := serverNow(server)
	for index := range nativePath.Assignments {
		assignment := &nativePath.Assignments[index]
		name := strings.TrimSpace(assignment.LinuxInterface)
		if name == "" {
			continue
		}
		entry, recorded := byName[name]
		recordedMode := ""
		if recorded {
			recordedMode = takeoverModeFromDriver(entry.Mode)
			if !isNativeTakeoverMode(recordedMode) || pciMismatch(inventoryPCI(name), entry.PCIAddress) {
				// DPDK proofs are handled by the ownership/crash-guard path; a
				// proof for a different PCI device is not this port's.
				recorded = false
				recordedMode = ""
			}
		}
		if recorded {
			switch strings.ToLower(strings.TrimSpace(entry.Verification)) {
			case "verified", "pending_link":
			default:
				recorded = false
				recordedMode = ""
			}
		}
		ruleDriver := firstNonEmptyTrimmed(server.portRuleDriver(ctx, name, assignment), portKernelDriver(name))
		if recorded && recordedMode != "" && !takeover.ModeAllowed(ruleDriver, portVendorDevice(name), takeover.Mode(recordedMode)) {
			// A stale "af_xdp_copy verified" entry must not become a synthetic
			// candidate. The rule library is the policy; the file is a hint.
			recordStaleModeDropped(name, firstNonEmptyTrimmed(entry.PCIAddress, inventoryPCI(name)), ruleDriver, recordedMode, "dpdk_verification", "the rule library forbids this mode; it cannot drive selection")
			dropForbiddenModeRecords(name, entry.PCIAddress, ruleDriver, portVendorDevice(name))
			recorded = false
			recordedMode = ""
		}
		live := server.observeNativeAttachment(ctx, name, "lyroute-"+name)
		mode := recordedMode
		source := "recorded"
		if live.present() && !live.ambiguous() && live.mode() != "" {
			// The live plugin always wins for reconciliation. It never wins over
			// the rule library: a live AF_XDP attachment on an igc port is a
			// residue that migrateForbiddenTakeoverModes repairs, not a mode to
			// copy into the next generation.
			liveMode := live.mode()
			if !takeover.ModeAllowed(ruleDriver, portVendorDevice(name), takeover.Mode(liveMode)) {
				recordStaleModeDropped(name, inventoryPCI(name), ruleDriver, liveMode, "live_attachment", "the rule library forbids the live mode; selection must not keep it")
				log.Printf("dataplane selection: %s is live on forbidden %s (%s); it will be migrated, not reselected", name, takeoverModeLabel(liveMode), live.summary())
				continue
			}
			mode = liveMode
			source = "live"
			if recorded && recordedMode != mode {
				log.Printf("dataplane selection: %s recorded %s but the live attachment is %s (%s); trusting the live readback", name, takeoverModeLabel(recordedMode), takeoverModeLabel(mode), live.summary())
				server.persistLiveVerification(ctx, takeoverSnapshot{LinuxInterface: name, Netdev: name, VPPInterface: "lyroute-" + name, PCIAddress: inventoryPCI(name)}, mode, "live_reconcile")
			}
		} else if live.ambiguous() {
			log.Printf("dataplane selection: %s has an ambiguous attachment (%s); no native candidate is forced until the switch repairs it", name, live.summary())
			continue
		}
		if mode == "" {
			continue
		}
		if !takeover.ModeAllowed(ruleDriver, portVendorDevice(name), takeover.Mode(mode)) {
			recordStaleModeDropped(name, inventoryPCI(name), ruleDriver, mode, "selection", "the rule library forbids this mode")
			continue
		}
		proof := vpp.CapabilityProof{
			Tier:            vpp.DataplaneTierNative,
			Hook:            vppNativeHookForTakeover(mode),
			Mode:            vppNativeModeForTakeover(mode),
			Source:          vpp.ProofSourceActiveRuntimeReadback,
			RuntimeVerified: true,
			Native:          true,
			HighPerformance: mode != takeoverModeAFPacket,
			Verification:    "verified",
			VPPInterface:    "lyroute-" + name,
			LinkState:       netdevLinkState(name),
			HardwareLink:    netdevLinkState(name),
			LinkSpeedMbps:   entry.LinkSpeedMbps,
			MACAddress:      netdevMAC(name),
			PCIAddress:      inventoryPCI(name),
			KernelDriver:    inventoryDriver(name),
			AcceptanceOnly:  mode == takeoverModeAFPacket,
			ObservedAt:      now.Add(-time.Second),
			ValidUntil:      now.Add(time.Hour),
		}
		assignment.Candidates = []vpp.CapabilityProof{proof}
		assignment.Proof = proof
		if mode == takeoverModeAFPacket {
			// The compatibility mode is already carrying traffic. Keeping it is
			// not a new fallback consent decision; dropping the policy consent
			// must not silently migrate the port to another mode.
			assignment.AFPacketConsent = true
		}
		log.Printf("dataplane selection: keeping the %s attachment for %s (source=%s); no other mode will be created", mode, name, source)
	}
}

// applyTakeoverSwitch executes one automatic mode switch and journals it. The
// caller decides whether the returned error is fatal; a failed switch with a
// successful rollback leaves the port where it was.
func (server *Server) applyTakeoverSwitch(ctx context.Context, snapshot takeoverSnapshot, mode string, confirmed bool) error {
	// A switch owns the port for its whole duration. The watcher, the apply
	// rollback safety net and a recovery must never mutate the same attachment
	// concurrently: that interleaving is what produced duplicate interfaces and
	// a "Cannot find device" storm on a live appliance.
	nativeSwitchMu.Lock()
	defer nativeSwitchMu.Unlock()
	if takeoverModeDestructive(mode) && strings.TrimSpace(snapshot.CurrentMode) == mode && snapshot.ModeConfirmed {
		// The device is already bound to the target transport but VPP has no
		// interface for it. A restart with a managed DPDK block that no longer
		// listed this PCI leaves exactly that state; planning a no-change
		// transition would report success while the port stays down. Re-attach
		// it through the same transactional bind + readback instead.
		if vppInterfaceForPCI(ctx, snapshot.PCIAddress) == "" {
			return server.reattachDPDKAttachment(ctx, snapshot, mode)
		}
	}
	capability := server.takeoverCapabilityForMode(snapshot.LinuxInterface, snapshot.PCIAddress, mode, loadTakeoverCapabilitiesQuietly())
	transition, err := planTakeoverTransition(snapshot, mode, true, confirmed, capability, serverNow(server))
	if err != nil {
		if journalErr := recordTakeoverTransition(transition); journalErr != nil {
			log.Printf("record takeover decision for %s: %v", snapshot.LinuxInterface, journalErr)
		}
		return err
	}
	if transition.Status == takeoverStatusNoChange || transition.Status == takeoverStatusKept {
		if err := recordTakeoverTransition(transition); err != nil {
			log.Printf("record takeover decision for %s: %v", snapshot.LinuxInterface, err)
		}
		return nil
	}
	log.Printf("dataplane takeover switch: %s %s -> %s (rollback_guaranteed=%t, driver=%s, capability=%s, probe=%t)", snapshot.LinuxInterface, takeoverModeLabel(transition.FromMode), takeoverModeLabel(transition.ToMode), transition.RollbackOK, transition.ToDriver, transition.Capability, transition.ProbeRequired)
	execErr := executeTakeoverTransition(ctx, &transition, systemTakeoverExecutor{server: server})
	if transition.ProbeRequired {
		log.Printf("dataplane takeover probe: %s %s -> %s capability_unproven_probe=true result=%s rollback=%s", snapshot.LinuxInterface, takeoverModeLabel(transition.FromMode), takeoverModeLabel(transition.ToMode), transition.ProbeResult, transition.RollbackResult)
	}
	netdev := firstNonEmptyTrimmed(snapshot.Netdev, snapshot.LinuxInterface)
	vppInterface := firstNonEmptyTrimmed(snapshot.VPPInterface, "lyroute-"+netdev)
	switch {
	case execErr == nil && takeoverModeDestructive(transition.ToMode):
		server.recordTakeoverProbeSuccess(snapshot.PCIAddress, firstNonEmptyTrimmed(snapshot.KernelDriver, recordedKernelDriver(snapshot.PCIAddress)), "automatic takeover to "+transition.ToMode+" verified")
		// Record the DPDK mode itself, not the previous AF_PACKET proof.
		server.persistDestructiveVerification(ctx, snapshot, transition.ToMode, "switch_readback")
		// Keep the port in the operator inventory so the boot preflight re-binds
		// it instead of releasing it to the kernel after a reboot.
		server.persistRulePreferredInventory(ctx, []string{snapshot.LinuxInterface})
	case execErr == nil:
		// Native modes are proven by the switch readback itself. The proof is
		// written from the live attachment, never from the requested policy: an
		// af_xdp request that ended as af_packet must not be recorded as af_xdp
		// "verified".
		if !server.persistLiveVerification(ctx, snapshot, transition.ToMode, "switch_readback") {
			transition.NeedsRepair = true
			transition.Failures = append(transition.Failures, "the switch reported success but the live readback does not confirm "+takeoverModeLabel(transition.ToMode))
			recordNativeRepair(transition, "live readback after the switch does not confirm the requested mode", executorLiveSummary(server, ctx, netdev, vppInterface))
			dropDPDKVerification(snapshot.LinuxInterface)
		} else {
			clearNativeRepair(snapshot.LinuxInterface)
		}
	case transition.RollbackResult == takeoverRollbackRestored:
		// The port is back on its previous attachment: the verification file
		// must say so. Leaving the failed target's proof behind is what made the
		// next apply insist on a mode the device does not carry.
		if !takeoverModeDestructive(transition.FromMode) {
			if !server.persistLiveVerification(ctx, snapshot, transition.FromMode, "switch_rollback") {
				dropDPDKVerification(snapshot.LinuxInterface)
				transition.NeedsRepair = true
				recordNativeRepair(transition, "rollback reported restored but the live readback does not confirm "+takeoverModeLabel(transition.FromMode), executorLiveSummary(server, ctx, netdev, vppInterface))
			}
		}
	case transition.RollbackResult == takeoverRollbackRestoreFailed:
		// A failed rollback leaves the attachment unknown. Drop the stale proof
		// so the status cannot claim either the old or the requested mode, and
		// record exactly what a human has to repair.
		dropDPDKVerification(snapshot.LinuxInterface)
		transition.NeedsRepair = true
		recordNativeRepair(transition, firstNonEmptyTrimmed(transition.RollbackDetail, "takeover rollback failed"), executorLiveSummary(server, ctx, netdev, vppInterface))
	}
	switch {
	case execErr == nil && takeoverModeDestructive(transition.ToMode):
		// The rule-preferred mode is live: clear the one-shot failure marker.
		recordDPDKAttemptSucceeded(snapshot.LinuxInterface, snapshot.PCIAddress, transition.ToMode)
	case execErr != nil && takeoverModeDestructive(transition.ToMode):
		// A failed DPDK attempt is never replayed automatically. The port has
		// already been rolled back to the previous attachment (AF_PACKET on
		// the migration path); the marker is what keeps the watcher and a
		// later apply from restarting VPP for the same verdict again.
		recordDPDKAttemptFailed(snapshot.LinuxInterface, snapshot.PCIAddress, transition.ToMode,
			firstLine(execErr.Error()+" "+transition.RollbackDetail, 240))
	}
	if recordErr := recordTakeoverTransition(transition); recordErr != nil {
		log.Printf("record takeover transition for %s: %v", snapshot.LinuxInterface, recordErr)
	}
	if execErr != nil && transition.RollbackResult == takeoverRollbackRestoreFailed {
		// A switch that could not be undone must not be retried in a loop.
		blockDataplaneBootstrap("takeover rollback failed for " + snapshot.LinuxInterface)
	}
	return execErr
}

// persistDestructiveVerification writes the proof for a DPDK attachment. The
// generic persistLiveVerification only looks for the product-named interface
// (lyroute-<netdev>), which a DPDK port never has, so every DPDK switch used to
// leave the previous AF_PACKET record in place. The proof is built from the VPP
// hardware table: interface present, no device error, and the PHY carrier.
func (server *Server) persistDestructiveVerification(ctx context.Context, snapshot takeoverSnapshot, mode, method string) bool {
	mode = strings.TrimSpace(mode)
	pci := strings.TrimSpace(snapshot.PCIAddress)
	if mode == "" || pci == "" {
		return false
	}
	vppName := vppInterfaceForPCI(ctx, pci)
	if vppName == "" {
		log.Printf("dataplane switch: refusing to persist %s verification for %s: VPP has no interface for %s", takeoverModeLabel(mode), snapshot.LinuxInterface, pci)
		return false
	}
	hardware, err := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "hardware-interfaces", vppName)
	if err != nil && strings.TrimSpace(hardware) == "" {
		log.Printf("dataplane switch: refusing to persist %s verification for %s: %v", takeoverModeLabel(mode), snapshot.LinuxInterface, err)
		return false
	}
	if deviceError := vppHardwareDeviceError(hardware); deviceError != "" {
		log.Printf("dataplane switch: refusing to persist %s verification for %s: VPP reports a device error (%s)", takeoverModeLabel(mode), snapshot.LinuxInterface, deviceError)
		return false
	}
	link := firstNonEmptyTrimmed(vppHardwareCarrier(hardware), "unknown")
	if link != "up" {
		// An interface that was just brought administratively up reports
		// "carrier down" for a few seconds until the PMD refreshes the PHY
		// state. Wait (bounded) for that refresh instead of persisting a
		// false pending_link for a port that is actually carrying traffic.
		interfaceState, _ := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "interface")
		if vppInterfaceState(interfaceState, vppName) == "up" {
			deadline := time.Now().Add(nativeSettleWindow)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
				refreshed, err := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "hardware-interfaces", vppName)
				if err != nil && strings.TrimSpace(refreshed) == "" {
					continue
				}
				if carrier := vppHardwareCarrier(refreshed); carrier == "up" {
					link = carrier
					break
				}
			}
		}
	}
	verification := "pending_link"
	if link == "up" {
		verification = "verified"
	}
	entry := dataplane.DPDKVerification{
		LinuxInterface: snapshot.LinuxInterface,
		PCIAddress:     pci,
		// The verification file records the binding driver (uio_pci_generic),
		// not the operator mode name (dpdk_uio); every reader maps it back
		// through takeoverModeFromDriver.
		Mode:               firstNonEmptyTrimmed(takeoverModeDriver(mode), mode),
		FunctionalVerified: verification == "verified",
		Verification:       verification,
		Method:             method,
		LinkState:          link,
		HardwareLink:       link,
		ObservedAt:         time.Now().UTC(),
	}
	if err := mergeDPDKVerification(dataplaneVerificationPath(), []dataplane.DPDKVerification{entry}); err != nil {
		log.Printf("persist %s verification for %s: %v", takeoverModeLabel(mode), snapshot.LinuxInterface, err)
		return false
	}
	return true
}

// vppInterfaceForPCI returns the VPP interface that owns a DPDK PCI device, or
// "" when VPP has no attachment for it. DPDK interfaces are named after the
// driver (GigabitEthernet4/0/0), never after the Linux netdev, so the product
// name can only be derived from the hardware table.
func vppInterfaceForPCI(ctx context.Context, pci string) string {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return ""
	}
	output, err := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "hardware-interfaces")
	if err != nil {
		return ""
	}
	return findVPPInterfaceByPCI(output, pci)
}

// reattachDPDKAttachment restores a DPDK port that is bound to uio/vfio but is
// missing from VPP: the kernel already cannot serve it, so the only safe repair
// is the transactional bind (which rewrites the managed DPDK block) followed by
// a VPP start and a hardware-table readback. The caller holds nativeSwitchMu.
func (server *Server) reattachDPDKAttachment(ctx context.Context, snapshot takeoverSnapshot, mode string) error {
	transition := takeoverTransition{
		SchemaVersion: 1,
		Interface:     snapshot.LinuxInterface,
		PCIAddress:    snapshot.PCIAddress,
		Netdev:        snapshot.Netdev,
		KernelDriver:  snapshot.KernelDriver,
		VPPInterface:  snapshot.VPPInterface,
		FromMode:      mode,
		ToMode:        mode,
		ToDriver:      takeoverModeDriver(mode),
		Status:        takeoverStatusPlanned,
		Reason:        "the device is bound to " + takeoverModeDriver(mode) + " but VPP has no interface for it; re-attaching",
		RollbackOK:    true,
		StartedAt:     time.Now().UTC(),
		Steps: []takeoverStep{
			{Kind: "bind", Mode: mode, Detail: "re-attach the bound DPDK device", Destructive: true},
			{Kind: "verify", Mode: mode, Detail: "verify the re-attached DPDK device with a VPP readback"},
		},
	}
	log.Printf("dataplane takeover: %s (%s) is bound to %s but VPP has no interface for it; re-attaching", snapshot.LinuxInterface, snapshot.PCIAddress, takeoverModeDriver(mode))
	execErr := executeTakeoverTransition(ctx, &transition, systemTakeoverExecutor{server: server})
	if execErr == nil {
		if !server.persistDestructiveVerification(ctx, snapshot, mode, "switch_readback") {
			transition.NeedsRepair = true
			transition.Failures = append(transition.Failures, "the re-attached device was not confirmed by the live readback")
			recordNativeRepair(transition, "the DPDK re-attach was not confirmed by the live readback", executorLiveSummary(server, ctx, snapshot.Netdev, snapshot.VPPInterface))
		} else {
			recordDPDKAttemptSucceeded(snapshot.LinuxInterface, snapshot.PCIAddress, mode)
			server.persistRulePreferredInventory(ctx, []string{snapshot.LinuxInterface})
		}
	}
	if recordErr := recordTakeoverTransition(transition); recordErr != nil {
		log.Printf("record takeover transition for %s: %v", snapshot.LinuxInterface, recordErr)
	}
	return execErr
}

func executorLiveSummary(server *Server, ctx context.Context, netdev, vppInterface string) string {
	return server.observeNativeAttachment(ctx, netdev, vppInterface).summary()
}

// nativeSettleWindow bounds how long a switch waits for the driver to report
// the state that only becomes visible after the first transmit attempt.
const nativeSettleWindow = 3 * time.Second

// awaitNativeAttachmentSettled waits up to nativeSettleWindow for the new
// attachment to be free of VPP error state and, when the link was up before the
// switch, to carry again. A copy-mode AF_XDP socket on a NIC without a usable
// generic TX path passes the immediate readback and only then reports
// "tx poll() failed: Device or resource busy", so the immediate readback alone
// would accept a dead port.
func (executor systemTakeoverExecutor) awaitNativeAttachmentSettled(ctx context.Context, transition *takeoverTransition, netdev, vppInterface string, live *nativeAttachmentLive) error {
	deadline := time.Now().Add(nativeSettleWindow)
	for {
		if next := executor.server.observeNativeAttachment(ctx, netdev, vppInterface); next.Reachable {
			*live = next
		}
		if live.DeviceError != "" {
			return &takeoverHardwareError{Interface: transition.Interface, Mode: transition.ToMode, Reason: live.DeviceError}
		}
		if !transition.LinkWasUp || netdev == "" || netdevLinkState(netdev) == "up" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("netdev %s lost its carrier after switching to %s (was up before the switch)", netdev, takeoverModeLabel(transition.ToMode))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func loadTakeoverCapabilitiesQuietly() dataplane.TakeoverCapabilityState {
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	return capabilities
}

// recordTakeoverFirstBindSuccess journals the first takeover of a port (old
// mode empty). It captures the runtime handles observed after verification so
// a later switch has the previous mode, driver and handles on record.
func (server *Server) recordTakeoverFirstBindSuccess(ctx context.Context, entry dataplane.DPDKVerification) {
	toMode := takeoverModeFromDriver(entry.Mode)
	snapshot := server.detectTakeoverSnapshot(ctx, entry.LinuxInterface, nil)
	if toMode == "" {
		toMode = snapshot.CurrentMode
	}
	transition := takeoverTransition{
		SchemaVersion:  1,
		Interface:      entry.LinuxInterface,
		PCIAddress:     entry.PCIAddress,
		Netdev:         entry.LinuxInterface,
		KernelDriver:   recordedKernelDriver(entry.PCIAddress),
		VPPInterface:   "lyroute-" + entry.LinuxInterface,
		FromMode:       "",
		ToMode:         toMode,
		FromDriver:     "",
		ToDriver:       takeoverModeDriver(toMode),
		SWIfIndex:      snapshot.SWIfIndex,
		QueueFDs:       append([]int(nil), snapshot.QueueFDs...),
		XDPProgram:     snapshot.XDPProgram,
		Capability:     string(vpp.CapabilitySupported),
		RollbackOK:     true,
		Status:         takeoverStatusApplied,
		Reason:         "first takeover verified",
		RollbackResult: takeoverRollbackNotNeeded,
		StartedAt:      entry.ObservedAt,
		FinishedAt:     time.Now().UTC(),
	}
	recordDPDKAttemptSucceeded(entry.LinuxInterface, entry.PCIAddress, toMode)
	server.persistRulePreferredInventory(ctx, []string{entry.LinuxInterface})
	if err := recordTakeoverTransition(transition); err != nil {
		log.Printf("record first takeover for %s: %v", entry.LinuxInterface, err)
	}
}

// recordTakeoverFirstBindFailures journals a rejected first takeover and the
// kernel-driver restore that actually happened for each device.
func (server *Server) recordTakeoverFirstBindFailures(failures []dpdkTakeoverFailure, released map[string]bool) {
	for _, failure := range failures {
		recordDPDKAttemptFailed(failure.LinuxInterface, failure.PCIAddress, failure.Mode, failure.Reason)
		status := takeoverStatusFailed
		rollback := takeoverRollbackNotAttempted
		detail := ""
		if ok, attempted := released[failure.LinuxInterface]; attempted {
			if ok {
				status = takeoverStatusRolledBack
				rollback = takeoverRollbackRestored
				detail = "the device was returned to its kernel driver"
			} else {
				rollback = takeoverRollbackRestoreFailed
				detail = "the kernel driver could not be restored; the port may stay offline"
			}
		}
		transition := takeoverTransition{
			SchemaVersion:  1,
			Interface:      failure.LinuxInterface,
			PCIAddress:     failure.PCIAddress,
			Netdev:         failure.LinuxInterface,
			KernelDriver:   recordedKernelDriver(failure.PCIAddress),
			VPPInterface:   "lyroute-" + failure.LinuxInterface,
			FromMode:       "",
			ToMode:         failure.Mode,
			ToDriver:       takeoverModeDriver(failure.Mode),
			Capability:     string(vpp.CapabilitySupported),
			Status:         status,
			Reason:         failure.Reason,
			RollbackResult: rollback,
			RollbackDetail: detail,
			Failures:       []string{failure.Reason},
			StartedAt:      time.Now().UTC(),
			FinishedAt:     time.Now().UTC(),
		}
		if err := recordTakeoverTransition(transition); err != nil {
			log.Printf("record failed first takeover for %s: %v", failure.LinuxInterface, err)
		}
	}
}
