package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
	"ly-route/backend/internal/runtime/vpp"
)

var dataplaneWatcherLast = map[string]time.Time{}

// dataplaneSwitchRetryAfter keeps a failing explicit switch from restarting VPP
// every watcher tick. A switch that fails and rolls back is expensive
// (stop/start) and must not become a restart loop.
var dataplaneSwitchRetryAfter = map[string]time.Time{}

const dataplaneSwitchFailureCooldown = 2 * time.Minute

// dataplaneSwitchHardwareCooldown is used when the device itself cannot do the
// requested mode (VPP reported a device error on the new attachment). Retrying
// such a switch every two minutes would blip the port forever.
const dataplaneSwitchHardwareCooldown = 30 * time.Minute

// switchFailureCooldown picks the retry delay for a failed switch.
func switchFailureCooldown(err error) time.Duration {
	var hardware *takeoverHardwareError
	if errors.As(err, &hardware) {
		return dataplaneSwitchHardwareCooldown
	}
	return dataplaneSwitchFailureCooldown
}

// StartDataplaneWatcher is the bounded auto-adoption watcher. It is the only
// component that performs a takeover without a new apply request: it reacts to
// link up, a new PCI device and a new interface on a role-assigned port.
// It never binds the management port, never retries a crash-guarded device,
// and never demotes ownership because a cable is unplugged.
func (server *Server) StartDataplaneWatcher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				server.watchDataplaneOnce(ctx)
			}
		}
	}()
}

// enforceDataplaneForwardingGuard keeps the Linux stack out of the transit
// path. VPP owns every role port through AF_PACKET, but the kernel still
// receives a copy of each frame on the raw netdev. With ip_forward=1 and
// connected routes on the LCP taps, Linux forwarded that copy a second time
// and every inter-VLAN packet was delivered twice. Disable forwarding on the
// VPP-owned netdevs and their LCP peers; the watcher reasserts it after boot,
// after a link event and whenever a new role port appears.
func (server *Server) enforceDataplaneForwardingGuard(ctx context.Context) {
	for _, rawName := range server.runtimeDataInterfaces(ctx) {
		name := strings.TrimSpace(rawName)
		if name == "" || name == server.managementInterfaceID(ctx) {
			continue
		}
		disableLinuxForwarding(name)
		if lcp := vpp.LANControlPlaneHostInterface(name); lcp != name {
			disableLinuxForwarding(lcp)
		}
	}
}

func disableLinuxForwarding(name string) {
	for _, family := range []string{"ipv4", "ipv6"} {
		path := "/proc/sys/net/" + family + "/conf/" + name + "/forwarding"
		_ = os.WriteFile(path, []byte("0\n"), 0o644)
	}
}

// dpdkAdminUpCooldown throttles the boot-time admin-state repair. VPP starts
// with every DPDK port administratively down, and nothing in the boot path
// raises it when the committed generation is already unchanged, so the UI
// showed every data port as "接口 DOWN" even with the cable connected.
var (
	dpdkAdminUpMu   sync.Mutex
	dpdkAdminUpSeen = map[string]time.Time{}
)

func (server *Server) ensureDPDKInterfacesAdminUp(ctx context.Context) {
	ownership, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath())
	if err != nil {
		return
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	for _, device := range ownership.Devices {
		name := strings.TrimSpace(device.LinuxInterface)
		if name == "" {
			continue
		}
		// The boot path attaches DPDK ports under the PMD name
		// (GigabitEthernetX/0/0) and only a later readback renames them.
		// Resolve the live name by PCI instead of assuming lyroute-<netdev>.
		snapshot, ok := server.vppLinkSnapshotFor(ctx, name, device.PCIAddress)
		if !ok || strings.TrimSpace(snapshot.Interface) == "" {
			continue
		}
		vppName := snapshot.Interface
		dpdkAdminUpMu.Lock()
		last := dpdkAdminUpSeen[vppName]
		if !last.IsZero() && time.Since(last) < 5*time.Minute {
			dpdkAdminUpMu.Unlock()
			continue
		}
		dpdkAdminUpSeen[vppName] = time.Now()
		dpdkAdminUpMu.Unlock()
		output, err := execCommandOutput(ctx, vppctl, "show", "interface", vppName)
		if err != nil || !vppCLIInterfacePresent(output, vppName) {
			continue
		}
		if !strings.Contains(output, " down ") {
			continue
		}
		if _, err := execCommandOutput(ctx, vppctl, "set", "interface", "state", vppName, "up"); err != nil {
			log.Printf("dataplane watcher: could not set %s up: %v", vppName, err)
			dpdkAdminUpMu.Lock()
			delete(dpdkAdminUpSeen, vppName)
			dpdkAdminUpMu.Unlock()
			continue
		}
		log.Printf("dataplane watcher: restored admin state for %s after boot", vppName)
	}
}

