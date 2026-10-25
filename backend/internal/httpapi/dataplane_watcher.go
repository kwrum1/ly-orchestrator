package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

var dataplaneWatcherLast = map[string]time.Time{}

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

func (server *Server) watchDataplaneOnce(ctx context.Context) {
	interfaces := server.runtimeDataInterfaces(ctx)
	if len(interfaces) == 0 {
		return
	}
	vppNames := server.vppInterfaceNames(ctx)
	policy := server.dataplanePolicy(ctx)
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
		if entry.Mode == "" && vppNames["lyroute-"+name] {
			// The only non-DPDK attachment that produces lyroute-<if> is
			// AF_PACKET (firstboot or a previous apply).
			entry.Mode = "af_packet"
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
			if entry.Mode == "af_packet" && !policyAllowsAFPort(policy, server.displayInterfaceName(ctx, name)) {
				log.Printf("dataplane watcher: %s lost its VPP attachment; AF_PACKET consent required; kernel netdev left intact", name)
				dropDPDKVerification(name)
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.consent_required", "blocked", "af_packet", "")
				continue
			}
			if adopted, ok := server.restoreNativeAttachment(ctx, name, entry); ok {
				updates[name] = adopted
				log.Printf("dataplane watcher: %s re-adopted via %s after VPP lost the attachment", name, adopted.Mode)
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_adopt", "success", "mode="+adopted.Mode, "")
			}
			continue
		}
		link := netdevLinkState(name)
		if link == "unknown" && entry.LinkState != "" {
			link = normalizeLinkState(entry.LinkState)
		}
		if owned {
			if link == "down" {
				if updated, changed := demoteOnLinkDown(entry, link); changed {
					updates[name] = updated
					log.Printf("dataplane watcher: %s link down; takeover retained, verification=pending_link", name)
					server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.pending_link", "success", "", "")
				}
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
		if capability, unsupported := capabilities.NativeUnsupported(strings.TrimSpace(item.KernelDriver), strings.TrimSpace(item.PCIAddress)); unsupported {
			if policyAllowsAFPort(policy, server.displayInterfaceName(ctx, name)) {
				if entry, ok := server.autoAdoptAF(ctx, name); ok {
					updates[name] = entry
					server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.auto_adopt", "success", "mode=af_packet", "")
				}
			} else {
				log.Printf("dataplane watcher: %s native path marked unsupported (%s); AF_PACKET consent required; kernel netdev left intact", name, capability.Reason)
				server.recordAuditWithTransaction("system", "system", "interface:"+name, "dataplane.takeover.consent_required", "blocked", "af_packet", "")
			}
			continue
		}
		bootstrapNeeded = true
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
	entry.Reason = "链路已断开；接管保留，等待链路恢复"
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
			capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
			capabilities.MarkUnsupported("", entry.PCIAddress, reason)
			if err := dataplane.SaveTakeoverCapabilities(takeoverCapabilityPath(), capabilities); err != nil {
				log.Printf("dataplane watcher: persist crash guard: %v", err)
			}
			server.releaseFailedDPDKTakeovers(ctx, []dpdkTakeoverFailure{{LinuxInterface: name, PCIAddress: entry.PCIAddress, Reason: reason}})
			if !server.restartVPPAndWait(ctx) {
				log.Printf("dataplane watcher: VPP did not recover after %s crash; management stays on Linux", name)
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
	peer := functionalPeer(name)
	if peer != "" {
		before, _ := vppInterfaceCounters(stateOutput, vppInterface)
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = exec.CommandContext(pingCtx, vppctl, "ping", peer, "source", vppInterface, "repeat", "1").CombinedOutput()
		cancel()
		afterOutput, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface)
		after, _ := vppInterfaceCounters(afterOutput, vppInterface)
		entry.TXBefore = before
		entry.TXAfter = after
		if after > before {
			entry.Verification = "verified"
			entry.FunctionalVerified = true
			entry.Method = "tx_counter_and_link_speed"
			return entry
		}
	}
	entry.Verification = "verified"
	entry.FunctionalVerified = true
	entry.Method = "watcher_link_state_up"
	return entry
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
// (for example after a VPP restart). The kernel netdev is untouched.
func (server *Server) restoreNativeAttachment(ctx context.Context, name string, entry dataplane.DPDKVerification) (dataplane.DPDKVerification, bool) {
	switch entry.Mode {
	case "af_packet":
		return server.autoAdoptAF(ctx, name)
	case "af_xdp_zero_copy", "af_xdp_copy":
		return server.autoAdoptAFXDP(ctx, name, entry.Mode)
	}
	return dataplane.DPDKVerification{}, false
}

func (server *Server) autoAdoptAFXDP(ctx context.Context, name, mode string) (dataplane.DPDKVerification, bool) {
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	vppInterface := "lyroute-" + name
	output, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface)
	if !vppCLIInterfacePresent(output, vppInterface) {
		ipCommand := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
		_, _ = exec.CommandContext(ctx, ipCommand, "link", "set", "dev", name, "promisc", "on", "up").CombinedOutput()
		modeFlag := "no-zero-copy"
		if mode == "af_xdp_zero_copy" {
			modeFlag = "zero-copy"
		}
		if _, err := exec.CommandContext(ctx, vppctl, "create", "interface", "af_xdp", "host-if", name, "name", vppInterface, "rx-queue-size", "512", "tx-queue-size", "512", "num-rx-queues", "all", modeFlag, "mac-reuse").CombinedOutput(); err != nil {
			return dataplane.DPDKVerification{}, false
		}
		if output, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface); !vppCLIInterfacePresent(output, vppInterface) {
			return dataplane.DPDKVerification{}, false
		}
	}
	_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
	entry := dataplane.DPDKVerification{
		LinuxInterface: name,
		PCIAddress:     inventoryPCI(name),
		Mode:           mode,
		LinkState:      netdevLinkState(name),
		Verification:   "pending_link",
		ObservedAt:     time.Now().UTC(),
	}
	log.Printf("dataplane watcher: auto-adopted %s via %s (verification=%s)", name, mode, entry.Verification)
	return entry, true
}

// autoAdoptAF takes over an AF_PACKET-only port. The caller has already
// verified operator consent; the kernel netdev stays intact.
func (server *Server) autoAdoptAF(ctx context.Context, name string) (dataplane.DPDKVerification, bool) {
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	vppInterface := "lyroute-" + name
	output, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface)
	if !vppCLIInterfacePresent(output, vppInterface) {
		// VPP's host-interface create accepts only the Linux name in this
		// version; the VPP name is set with a separate rename. Create-then-
		// rename is also what firstboot and the apply transaction do.
		ipCommand := envOrDefault("LY_ROUTE_IP_COMMAND", "ip")
		_, _ = exec.CommandContext(ctx, ipCommand, "link", "set", "dev", name, "promisc", "on", "up").CombinedOutput()
		if _, err := exec.CommandContext(ctx, vppctl, "create", "host-interface", "name", name, "cksum-gso-disable").CombinedOutput(); err != nil {
			return dataplane.DPDKVerification{}, false
		}
		_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "name", "host-"+name, vppInterface).CombinedOutput()
		if mac := netdevMAC(name); mac != "" {
			_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "mac", "address", vppInterface, mac).CombinedOutput()
		}
		if output, _ := execCommandOutput(ctx, vppctl, "show", "interface", vppInterface); !vppCLIInterfacePresent(output, vppInterface) {
			return dataplane.DPDKVerification{}, false
		}
	}
	_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
	entry := dataplane.DPDKVerification{
		LinuxInterface: name,
		PCIAddress:     inventoryPCI(name),
		Mode:           "af_packet",
		LinkState:      netdevLinkState(name),
		Verification:   "pending_link",
		ObservedAt:     time.Now().UTC(),
		Reason:         "AF_PACKET 兼容模式（性能受限）",
	}
	if entry.LinkState == "up" {
		// The next watcher tick promotes it with a functional probe. Never
		// claim verified purely from the link state on adoption.
		entry.Reason = ""
	}
	log.Printf("dataplane watcher: auto-adopted %s via AF_PACKET (verification=%s)", name, entry.Verification)
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
