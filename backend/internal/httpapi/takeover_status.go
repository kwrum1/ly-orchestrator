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
	"time"

	"ly-route/backend/internal/runtime/dataplane"
	"ly-route/backend/internal/runtime/takeover"
	"ly-route/backend/internal/runtime/vpp"
)

type activeDataplaneFile struct {
	Path struct {
		Tier        vpp.DataplaneTier `json:"tier"`
		Attachments []struct {
			LinuxInterface string            `json:"linux_interface"`
			VPPInterface   string            `json:"vpp_interface"`
			Tier           vpp.DataplaneTier `json:"tier"`
			Hook           vpp.NativeHook    `json:"hook"`
			Mode           vpp.NativeMode    `json:"mode"`
			PCIAddress     string            `json:"pci_address"`
			Verification   string            `json:"verification"`
		} `json:"attachments"`
	} `json:"path"`
}

func (server *Server) vppInterfaceNames(ctx context.Context) map[string]bool {
	names := map[string]bool{}
	output, err := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "interface")
	if err != nil {
		return names
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && line != "" && line[0] != ' ' && line[0] != '\t' {
			names[fields[0]] = true
		}
	}
	return names
}

// interfaceTakeoverStatus is the UI contract. Capability and link state are
// orthogonal: link_state never changes takeover_state. No driver, model or
// kernel stack string is exposed here.
func (server *Server) interfaceTakeoverStatus(ctx context.Context, name string, item map[string]any, vppNames map[string]bool) map[string]any {
	name = strings.TrimSpace(name)
	management := name == server.managementInterfaceID(ctx) || strings.EqualFold(strings.TrimSpace(stringField(item, "gateway_role")), "management")
	inventory := map[string]dataplane.InventoryInterface{}
	if state, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
		for _, entry := range state.DataInterfaces {
			inventory[strings.TrimSpace(entry.LinuxInterface)] = entry
		}
	}
	ownership := map[string]dataplane.OwnershipDevice{}
	if state, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, entry := range state.Devices {
			ownership[strings.TrimSpace(entry.LinuxInterface)] = entry
		}
	}
	verification := map[string]dataplane.DPDKVerification{}
	if state, err := dataplane.LoadDPDKVerification(dataplaneVerificationPath()); err == nil {
		for _, entry := range state.Interfaces {
			verification[strings.TrimSpace(entry.LinuxInterface)] = entry
		}
	}
	active := map[string]struct {
		Mode         string
		PCI          string
		Verification string
	}{}
	if data, err := os.ReadFile(envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json")); err == nil {
		var state activeDataplaneFile
		if json.Unmarshal(data, &state) == nil {
			for _, attachment := range state.Path.Attachments {
				active[strings.TrimSpace(attachment.LinuxInterface)] = struct {
					Mode         string
					PCI          string
					Verification string
				}{
					Mode:         takeoverModeFromAttachment(attachment.Tier, attachment.Hook, attachment.Mode),
					PCI:          strings.TrimSpace(attachment.PCIAddress),
					Verification: strings.TrimSpace(attachment.Verification),
				}
			}
		}
	}
	pci := inventoryPCI(name)
	if entry, ok := inventory[name]; ok && pci == "" {
		pci = strings.TrimSpace(entry.PCIAddress)
	}
	if entry, ok := ownership[name]; ok && pci == "" {
		pci = strings.TrimSpace(entry.PCIAddress)
	}
	if entry, ok := verification[name]; ok && pci == "" {
		pci = strings.TrimSpace(entry.PCIAddress)
	}
	if entry, ok := active[name]; ok && pci == "" {
		pci = entry.PCI
	}

	linkState := netdevLinkState(name)
	if linkState == "unknown" {
		// Host inventory may already carry the observed link state (and the
		// unit tests pin it explicitly). Prefer it over "unknown".
		if value := strings.TrimSpace(stringField(item, "link_state")); value != "" {
			linkState = normalizeLinkState(value)
		}
	}
	linkSpeed := netdevLinkSpeedMbps(name)
	if linkSpeed == 0 {
		linkSpeed = inventoryLinkSpeedMbps(item)
	}
	verificationState := ""
	ownedMode := ""
	staleReasons := []string{}
	if entry, ok := verification[name]; ok {
		if pciMismatch(pci, entry.PCIAddress) {
			staleReasons = append(staleReasons, "the persisted verification belongs to another PCI device")
		} else {
			if linkState == "unknown" {
				if entry.HardwareLink != "" {
					linkState = normalizeLinkState(entry.HardwareLink)
				} else if entry.LinkState != "" {
					linkState = normalizeLinkState(entry.LinkState)
				}
			}
			if entry.LinkSpeedMbps > 0 {
				linkSpeed = entry.LinkSpeedMbps
			}
			verificationState = normalizeVerification(entry.Verification, entry.FunctionalVerified)
			ownedMode = takeoverModeFromDriver(entry.Mode)
		}
	}
	owned := false
	if _, ok := verification[name]; ok && len(staleReasons) == 0 {
		// A persisted verification entry is proof of a committed takeover even
		// when the ownership record was cleaned up (AF_PACKET/AF_XDP keep the
		// kernel netdev and do not create an ownership entry).
		owned = true
	}
	if entry, ok := ownership[name]; ok && strings.TrimSpace(entry.PCIAddress) != "" {
		if pciMismatch(pci, entry.PCIAddress) {
			staleReasons = append(staleReasons, "the ownership record belongs to another PCI device")
		} else {
			owned = true
			if mode := takeoverModeFromDriver(entry.Transport); mode != "" {
				// The ownership record describes the live PCI binding. A stale
				// verification entry from a previous AF_PACKET/AF_XDP generation
				// must not make a UIO-bound port look like a compatibility port.
				if ownedMode != "" && ownedMode != mode {
					verificationState = ""
					linkState = "unknown"
					linkSpeed = 0
				}
				ownedMode = mode
			}
		}
	}
	if entry, ok := active[name]; ok && !pciMismatch(pci, entry.PCI) {
		owned = true
		if ownedMode == "" {
			ownedMode = entry.Mode
		}
		if verificationState == "" {
			verificationState = normalizeVerification(entry.Verification, false)
		}
	}
	// A DPDK port has no kernel netdev: carrier and negotiated rate only exist
	// in VPP. Read them live so the UI stops showing "—" after the takeover.
	if strings.HasPrefix(ownedMode, "dpdk_") || (strings.TrimSpace(ownedMode) == "" && pciBoundDestructively(pci)) {
		if snapshot, ok := server.vppLinkSnapshotFor(ctx, name, pci); ok {
			if snapshot.Carrier == "up" || snapshot.Carrier == "down" {
				linkState = snapshot.Carrier
			}
			if snapshot.SpeedMbps > 0 {
				linkSpeed = snapshot.SpeedMbps
			}
		}
	}
	interfaceNameOnly := false
	if vppNames["lyroute-"+name] {
		// A VPP interface name is a weak signal: after a NIC swap or a role
		// deletion it can be a leftover down interface. It never proves a
		// takeover on its own.
		interfaceNameOnly = true
	}
	if !owned {
		if proof, ok := server.validatedDPDKOwnershipProof(name, pci); ok {
			owned = true
			staleReasons = nil
			if ownedMode == "" {
				ownedMode = proof.Mode
			}
			log.Printf("dataplane capability: %s (%s) is already bound and has a trusted %s proof; reporting %s", name, pci, proof.Source, proof.Mode)
		}
	}
	if !owned && interfaceNameOnly {
		staleReasons = append(staleReasons, "VPP has an interface with the product name but no ownership evidence")
	}
	if owned && verificationState == "" {
		verificationState = "pending_link"
	}
	// Live link down demotes the proof, never the ownership.
	if owned && linkState == "down" {
		verificationState = "pending_link"
	}

	state := "unsupported"
	mode := ""
	class := ""
	reason := ""
	assessment := vpp.CapabilityAssessment{Status: vpp.CapabilityUnknown}
	needsRepair := false
	if last, ok := lastTakeoverTransition(name); ok {
		needsRepair = last.NeedsRepair
	}
	if management {
		state = "management"
		reason = "管理口不参与数据面接管"
		assessment = vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: reason, Source: "product_policy"}
		if verificationState == "" {
			verificationState = "pending_link"
		}
	} else if owned {
		state = "taken_over"
		mode = ownedMode
		class = takeoverClass(mode)
		assessment = committedTakeoverAssessment(mode, pci)
		if verificationState == "" {
			verificationState = "pending_link"
		}
		if needsRepair {
			state = "needs_repair"
			reason = "上次接管失败且未能完全清理，需要修复后再重试"
		}
	} else if len(staleReasons) > 0 {
		// The persisted evidence does not match the device that is present now.
		// Do not claim a takeover; expose the stale state and let the next probe
		// decide.
		state = "stale"
		verificationState = "pending_link"
		reason = "接管证据与当前网卡不一致，等待重新探测"
		assessment = vpp.CapabilityAssessment{
			Status:      vpp.CapabilityUnknown,
			Reason:      strings.Join(staleReasons, "; "),
			Constraints: []string{"stale takeover evidence"},
			Source:      "stale_evidence",
		}
	} else if needsRepair {
		// No live attachment and the last automatic attempt could not clean up.
		// Never report the port as available: it needs a repair, not a retry.
		state = "needs_repair"
		verificationState = "failed"
		reason = "上次接管失败且未能完全清理，需要修复后再重试"
		assessment = vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: reason, Source: "takeover_journal"}
	} else {
		// The decision is backend-owned: the plan is the authority for what the
		// appliance will do automatically. Nothing client-supplied is read here.
		plan := server.takeoverPlanForPort(ctx, name)
		if plan.Skipped != "" {
			state = "unsupported"
			verificationState = "failed"
			reason = plan.Skipped
			assessment = vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: reason, Source: "takeover_rule_library"}
		} else if primary, ok := plan.Primary(); ok {
			mode = string(primary.Mode)
			class = takeoverClass(mode)
			assessment = vpp.CapabilityAssessment{
				Hook:        vppNativeHookForTakeover(mode),
				Mode:        vppNativeModeForTakeover(mode),
				Status:      vpp.CapabilitySupportedWithConstraints,
				Constraints: planConstraints(plan),
				Reason:      firstNonEmptyTrimmed(primary.Reason, plan.RuleReason),
				Source:      "takeover_rule_library",
			}
			state = "available"
			verificationState = "pending_link"
		} else {
			state = "unsupported"
			verificationState = "failed"
			reason = "该网卡当前没有可用的接管模式"
			assessment = vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: reason, Source: "takeover_rule_library"}
		}
	}
	constraints := assessment.Constraints
	if constraints == nil {
		constraints = []string{}
	}
	payload := map[string]any{
		"takeover_state":             state,
		"takeover_mode":              nullableString(mode),
		"takeover_class":             nullableString(class),
		"takeover_capability":        string(assessment.Status),
		"capability_constraints":     constraints,
		"takeover_automatic":         true,
		"takeover_display":           string(takeover.DisplayFor(takeover.Mode(mode), management, needsRepair)),
		"stale":                      len(staleReasons) > 0,
		"needs_repair":               needsRepair,
		"link_state":                 normalizeLinkState(linkState),
		"link_speed_mbps":            linkSpeed,
		"link_speed_capability_mbps": linkSpeedCapabilityMbps(pci),
		"verification":               verificationState,
		"management":                 management,
		"pci":                        pci,
		"reason":                     reason,
	}
	if assessment.Reason != "" && reason == "" {
		payload["capability_reason"] = assessment.Reason
	}
	if transition, ok := lastTakeoverTransition(name); ok {
		payload["takeover_transition"] = takeoverTransitionSummary(transition)
		if transition.CapabilityUnprovenProbe {
			payload["capability_unproven_probe"] = true
			payload["probe_result"] = transition.ProbeResult
		}
	}
	return payload
}