func (server *Server) watchDataplaneOnce(ctx context.Context) {
	server.ensureDPDKInterfacesAdminUp(ctx)
	server.enforceDataplaneForwardingGuard(ctx)
	// An apply owns the dataplane transition. Letting the watcher adopt a port
	// while the apply is establishing the crash fallback would create the same
	// VPP interface twice and can turn a successful fallback into a rollback.
	if state := server.applyState(); state.InProgress {
		return
	}
	// Repair a live attachment the rule library forbids (an igc AF_XDP residue)
	// before the normal adoption loop can re-create it. The migration is
	// one-shot per boot per port and falls back to AF_PACKET, never DPDK.
	server.migrateForbiddenTakeoverModes(ctx)
	interfaces := server.takeoverDataInterfaces(ctx)
	if len(interfaces) == 0 {
		return
	}
	vppNames := server.vppInterfaceNames(ctx)
	verificationState, _ := dataplane.LoadDPDKVerification(dataplaneVerificationPath())
	verification := map[string]int{}
	for index, entry := range verificationState.Interfaces {
		verification[strings.TrimSpace(entry.LinuxInterface)] = index
	}
	ownership := map[string]dataplane.OwnershipDevice{}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, entry := range state.Devices {
			ownership[strings.TrimSpace(entry.LinuxInterface)] = entry
		}
	}
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
	updates := map[string]dataplane.DPDKVerification{}
	bootstrapNeeded := false
	for _, rawName := range interfaces {
		name := strings.TrimSpace(rawName)
		if name == "" || name == server.managementInterfaceID(ctx) {
			continue
		}
		item, ok := readInventoryInterface(name)
		if !ok {
			continue
		}
		index, recorded := verification[name]
		entry := dataplane.DPDKVerification{LinuxInterface: name, PCIAddress: item.PCIAddress}
		if recorded {
			entry = verificationState.Interfaces[index]
		}
		owned := recorded || vppNames["lyroute-"+name]
		if owner, ok := ownership[name]; ok {
			owned = true
			if entry.Mode == "" {
				entry.Mode = owner.Transport
			}
		}
		if !owned {
			if active, ok := activeDataplaneMode(name); ok && active != "" {
				owned = true
				if entry.Mode == "" {
					entry.Mode = active
				}
			}
		}
		if !owned {
			if proof, ok := server.validatedDPDKOwnershipProof(name, item.PCIAddress); ok {
				owned = true
				if entry.Mode == "" {
					entry.Mode = proof.Mode
				}
				log.Printf("dataplane watcher: %s (%s) trusted %s ownership proof; reusing the bound device without a probe", name, item.PCIAddress, proof.Source)
				server.markTakeoverGuardSupported(item.PCIAddress, item.KernelDriver, "trusted "+proof.Source+" ownership")
			}
		}
		if entry.Mode != "" && isNativeTakeoverMode(entry.Mode) && !takeover.ModeAllowed(firstNonEmptyTrimmed(item.KernelDriver, portKernelDriver(name)), portVendorDevice(name), takeover.Mode(entry.Mode)) {
			recordStaleModeDropped(name, item.PCIAddress, item.KernelDriver, entry.Mode, "watcher_verification", "the rule library forbids this recorded mode")
			dropForbiddenModeRecords(name, item.PCIAddress, item.KernelDriver, portVendorDevice(name))
			entry.Mode = ""
			entry.Verification = ""
			entry.FunctionalVerified = false
			recorded = false
		}
		if entry.Mode == "" && vppNames["lyroute-"+name] {
			// Read the plugin that really owns the interface instead of assuming
			// AF_PACKET: the assumption rewrote a live AF_XDP port as af_packet
			// on every tick, and the next apply then asked for the wrong mode.
			if live := server.observeNativeAttachment(ctx, name, "lyroute-"+name); live.present() && !live.ambiguous() && live.mode() != "" {
				if takeover.ModeAllowed(firstNonEmptyTrimmed(item.KernelDriver, portKernelDriver(name)), portVendorDevice(name), takeover.Mode(live.mode())) {
					entry.Mode = live.mode()
				} else {
					recordStaleModeDropped(name, item.PCIAddress, item.KernelDriver, live.mode(), "watcher_live", "the rule library forbids the live mode")
				}
			}
		}
		if isNativeTakeoverMode(entry.Mode) && vppNames["lyroute-"+name] {
			// The live plugin is authoritative. A recorded mode that disagrees
			// is stale (a failed switch, a watcher rewrite or an older image)
			// and must be corrected here, not carried into the next apply.
			if live := server.observeNativeAttachment(ctx, name, "lyroute-"+name); live.present() && !live.ambiguous() && live.mode() != "" && live.mode() != entry.Mode {
				if takeover.ModeAllowed(firstNonEmptyTrimmed(item.KernelDriver, portKernelDriver(name)), portVendorDevice(name), takeover.Mode(live.mode())) {
					log.Printf("dataplane watcher: %s recorded %s but the live attachment is %s; correcting the verification record", name, takeoverModeLabel(entry.Mode), takeoverModeLabel(live.mode()))
					entry.Mode = live.mode()
					entry.Verification = "pending_link"
					entry.FunctionalVerified = false
					entry.ObservedAt = time.Now().UTC()
					updates[name] = entry
				} else {
					recordStaleModeDropped(name, item.PCIAddress, item.KernelDriver, live.mode(), "watcher_live", "the rule library forbids the live mode")
				}
			}
		}
		if isNativeTakeoverMode(entry.Mode) && !vppNames["lyroute-"+name] {
			// VPP restarted (or the attachment was deleted) and lost the
			// non-destructive interface while the ownership record survived.
			// Re-create it without waiting for an apply. The kernel netdev is
			// intact; this is not a destructive re-bind.
			last := dataplaneWatcherLast[name]
			if time.Since(last) < 30*time.Second {
				continue
			}
			dataplaneWatcherLast[name] = time.Now()
			if adopted, ok := server.restoreNativeAttachment(ctx, name, entry); ok {
				updates[name] = adopted
				log.Printf("dataplane watcher: %s re-adopted via %s after VPP lost the attachment", name, adopted.Mode)
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_adopt", "success", "mode="+adopted.Mode, "")
			}
			continue
		}
		link := netdevLinkState(name)
		if isDPDKVerificationMode(entry.Mode) && vppNames["lyroute-"+name] {
			// A DPDK port has no kernel netdev, so netdevLinkState reports
			// "unknown" forever. Read the PHY carrier from VPP instead: the
			// persisted proof used to keep hardware_link: down from the apply
			// moment long after both ports were up.
			if carrier := vppCarrierState(ctx, "lyroute-"+name); carrier != "" {
				link = carrier
			}
		}
		if link == "unknown" && entry.LinkState != "" {
			link = normalizeLinkState(entry.LinkState)
		}
		if owned {
			if isDPDKVerificationMode(entry.Mode) {
				// Self-heal an upgrade from an image that did not record the
				// data ports: a committed DPDK port must be in the operator
				// inventory or the next boot preflight releases it to Linux.
				server.persistRulePreferredInventory(ctx, []string{name})
			}
			// The backend plan decides whether a healthy port should be
			// upgraded. There is no operator switch to honour.
			if target, upgrade := server.automaticTakeoverUpgrade(ctx, name, takeoverModeFromDriver(entry.Mode), capabilities); upgrade {
				if retryAfter, blocked := dataplaneSwitchRetryAfter[name]; blocked && time.Now().Before(retryAfter) {
					continue
				}
				if time.Since(dataplaneWatcherLast[name]) > 30*time.Second {
					dataplaneWatcherLast[name] = time.Now()
					if !claimAutomaticModeSwitch(name) {
						// The one automatic switch for this boot was already spent
						// (a failed repair or an earlier attempt). Never replay it.
						continue
					}
					snapshot := server.detectTakeoverSnapshot(ctx, name, vppNames)
					if !snapshot.ModeConfirmed {
						// Never drive a destructive switch from an inferred
						// mode. The status layer reports it as stale; an apply
						// or the automatic probe must establish the real mode.
						log.Printf("dataplane watcher: %s upgrade to %s deferred: the current mode is not confirmed", name, target)
						continue
					}
					if err := server.applyTakeoverSwitch(ctx, snapshot, target, true); err != nil {
						log.Printf("dataplane watcher: %s upgrade %s -> %s failed: %v", name, takeoverModeLabel(snapshot.CurrentMode), target, err)
						dataplaneSwitchRetryAfter[name] = time.Now().Add(switchFailureCooldown(err))
						server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_upgrade", "failed", "target="+target, "")
					} else {
						delete(dataplaneSwitchRetryAfter, name)
						log.Printf("dataplane watcher: %s upgraded %s -> %s", name, takeoverModeLabel(snapshot.CurrentMode), target)
						server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_upgrade", "success", "target="+target, "")
					}
				}
				continue
			}
			if link == "down" {
				if updated, changed := demoteOnLinkDown(entry, link); changed {
					updates[name] = updated
					log.Printf("dataplane watcher: %s link down; takeover retained, verification=pending_link", name)
					server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.pending_link", "success", "", "")
				}
			}
			if link == "up" && entry.Verification == "verified" && entry.HardwareLink != "up" {
				// Refresh a proof written while the PHY had not finished
				// initialising (the apply moment); the carrier is up now.
				entry.LinkState = "up"
				entry.HardwareLink = "up"
				entry.ObservedAt = time.Now().UTC()
				updates[name] = entry
			}
			if link == "up" && entry.Verification != "verified" {
				probed := server.functionalProbe(ctx, name, entry, vppNames)
				if probed.Verification == "failed" {
					// The crash guard already removed the proof and restored
					// the kernel driver; do not resurrect a failed DPDK owner.
					continue
				}
				if probed.Verification != strings.TrimSpace(entry.Verification) || probed.FunctionalVerified != entry.FunctionalVerified || probed.LinkState != entry.LinkState {
					updates[name] = probed
					if probed.Verification == "verified" {
						log.Printf("dataplane watcher: %s link up; promoted to verified via %s", name, probed.Method)
						server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.verified", "success", "", "")
					}
				}
			}
			continue
		}
		// Auto-adopt only role-assigned ports (runtimeDataInterfaces already
		// filters to configured LAN/WAN/internal/external roles).
		last := dataplaneWatcherLast[name]
		if time.Since(last) < 30*time.Second {
			continue
		}
		dataplaneWatcherLast[name] = time.Now()
		plan := server.takeoverPlanForPort(ctx, name)
		primary, planned := plan.Primary()
		if !planned {
			continue
		}
		if takeoverModeDestructive(string(primary.Mode)) {
			// The rule's first choice is DPDK. Only two conditions make the
			// transactional bind impossible right now: the device is
			// crash-guarded in this environment, or the first-bind tooling is
			// missing/backing off. Only then may the compatibility candidate be
			// adopted -- never before the rule-preferred attempt.
			guarded := server.takeoverGuardActive(capabilities, strings.TrimSpace(item.KernelDriver), strings.TrimSpace(item.PCIAddress), name)
			attempted, _ := dpdkAttemptFailure(item.PCIAddress, name)
			if guarded || attempted || !destructiveFirstBindAvailable() {
				for _, candidate := range plan.Candidates {
					if takeoverModeDestructive(string(candidate.Mode)) {
						continue
					}
					if entry, ok := server.adoptNativeAttachment(ctx, name, string(candidate.Mode)); ok {
						updates[name] = entry
						server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_adopt", "success", "mode="+string(candidate.Mode), "")
					}
					break
				}
				continue
			}
			bootstrapNeeded = true
			continue
		}
		if entry, ok := server.adoptNativeAttachment(ctx, name, string(primary.Mode)); ok {
			updates[name] = entry
			server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_adopt", "success", "mode="+string(primary.Mode), "")
		}
		continue
	}
	if bootstrapNeeded {
		if server.bootstrapDataplaneOwnership(ctx) {
			log.Printf("dataplane watcher: auto-adopted newly available role port(s)")
			server.recordAuditWithTransaction("system", "system", "dataplane", "dataplane.takeover.auto_adopt", "success", "bootstrap", "")
		}
	}
	if len(updates) == 0 {
		return
	}
	state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath())
	if err != nil {
		state = verificationState
	}
	for name, entry := range updates {
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
	if err := dataplane.SaveDPDKVerification(dataplaneVerificationPath(), state); err != nil {
		log.Printf("dataplane watcher: persist verification: %v", err)
	}
}

