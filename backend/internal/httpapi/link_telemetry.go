package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// vppLinkSnapshot is the live hardware telemetry VPP reports for one port.
// DPDK ports have no kernel netdev, so sysfs link_state/speed are meaningless
// once the PMD owns the device. The carrier and negotiated rate only exist in
// `show hardware-interfaces`.
type vppLinkSnapshot struct {
	Interface string
	PCI       string
	Carrier   string
	SpeedMbps int64
	MAC       string
}

type vppLinkCacheEntry struct {
	source  string
	expires time.Time
	byName  map[string]vppLinkSnapshot
	byPCI   map[string]string
	err     error
}

var (
	vppLinkCacheMu sync.Mutex
	vppLinkCache   vppLinkCacheEntry
)

// The UI polls the interface list; a full `show hardware-interfaces` for every
// row would multiply vppctl calls. Four seconds keeps the link column live
// without turning a page refresh into a VPP CLI storm.
const vppLinkCacheTTL = 4 * time.Second

func (server *Server) vppLinkTelemetry(ctx context.Context) (map[string]vppLinkSnapshot, map[string]string, error) {
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	vppLinkCacheMu.Lock()
	if vppLinkCache.source == vppctl && time.Now().Before(vppLinkCache.expires) {
		byName, byPCI, err := vppLinkCache.byName, vppLinkCache.byPCI, vppLinkCache.err
		vppLinkCacheMu.Unlock()
		return byName, byPCI, err
	}
	vppLinkCacheMu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	output, err := execCommandOutput(readCtx, vppctl, "show", "hardware-interfaces")
	byName, byPCI := parseVPPHardwareLinks(output)

	vppLinkCacheMu.Lock()
	vppLinkCache = vppLinkCacheEntry{
		source:  vppctl,
		expires: time.Now().Add(vppLinkCacheTTL),
		byName:  byName,
		byPCI:   byPCI,
		err:     err,
	}
	vppLinkCacheMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	return byName, byPCI, nil
}

// vppLinkSnapshotFor resolves the VPP port that belongs to a product interface.
// A DPDK port is normally renamed to lyroute-<name>; during a rebuild VPP may
// still expose the PMD name, so the PCI address is the fallback locator.
func (server *Server) vppLinkSnapshotFor(ctx context.Context, name, pci string) (vppLinkSnapshot, bool) {
	byName, byPCI, err := server.vppLinkTelemetry(ctx)
	if err != nil {
		return vppLinkSnapshot{}, false
	}
	if snapshot, ok := byName["lyroute-"+strings.TrimSpace(name)]; ok {
		return snapshot, true
	}
	normalized := normalizePCIAddress(pci)
	if normalized == "" {
		return vppLinkSnapshot{}, false
	}
	if interfaceName, ok := byPCI[normalized]; ok {
		if snapshot, ok := byName[interfaceName]; ok {
			return snapshot, true
		}
	}
	return vppLinkSnapshot{}, false
}

// parseVPPHardwareLinks walks `show hardware-interfaces` blocks. A block header
// starts at column zero; carrier, Link speed and PCI address are indented
// underneath it. Returning both indexes lets the caller locate a port by its
// product name or by PCI after a restart changed the VPP name.
func parseVPPHardwareLinks(output string) (map[string]vppLinkSnapshot, map[string]string) {
	byName := map[string]vppLinkSnapshot{}
	byPCI := map[string]string{}
	current := ""
	block := []string{}
	flush := func() {
		if current == "" {
			return
		}
		text := strings.Join(block, "\n")
		snapshot := vppLinkSnapshot{
			Interface: current,
			Carrier:   "unknown",
			SpeedMbps: vppLinkSpeedMbps(text),
		}
		if carrier := vppHardwareCarrier(text); carrier == "up" || carrier == "down" {
			snapshot.Carrier = carrier
		}
		for _, line := range block {
			if snapshot.MAC == "" {
				if mac := vppHardwareMACAddress(line); mac != "" {
					snapshot.MAC = mac
				}
			}
			if pci := vppHardwarePCIAddress(line); pci != "" {
				snapshot.PCI = pci
				break
			}
		}
		byName[current] = snapshot
		if snapshot.PCI != "" {
			byPCI[snapshot.PCI] = current
		}
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			flush()
			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				current = ""
				block = nil
				continue
			}
			current = fields[0]
			block = nil
			continue
		}
		if current != "" {
			block = append(block, line)
		}
	}
	flush()
	return byName, byPCI
}