// planConstraints turns the backend plan into the short constraint list the UI
// shows. Compatibility mode is always described as performance limited.
func planConstraints(plan takeover.Plan) []string {
	constraints := make([]string, 0, 2)
	for _, candidate := range plan.Candidates {
		switch candidate.Mode {
		case takeover.ModeAFPacket:
			constraints = append(constraints, "compatibility mode (performance limited)")
		case takeover.ModeAFXDPCopy:
			constraints = append(constraints, "copy mode (no zero-copy)")
		case takeover.ModeDPDKUIO:
			constraints = append(constraints, "no IOMMU protection (uio_pci_generic)")
		}
	}
	if len(constraints) == 0 {
		return nil
	}
	return constraints
}

// committedTakeoverAssessment reports the four-state capability of a mode that
// is already carrying traffic. Being committed proves this mode works; it does
// NOT prove that the other transport is available. IOMMU/VFIO/UIO facts are
// therefore reported as unknown instead of fabricated "known true" values.
func committedTakeoverAssessment(mode string, pci string) vpp.CapabilityAssessment {
	assessment := vpp.CapabilityAssessment{
		Hook:   vppNativeHookForTakeover(mode),
		Mode:   vppNativeModeForTakeover(mode),
		Source: "committed_state",
		Evidence: []vpp.CapabilityEvidence{
			{Name: "iommu_protected", CapabilityFact: vpp.UnknownFact("not observed in the committed state")},
			{Name: "vfio_pci", CapabilityFact: vpp.UnknownFact("not observed in the committed state")},
			{Name: "uio_pci_generic", CapabilityFact: vpp.UnknownFact("not observed in the committed state")},
		},
	}
	if pci != "" {
		assessment.Evidence = append(assessment.Evidence, vpp.CapabilityEvidence{
			Name:           "pci_address",
			CapabilityFact: vpp.KnownFact(true),
		})
	}
	switch mode {
	case takeoverModeDPDKVFIO:
		assessment.Status = vpp.CapabilitySupported
		return assessment
	case takeoverModeDPDKUIO:
		// The no-IOMMU constraint is a property of the committed binding itself,
		// not an observation about an IOMMU group.
		assessment.Status = vpp.CapabilitySupportedWithConstraints
		assessment.Constraints = []string{"no IOMMU protection (uio_pci_generic)"}
		return assessment
	case takeoverModeAFPacket:
		assessment.Status = vpp.CapabilitySupportedWithConstraints
		assessment.Constraints = []string{"compatibility mode (performance limited)"}
		return assessment
	case "":
		return vpp.CapabilityAssessment{Status: vpp.CapabilityUnknown, Reason: "no takeover mode is committed", Source: "committed_state"}
	}
	assessment.Status = vpp.CapabilitySupported
	return assessment
}