// demoteOnLinkDown keeps ownership and only weakens the proof. It never
// releases a port, never changes its mode, and never marks it unsupported.
func demoteOnLinkDown(entry dataplane.DPDKVerification, link string) (dataplane.DPDKVerification, bool) {
	if link != "down" {
		return entry, false
	}
	if entry.Verification == "pending_link" && entry.LinkState == "down" {
		return entry, false
	}
	entry.LinkState = "down"
	entry.HardwareLink = "down"
	entry.Verification = "pending_link"
	entry.FunctionalVerified = false
	entry.Reason = "閾捐矾宸叉柇寮€锛涙帴绠′繚鐣欙紝绛夊緟閾捐矾鎭㈠"
	return entry, true
}

// functionalProbe promotes pending_link to verified with a functional proof
// (TX counters when a peer is configured, otherwise a live link). It never
// performs a destructive bind and never restores a kernel driver.
func (server *Server) functionalProbe(ctx context.Context, name string, entry dataplane.DPDKVerification, vppNames map[string]bool) dataplane.DPDKVerification {
	entry.ObservedAt = time.Now().UTC()
	vppInterface := ""
	if vppNames["lyroute-"+name] {
		vppInterface = "lyroute-" + name
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	if vppInterface == "" && entry.PCIAddress != "" {
		hardware, _ := execCommandOutput(ctx, vppctl, "show", "hardware-interfaces")
		vppInterface = findVPPInterfaceByPCI(hardware, entry.PCIAddress)
	}
	entry.LinkState = netdevLinkState(name)
	if vppInterface != "" {
		// A DPDK port has no Linux netdev, so netdevLinkState reports "down"
		// (or "unknown") even when the PHY is up. Read the carrier from VPP
		// instead; otherwise the persisted proof says hardware_link: down while
		// `show hardware-interfaces` says `carrier up`, and every UI/API
		// readback disagrees with the dataplane.
		hardware, _ := execCommandOutput(ctx, vppctl, "show", "hardware-interfaces", vppInterface)
		if carrier := vppHardwareCarrier(hardware); carrier == "up" || carrier == "down" {
			entry.LinkState = carrier
		}
	}
	entry.HardwareLink = entry.LinkState
	if vppInterface == "" {
		entry.Verification = "pending_link"
		return entry
	}
	stateOutput, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface)
	state := vppInterfaceState(stateOutput, vppInterface)
	if state != "up" {
		upOutput, upErr := exec.CommandContext(ctx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
		if upErr != nil && isDPDKVerificationMode(entry.Mode) {
			// A DPDK PMD that aborts VPP on admin-up must never be retried in
			// a loop. Persist the crash guard, restore the kernel driver and
			// let the next tick fall back to a non-destructive attachment.
			reason := "set interface state up failed: " + upErr.Error() + ": " + strings.TrimSpace(string(upOutput))
			server.recordTakeoverCrashGuard([]dpdkTakeoverFailure{{
				LinuxInterface: name,
				PCIAddress:     entry.PCIAddress,
				Reason:         reason,
				Crashed:        vppDownSignature(reason),
			}})
			server.releaseFailedDPDKTakeovers(ctx, []dpdkTakeoverFailure{{LinuxInterface: name, PCIAddress: entry.PCIAddress, Reason: reason}})
			if !server.restartVPPAndWait(ctx) {
				log.Printf("dataplane watcher: VPP did not recover after %s crash; management stays on Linux", name)
			} else {
				server.refreshTakeoverGuardEnvironments([]string{entry.PCIAddress})
			}
			entry.Verification = "failed"
			entry.FunctionalVerified = false
			entry.Reason = reason
			log.Printf("dataplane watcher: %s native takeover crashed VPP; crash guard recorded and kernel driver restored", name)
			return entry
		}
		stateOutput, _ = execCommandOutput(ctx, vppctl, "show", "interface", vppInterface)
		state = vppInterfaceState(stateOutput, vppInterface)
	}
	if state != "up" {
		entry.Verification = "pending_link"
		return entry
	}
	entry.LinkState = "up"
	entry.HardwareLink = "up"
	// Functional verification must never use `vppctl ping`: on this image it
	// seeds a self-referential NAT44 session and SIGSEGVs
	// nat44_ed_out2in_node_fn. A live VPP carrier plus an admin-up interface is
	// the non-destructive proof (the same contract the apply readback uses).
	if peer := functionalPeer(name); peer != "" {
		log.Printf("dataplane watcher: ignoring legacy DPDK functional peer %s for %s; probing VPP with the CLI ping verb is forbidden on this image", peer, name)
	}
	entry.Verification = "verified"
	entry.FunctionalVerified = true
	entry.Method = "watcher_link_state_up"
	// The proof describes an attachment, so the attachment has to be live and
	// of the recorded kind. Without this readback an interface created by a
	// different plugin was promoted as "verified" for the wrong mode.
	if !isDPDKVerificationMode(entry.Mode) {
		live := server.observeNativeAttachment(ctx, name, "lyroute-"+name)
		if live.Reachable && live.present() && !live.matches(entry.Mode) {
			entry.Verification = "pending_link"
			entry.FunctionalVerified = false
			entry.Method = ""
			entry.Reason = "live attachment does not match the recorded mode: " + live.summary()
			return entry
		}
	}
	server.recordTakeoverProbeSuccess(entry.PCIAddress, recordedKernelDriver(entry.PCIAddress), "watcher link state up")
	return entry
}

func vppCarrierState(ctx context.Context, vppInterface string) string {
	vppInterface = strings.TrimSpace(vppInterface)
	if vppInterface == "" {
		return ""
	}
	output, err := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "hardware-interfaces", vppInterface)
	if err != nil {
		return ""
	}
	switch carrier := vppHardwareCarrier(output); carrier {
	case "up", "down":
		return carrier
	}
	return ""
}

