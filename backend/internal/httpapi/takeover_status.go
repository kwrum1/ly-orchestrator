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
	policy := server.dataplanePolicy(ctx)
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
	capabilities, _ := dataplane.LoadTakeoverCapabilities(takeoverCapabilityPath())
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
	if entry, ok := verification[name]; ok {
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
	owned := false
	if entry, ok := ownership[name]; ok && strings.TrimSpace(entry.PCIAddress) != "" {
		owned = true
		if ownedMode == "" {
			ownedMode = takeoverModeFromDriver(entry.Transport)
		}
	}
	if entry, ok := active[name]; ok {
		owned = true
		if ownedMode == "" {
			ownedMode = entry.Mode
		}
		if verificationState == "" {
			verificationState = normalizeVerification(entry.Verification, false)
		}
	}
	if vppNames["lyroute-"+name] {
		owned = true
		if ownedMode == "" {
			ownedMode = "af_packet"
		}
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
	if management {
		state = "unsupported"
		reason = "管理口不参与数据面接管"
		if verificationState == "" {
			verificationState = "pending_link"
		}
	} else if owned {
		state = "taken_over"
		mode = ownedMode
		class = takeoverClass(mode)
		if verificationState == "" {
			verificationState = "pending_link"
		}
	} else {
		mode, reason = server.bestAvailableTakeoverMode(ctx, name, pci, policy, capabilities)
		class = takeoverClass(mode)
		if mode == "" {
			state = "unsupported"
			verificationState = "failed"
			if reason == "" {
				reason = "该网卡当前没有可用的接管模式"
			}
		} else if mode == "af_packet" && !server.allowsAFPacketFor(ctx, policy, name) {
			state = "consent_required"
			verificationState = "pending_link"
			reason = "需要确认 AF_PACKET 兼容模式（性能受限）"
		} else {
			state = "available"
			verificationState = "pending_link"
			reason = ""
		}
	}
	payload := map[string]any{
		"takeover_state":  state,
		"takeover_mode":   nullableString(mode),
		"takeover_class":  nullableString(class),
		"link_state":      normalizeLinkState(linkState),
		"link_speed_mbps": linkSpeed,
		"verification":    verificationState,
		"management":      management,
		"pci":             pci,
		"reason":          reason,
	}
	return payload
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

// bestAvailableTakeoverMode is a non-destructive capability probe. It never
// binds a device: it reads the cached runtime capability report first and only
// falls back to driver/XDP facts from sysfs. The selected order follows the
// documented policy (see docs/DATAPLANE-REFERENCES.md).
func (server *Server) bestAvailableTakeoverMode(ctx context.Context, name, pci string, policy DataplanePolicy, capabilities dataplane.TakeoverCapabilityState) (string, string) {
	if report, err := loadCapabilityReport(envOrDefault("LY_ROUTE_VPP_CAPABILITY_PROOF", "/var/lib/ly-route/vpp-native-capabilities.json")); err == nil {
		for _, proof := range report.Proofs {
			if strings.TrimSpace(proof.LinuxInterface) != name {
				continue
			}
			candidates := proof.Candidates
			if len(candidates) == 0 {
				candidates = []vpp.CapabilityProof{proof.Proof}
			}
			best := ""
			bestRank := 1 << 30
			for _, candidate := range candidates {
				mode := takeoverModeFromAttachment(candidate.Tier, candidate.Hook, candidate.Mode)
				if mode == "" {
					continue
				}
				if rank := takeoverModeRank(mode); rank < bestRank {
					best, bestRank = mode, rank
				}
			}
			if best != "" {
				return best, ""
			}
		}
	}
	driver := strings.TrimSpace(inventoryDriver(name))
	if _, unsupported := capabilities.NativeUnsupported(driver, pci); unsupported {
		if server.allowsAFPacketFor(ctx, policy, name) {
			return "af_packet", ""
		}
		return "af_packet", "原生接管在该网卡上不可用；AF_PACKET 兼容模式需要确认"
	}
	device := filepath.Join(sysfsRoot(), "class/net", name)
	if _, err := os.Stat(filepath.Join(device, "device")); err != nil {
		return "", ""
	}
	// A netdev with RX queues can keep its kernel stack and still be attached
	// through AF_XDP. Report the conservative copy mode until a cached probe
	// proves zero-copy support.
	if _, err := os.Stat(filepath.Join(device, "queues/rx-0")); err == nil {
		return "af_xdp_copy", ""
	}
	return "af_packet", ""
}

func takeoverModeRank(mode string) int {
	switch mode {
	case "dpdk_vfio":
		return 0
	case "af_xdp_zero_copy":
		return 1
	case "dpdk_uio":
		return 2
	case "af_xdp_copy":
		return 3
	case "rdma":
		return 4
	case "af_packet":
		return 5
	}
	return 100
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