// stripInternalInterfaceFields removes legacy internal codes and driver/kernel
// details from the public interface payload. The takeover contract carries the
// user-facing capability; active_path/work_mode/capability were internal
// implementation vocabulary and must not reach the UI.
func stripInternalInterfaceFields(item map[string]any) {
	for _, key := range []string{
		"driver", "kernel_driver", "driver_model", "dataplane_hook",
		"dataplane_driver", "takeover_driver", "takeover", "hook",
		"active_path", "work_mode", "vpp_interface", "capability",
	} {
		delete(item, key)
	}
}

func normalizeVerification(value string, functional bool) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "verified", "pending_link", "failed":
		return strings.ToLower(strings.TrimSpace(value))
	}
	if functional {
		return "verified"
	}
	return ""
}

// persistNativeTakeoverProofs records the non-DPDK attachments of a committed
// apply. They start as pending_link and the watcher promotes them with a
// functional probe; the mode/class is then known without inspecting VPP output.
func (server *Server) persistNativeTakeoverProofs(ctx context.Context, path vpp.NativePath) {
	entries := make([]dataplane.DPDKVerification, 0, len(path.Attachments))
	now := time.Now().UTC()
	for _, attachment := range path.Attachments {
		if attachment.Tier == vpp.DataplaneTierDPDK {
			continue
		}
		mode := takeoverModeFromAttachment(attachment.Tier, attachment.Hook, attachment.Mode)
		if mode == "" {
			continue
		}
		link := netdevLinkState(attachment.LinuxInterface)
		entry := dataplane.DPDKVerification{
			LinuxInterface: attachment.LinuxInterface,
			PCIAddress:     inventoryPCI(attachment.LinuxInterface),
			Mode:           mode,
			LinkState:      link,
			HardwareLink:   link,
			Verification:   "pending_link",
			ObservedAt:     now,
		}
		if link == "down" {
			entry.Reason = "链路已断开；接管保留，等待链路恢复"
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return
	}
	if err := mergeDPDKVerification(dataplaneVerificationPath(), entries); err != nil {
		log.Printf("persist native takeover proofs: %v", err)
	}
}

// bestAvailableTakeoverMode reports the backend-selected mode for a port that is
// not taken over yet. The decision comes from the static rule library and the
// ordered plan (docs/TAKEOVER-OUT-OF-BOX.md); the client cannot influence it.
//
// A crash-guarded device skips its destructive/DPDK candidates; the plan then
// continues with AF_XDP copy and the compatibility path. A missing runtime proof
// no longer blocks a first takeover: the automatic engine performs one
// transactional attempt instead of asking an operator to confirm.
func (server *Server) bestAvailableTakeoverMode(ctx context.Context, name, pci string, _ DataplanePolicy, capabilities dataplane.TakeoverCapabilityState) (string, vpp.CapabilityAssessment, string) {
	// A trusted ownership proof is device-level evidence and outranks the
	// plan's sysfs view: a DPDK-bound port has no netdev, so it would otherwise
	// look "not physical" and be skipped.
	if proof, ok := server.validatedDPDKOwnershipProof(name, pci); ok {
		log.Printf("dataplane capability: %s (%s) already has a trusted %s ownership proof; reporting %s without a probe", name, pci, proof.Source, proof.Mode)
		return proof.Mode, committedTakeoverAssessment(proof.Mode, pci), ""
	}
	plan := server.takeoverPlanForPort(ctx, name)
	if plan.Skipped != "" {
		return "", vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: plan.Skipped, Source: "takeover_rule_library"}, plan.Skipped
	}
	driver := strings.TrimSpace(inventoryDriver(name))
	guarded := server.takeoverGuardActive(capabilities, driver, pci, name)
	for _, candidate := range plan.Candidates {
		if guarded && takeoverModeDestructive(string(candidate.Mode)) {
			log.Printf("dataplane capability: %s (%s) skips %s: the DPDK path is crash-guarded in this environment", name, pci, candidate.Mode)
			continue
		}
		assessment := vpp.CapabilityAssessment{
			Hook:        vppNativeHookForTakeover(string(candidate.Mode)),
			Mode:        vppNativeModeForTakeover(string(candidate.Mode)),
			Status:      vpp.CapabilitySupportedWithConstraints,
			Constraints: planConstraints(plan),
			Reason:      firstNonEmptyTrimmed(candidate.Reason, plan.RuleReason),
			Source:      "takeover_rule_library",
		}
		if takeoverModeDestructive(string(candidate.Mode)) {
			assessment.Reason = "automatic takeover attempt; the mode is verified after the bind"
		}
		return string(candidate.Mode), assessment, ""
	}
	return "", vpp.CapabilityAssessment{Status: vpp.CapabilityUnsupported, Reason: "no takeover candidate is available for this port", Source: "takeover_rule_library"}, "该网卡当前没有可用的接管模式"
}