func functionalPeer(name string) string {
	values := strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_FUNCTIONAL_PEERS"))
	for _, pair := range strings.Split(values, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == name {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(os.Getenv("LY_ROUTE_DPDK_FUNCTIONAL_PEER"))
}

// vppInterfacePresent parses `show interface <name>` instead of searching for
// the name as a substring: VPP prints CLI errors to stdout, and the error text
// itself contains the interface name.
func vppCLIInterfacePresent(output, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "unknown input") || strings.Contains(trimmed, "unknown interface") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func netdevMAC(name string) string {
	data, err := os.ReadFile(filepath.Join(sysfsRoot(), "class/net", name, "address"))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(data)))
}

func isDPDKVerificationMode(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "vfio-pci", "vfio", "vfio_pci", "uio_pci_generic", "uio":
		return true
	}
	return false
}

func isNativeTakeoverMode(mode string) bool {
	switch mode {
	case "af_packet", "af_xdp_zero_copy", "af_xdp_copy", "rdma":
		return true
	}
	return false
}

// restoreNativeAttachment re-creates the VPP interface for a non-destructive
// takeover that survived in the ownership record but disappeared from VPP
// (for example after a VPP restart). The kernel netdev is untouched. The mode
// comes from the record because nothing is attached to observe.
func (server *Server) restoreNativeAttachment(ctx context.Context, name string, entry dataplane.DPDKVerification) (dataplane.DPDKVerification, bool) {
	if mode := takeoverModeFromDriver(entry.Mode); mode != "" && !takeoverModeDestructive(mode) {
		return server.adoptNativeAttachment(ctx, name, mode)
	}
	return dataplane.DPDKVerification{}, false
}

