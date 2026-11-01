package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// afXDPPluginRecheckInterval bounds how often the plugin inventory is re-read
// while the interface set is unchanged. A plugin set only changes when VPP
// starts, but the recheck also covers a build that loads a plugin later. It
// never causes `show af_xdp` to run unless the inventory actually changed.
const afXDPPluginRecheckInterval = 60 * time.Second

// afXDPProbeCache caches the optional `show af_xdp` audit probe.
//
// On VPP 26.06 the AF_XDP plugin is loaded while the `show af_xdp` CLI is gone,
// so every call logs "unknown input 'af_xdp'". Status polling runs every few
// seconds; probing on each poll produced hundreds of log lines per minute and
// told us nothing. Queue fds are an audit hint, never proof, so probe once and
// invalidate only when the answer could have changed:
//
//   - a different VPP process (restart/reload),
//   - a different host interface or PCI set (NIC added/removed/moved),
//   - a different plugin inventory.
//
// Hardware and VPP identity are read from the local filesystem, so a cache hit
// issues no VPP CLI command at all. The plugin inventory is re-read at most
// once per afXDPPluginRecheckInterval; an unchanged inventory issues no
// `show af_xdp`.
type afXDPProbeCache struct {
	mu sync.Mutex

	fingerprint       string
	pluginFingerprint string
	pluginCheckedAt   time.Time
	probedAt          time.Time
	probed            bool
	fds               []int

	// Injectable for tests. Production leaves these nil.
	fingerprintFn func() string
	pluginFn      func(context.Context, string) (string, bool, error)
	queueFn       func(context.Context, string) ([]int, error)
	now           func() time.Time
}

func (cache *afXDPProbeCache) init() {
	if cache.fingerprintFn == nil {
		cache.fingerprintFn = afXDPFingerprint
	}
	if cache.pluginFn == nil {
		cache.pluginFn = probeAFXDPPluginInventory
	}
	if cache.queueFn == nil {
		cache.queueFn = probeAFXDPQueueFDs
	}
	if cache.now == nil {
		cache.now = time.Now
	}
}

// queueFDs returns the cached AF_XDP queue fds. force bypasses the cached
// verdict for a takeover transition, which has just rewritten the attachment.
// The plugin inventory still gates the probe: a build without the AF_XDP
// plugin never sees `show af_xdp`.
func (cache *afXDPProbeCache) queueFDs(ctx context.Context, vppctl string, force bool) []int {
	cache.init()
	now := cache.now()
	fingerprint := cache.fingerprintFn()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	first := !cache.probed
	fingerprintChanged := fingerprint != "" && fingerprint != cache.fingerprint
	recheck := cache.pluginCheckedAt.IsZero() || now.Sub(cache.pluginCheckedAt) >= afXDPPluginRecheckInterval
	if !force && !first && !fingerprintChanged && !recheck {
		return cloneQueueFDs(cache.fds)
	}

	pluginFingerprint, loaded, err := cache.pluginFn(ctx, vppctl)
	if err != nil {
		// A failed plugin read must not make the next status poll probe
		// again. The 60s plugin recheck or a VPP/hardware change retries.
		cache.fingerprint = fingerprint
		cache.pluginCheckedAt = now
		cache.probedAt = now
		cache.probed = true
		cache.fds = nil
		return nil
	}
	pluginChanged := cache.pluginFingerprint != pluginFingerprint
	cache.pluginFingerprint = pluginFingerprint
	cache.pluginCheckedAt = now

	needQueueProbe := force || first || fingerprintChanged || pluginChanged
	if !loaded {
		// No plugin means the CLI does not exist; never emit the noisy probe.
		cache.fingerprint = fingerprint
		cache.fds = nil
		cache.probed = true
		if needQueueProbe {
			cache.probedAt = now
		}
		return nil
	}
	if needQueueProbe {
		fds, queueErr := cache.queueFn(ctx, vppctl)
		cache.probedAt = now
		cache.probed = true
		if queueErr != nil {
			// Cache the absence as well. VPP 26.06 removed the CLI even
			// though the plugin is loaded, and retrying every poll is exactly
			// the noise this cache exists to prevent.
			cache.fds = nil
		} else {
			cache.fds = fds
		}
	}
	cache.fingerprint = fingerprint
	return cloneQueueFDs(cache.fds)
}

func cloneQueueFDs(fds []int) []int {
	if len(fds) == 0 {
		return nil
	}
	return append([]int(nil), fds...)
}

// afXDPFingerprint identifies the running VPP process and the host interface
// set. Both parts are read from local files; no VPP CLI is involved.
func afXDPFingerprint() string {
	parts := []string{
		"vpp=" + strings.TrimSpace(vppProcessRuntimeID()),
		"net=" + afXDPInterfaceFingerprint(),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// afXDPInterfaceFingerprint lists every netdev and, when it is a physical
// device, the PCI address behind it. A NIC added, removed or moved to another
// PCI slot changes the string.
func afXDPInterfaceFingerprint() string {
	root := filepath.Join(sysfsRoot(), "class", "net")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		pci := ""
		if resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(root, name, "device")); resolveErr == nil {
			pci = filepath.Base(resolved)
		}
		rows = append(rows, name+"="+pci)
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// probeAFXDPPluginInventory fingerprints the plugin list and reports whether
// the AF_XDP plugin is present. The fingerprint is order-insensitive so a
// rebuild that reorders the same plugin set does not force a dataplane probe.
func probeAFXDPPluginInventory(ctx context.Context, vppctl string) (string, bool, error) {
	output, err := execCommandOutput(ctx, vppctl, "show", "plugins")
	if err != nil {
		return "", false, err
	}
	loaded := false
	lines := make([]string, 0, 16)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "af_xdp") || strings.Contains(lower, "af-xdp") || strings.Contains(lower, "af xdp") {
			loaded = true
		}
		lines = append(lines, lower)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), loaded, nil
}

// probeAFXDPQueueFDs performs the one optional `show af_xdp` read. The log
// line is intentional: it is the observable probe counter for acceptance.
func probeAFXDPQueueFDs(ctx context.Context, vppctl string) ([]int, error) {
	log.Printf("takeover: probing show af_xdp (optional audit hint; cached until the VPP process, interface set or plugin inventory changes)")
	output, err := execCommandOutput(ctx, vppctl, "show", "af_xdp")
	if err != nil {
		return nil, err
	}
	return parseAFXDPQueueFDs(output), nil
}