func inventoryLinkSpeedMbps(item map[string]any) int64 {
	for _, key := range []string{"link_speed_mbps", "speed_mbps"} {
		if value, ok := item[key]; ok {
			switch typed := value.(type) {
			case int:
				return int64(typed)
			case int64:
				return typed
			case float64:
				return int64(typed)
			case json.Number:
				if parsed, err := typed.Int64(); err == nil {
					return parsed
				}
			}
		}
	}
	raw := strings.ToLower(strings.TrimSpace(stringField(item, "speed")))
	if raw == "" {
		return 0
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(raw, "gb/s") || strings.HasSuffix(raw, "gbps"):
		multiplier = 1000
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "gb/s"), "gbps")
	case strings.HasSuffix(raw, "mb/s") || strings.HasSuffix(raw, "mbps"):
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "mb/s"), "mbps")
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value * multiplier
}

func inventoryPCI(name string) string {
	device := filepath.Join(sysfsRoot(), "class/net", name, "device")
	resolved, err := filepath.EvalSymlinks(device)
	if err != nil {
		return ""
	}
	return filepath.Base(resolved)
}

func inventoryDriver(name string) string {
	device := filepath.Join(sysfsRoot(), "class/net", name, "device")
	driver, err := filepath.EvalSymlinks(filepath.Join(device, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(driver)
}

func netdevLinkState(name string) string {
	path := filepath.Join(sysfsRoot(), "class/net", name)
	if data, err := os.ReadFile(filepath.Join(path, "carrier")); err == nil {
		if strings.TrimSpace(string(data)) == "1" {
			return "up"
		}
		return "down"
	}
	if data, err := os.ReadFile(filepath.Join(path, "operstate")); err == nil {
		return normalizeLinkState(string(data))
	}
	return "unknown"
}

func normalizeLinkState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "up":
		return "up"
	case "down", "lowerlayerdown", "notpresent":
		return "down"
	default:
		return "unknown"
	}
}