// adoptNativeAttachment rebuilds a native attachment under the single switch
// owner. A second owner (an apply that is switching the same port) wins: this
// call is skipped, never interleaved.
func (server *Server) adoptNativeAttachment(ctx context.Context, name, mode string) (dataplane.DPDKVerification, bool) {
	if !server.tryNativeSwitchLock() {
		log.Printf("dataplane watcher: %s adoption of %s skipped: %v", name, takeoverModeLabel(mode), errNativeSwitchBusy)
		return dataplane.DPDKVerification{}, false
	}
	defer server.unlockNativeSwitch()
	return server.adoptNativeAttachmentLocked(ctx, name, mode)
}

// adoptNativeAttachmentLocked is the shared teardown/build path. The caller
// holds the native switch lock.
func (server *Server) adoptNativeAttachmentLocked(ctx context.Context, name, mode string) (dataplane.DPDKVerification, bool) {
	name = strings.TrimSpace(name)
	mode = strings.TrimSpace(mode)
	if name == "" || mode == "" || takeoverModeDestructive(mode) {
		return dataplane.DPDKVerification{}, false
	}
	// AF_XDP cannot attach to a paravirtual NIC. Recreating the attachment on a
	// driver that is known to reject it is a dataplane event with no upside, so
	// the persisted read-only verdict wins over a stale ownership record.
	if mode != takeoverModeAFPacket {
		if driver := vpp.NativeProbeUnsupportedNetdev(name); driver != "" {
			log.Printf("dataplane watcher: refusing %s adoption for %s: kernel driver %s does not support it (read-only verdict)", takeoverModeLabel(mode), name, driver)
			return dataplane.DPDKVerification{}, false
		}
	}
	transition := takeoverTransition{
		SchemaVersion: 1,
		Interface:     name,
		Netdev:        name,
		VPPInterface:  "lyroute-" + name,
		PCIAddress:    inventoryPCI(name),
		FromMode:      "",
		ToMode:        mode,
	}
	if err := (systemTakeoverExecutor{server: server}).convergeNativeAttachmentTo(ctx, &transition, mode); err != nil {
		log.Printf("dataplane watcher: %s adoption of %s failed: %v", name, takeoverModeLabel(mode), err)
		return dataplane.DPDKVerification{}, false
	}
	snapshot := takeoverSnapshot{LinuxInterface: name, Netdev: name, VPPInterface: "lyroute-" + name, PCIAddress: inventoryPCI(name)}
	if !server.persistLiveVerification(ctx, snapshot, mode, "watcher_auto_adopt") {
		log.Printf("dataplane watcher: %s adoption of %s was not confirmed by the live readback", name, takeoverModeLabel(mode))
		return dataplane.DPDKVerification{}, false
	}
	entry := dataplane.DPDKVerification{
		LinuxInterface: name,
		PCIAddress:     inventoryPCI(name),
		Mode:           mode,
		LinkState:      netdevLinkState(name),
		Verification:   "pending_link",
		ObservedAt:     time.Now().UTC(),
	}
	if entry.LinkState == "up" {
		entry.Verification = "verified"
		entry.FunctionalVerified = true
		entry.HardwareLink = "up"
		entry.Method = "watcher_auto_adopt"
	}
	log.Printf("dataplane watcher: auto-adopted %s via %s (verification=%s)", name, takeoverModeLabel(mode), entry.Verification)
	return entry, true
}

// activeDataplaneMode returns the takeover mode recorded in active.json.
func activeDataplaneMode(name string) (string, bool) {
	data, err := os.ReadFile(envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json"))
	if err != nil {
		return "", false
	}
	var state activeDataplaneFile
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false
	}
	for _, attachment := range state.Path.Attachments {
		if strings.TrimSpace(attachment.LinuxInterface) == strings.TrimSpace(name) {
			return takeoverModeFromAttachment(attachment.Tier, attachment.Hook, attachment.Mode), true
		}
	}
	return "", false
}

func watcherCooldownReset(name string) {
	delete(dataplaneWatcherLast, name)
}