// vppHardwareMACAddress reads the MAC VPP reports for a DPDK port. A port bound
// to uio has no kernel netdev, so this is the only place the address still
// exists after ownership/inventory records lose it.
func vppHardwareMACAddress(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "Ethernet address") {
		return ""
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fields[2]))
}

func vppHardwarePCIAddress(line string) string {
	// Only the "pci:" line carries the PCI address. Matching the bare word
	// "address" also hits "Ethernet address <mac>" which appears earlier in
	// the block and would index the port by its MAC instead of its PCI slot,
	// leaving every DPDK port without live carrier/speed telemetry.
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "pci:") {
		return ""
	}
	fields := strings.Fields(trimmed)
	for index, field := range fields {
		if field == "address" && index+1 < len(fields) {
			return normalizePCIAddress(fields[index+1])
		}
	}
	return ""
}

// normalizePCIAddress makes sysfs ("0000:02:00.0") and VPP
// ("0000:02:00.00") spellings comparable.
func normalizePCIAddress(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "0000:")
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return value
	}
	bus := strings.TrimLeft(parts[0], "0")
	if bus == "" {
		bus = "0"
	}
	devFunc := strings.SplitN(parts[1], ".", 2)
	if len(devFunc) != 2 {
		return value
	}
	device := strings.TrimLeft(devFunc[0], "0")
	if device == "" {
		device = "0"
	}
	function := strings.TrimLeft(devFunc[1], "0")
	if function == "" {
		function = "0"
	}
	return bus + ":" + device + "." + function
}

// linkSpeedCapabilityMbps reports the PHY's maximum line rate. A DPDK port has
// no `speed` in sysfs, and VPP prints "Link speed: unknown" without carrier, so
// the capability is the only rate the UI can show for an unplugged 1GbE port.
func linkSpeedCapabilityMbps(pci string) int64 {
	pci = strings.TrimSpace(strings.ToLower(pci))
	if pci == "" {
		return 0
	}
	devicePath := filepath.Join(sysfsRoot(), "bus/pci/devices", pci)
	vendor := readSysfsTrimmed(filepath.Join(devicePath, "vendor"))
	device := readSysfsTrimmed(filepath.Join(devicePath, "device"))
	if speed := linkSpeedCapabilityForIDs(vendor, device); speed > 0 {
		return speed
	}
	driver := ""
	if resolved, err := filepath.EvalSymlinks(filepath.Join(devicePath, "driver")); err == nil {
		driver = filepath.Base(resolved)
	}
	return linkSpeedCapabilityForDriver(driver)
}

func readSysfsTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func linkSpeedCapabilityForIDs(vendor, device string) int64 {
	key := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(vendor), "0x")) + ":" +
		strings.ToLower(strings.TrimPrefix(strings.TrimSpace(device), "0x"))
	switch key {
	case "8086:1539", "8086:1533", "8086:1535", "8086:1521", "8086:1522",
		"8086:150e", "8086:10d3", "8086:15b7", "8086:15b8", "8086:15b9",
		"8086:15d8", "8086:15e3":
		// igb / e1000e (i210, i211, i219 and the J1900 onboard NICs).
		return 1000
	case "8086:15f3", "8086:15f2", "8086:125c", "8086:125d":
		// igc (i225/i226).
		return 2500
	case "8086:10fb", "8086:10f8", "8086:11fc", "8086:1563",
		"8086:1572", "8086:1573", "8086:1581":
		// ixgbe / i40e.
		return 10000
	}
	return 0
}

func linkSpeedCapabilityForDriver(driver string) int64 {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "igb", "e1000", "e1000e", "r8169", "tg3", "bnx2":
		return 1000
	case "igc":
		return 2500
	case "ixgbe", "i40e", "ice", "mlx4_core", "mlx5_core":
		return 10000
	}
	return 0
}