func netdevLinkSpeedMbps(name string) int64 {
	data, err := os.ReadFile(filepath.Join(sysfsRoot(), "class/net", name, "speed"))
	if err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func takeoverModeFromAttachment(tier vpp.DataplaneTier, hook vpp.NativeHook, mode vpp.NativeMode) string {
	if tier == vpp.DataplaneTierDPDK || hook == vpp.NativeHookDPDK {
		switch mode {
		case vpp.NativeModeDPDKVFIO:
			return "dpdk_vfio"
		case vpp.NativeModeDPDKUIO:
			return "dpdk_uio"
		}
	}
	switch hook {
	case vpp.NativeHookAFXDP:
		if mode == vpp.NativeModeZeroCopy {
			return "af_xdp_zero_copy"
		}
		return "af_xdp_copy"
	case vpp.NativeHookRDMA:
		return "rdma"
	case vpp.NativeHookAFPacket:
		return "af_packet"
	}
	return ""
}

func takeoverModeFromDriver(value string) string {
	switch strings.TrimSpace(value) {
	case "vfio", "vfio-pci", "vfio_pci":
		return "dpdk_vfio"
	case "uio", "uio_pci_generic":
		return "dpdk_uio"
	case "af_packet", "linux_packet_socket":
		return "af_packet"
	case "af_xdp_zero_copy":
		return "af_xdp_zero_copy"
	case "af_xdp_copy":
		return "af_xdp_copy"
	case "rdma", "rdma_dv":
		return "rdma"
	}
	return ""
}

func takeoverClass(mode string) string {
	if mode == "" {
		return ""
	}
	if mode == "af_packet" {
		return "compatibility"
	}
	return "native"
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func execCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	return string(output), err
}

func loadCapabilityReport(path string) (vpp.CapabilityReport, error) {
	var report vpp.CapabilityReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, err
	}
	return report, nil
}
