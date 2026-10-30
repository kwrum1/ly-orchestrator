// Package takeover owns the out-of-the-box takeover decision.
//
// The decision is backend-only. There is no operator switch, no consent flag and
// no client-supplied mode: every physical data port (the management port
// excluded) is taken over automatically, following the ordered fallback the
// product documents in docs/TAKEOVER-OUT-OF-BOX.md.
//
// This package is deliberately free of sysfs, VPP and HTTP dependencies so the
// ordering, the static rule library and the failure policy can be unit tested
// without a device.
package takeover

import "strings"

// Mode is the operator-facing takeover mode vocabulary. The strings are the
// same values the API, the journal and the UI use.
type Mode string

const (
	ModeAFXDPZeroCopy Mode = "af_xdp_zero_copy"
	ModeAFXDPCopy     Mode = "af_xdp_copy"
	ModeRDMA          Mode = "rdma"
	ModeDPDKUIO       Mode = "dpdk_uio"
	ModeDPDKVFIO      Mode = "dpdk_vfio"
	ModeAFPacket      Mode = "af_packet"
)

// Transport returns the DPDK PCI binding transport for a mode.
func (mode Mode) Transport() string {
	switch mode {
	case ModeDPDKVFIO:
		return "vfio"
	case ModeDPDKUIO:
		return "uio"
	}
	return ""
}

// HighPerformance reports whether the mode is a negotiated high-performance
// data path. AF_PACKET is the compatibility fallback and is never "high
// performance".
func (mode Mode) HighPerformance() bool {
	return mode != "" && mode != ModeAFPacket
}

// Valid reports whether the string is a mode this build implements.
func (mode Mode) Valid() bool {
	switch mode {
	case ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA, ModeDPDKUIO, ModeDPDKVFIO, ModeAFPacket:
		return true
	}
	return false
}

// Rule is one row of the static, built-in rule library.
//
// Only modes that were verified on the device family belong in the first
// priority slot (Verified). Everything else is a preference, not a proof: the
// runtime still takes the port over, but through the normal transactional
// attempt and fallback instead of a "no probe" fast path.
type Rule struct {
	ID        string
	Drivers   []string
	Devices   []string // vendor:device, lower case hex, optional
	Preferred Mode
	// Verified marks a mode that is proven for this device family. A verified
	// rule is taken over directly; it is never downgraded because a capability
	// probe has not run yet.
	Verified bool
	// AllowedModes is the complete set of modes the rule library permits for
	// this device family. An empty list keeps the legacy behaviour for rules
	// that have not been audited yet; a non-empty list is authoritative and no
	// stale verification, capability probe or active generation can add a mode
	// outside it.
	AllowedModes []Mode
	// ForbiddenModes names modes that must never be selected for this family,
	// even when a device-level proof claims otherwise. It is intentionally
	// redundant with AllowedModes for the NICs whose AF_XDP path is unsafe: a
	// future allow-list edit must not silently re-enable the crash.
	ForbiddenModes []Mode
	Reason         string
}

// Allows reports whether the rule library permits a mode for this family.
func (rule Rule) Allows(mode Mode) bool {
	mode = Mode(strings.ToLower(strings.TrimSpace(string(mode))))
	if mode == "" || !mode.Valid() {
		return false
	}
	for _, forbidden := range rule.ForbiddenModes {
		if forbidden == mode {
			return false
		}
	}
	if len(rule.AllowedModes) == 0 {
		return true
	}
	for _, allowed := range rule.AllowedModes {
		if allowed == mode {
			return true
		}
	}
	return false
}

// Forbids reports whether the rule explicitly bans a mode.
func (rule Rule) Forbids(mode Mode) bool {
	for _, forbidden := range rule.ForbiddenModes {
		if forbidden == mode {
			return true
		}
	}
	return false
}

func (rule Rule) matchesDriver(driver string) bool {
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver == "" {
		return false
	}
	for _, candidate := range rule.Drivers {
		if strings.ToLower(strings.TrimSpace(candidate)) == driver {
			return true
		}
	}
	return false
}

func (rule Rule) matchesDevice(device string) bool {
	device = strings.ToLower(strings.TrimSpace(device))
	if device == "" || len(rule.Devices) == 0 {
		return false
	}
	for _, candidate := range rule.Devices {
		if strings.ToLower(strings.TrimSpace(candidate)) == device {
			return true
		}
	}
	return false
}

// builtinRules is the complete rule library shipped in the image. It is small
// on purpose: only device families the project has actually run are listed, and
// only verified zero-copy/RDMA families are marked Verified.
var builtinRules = []Rule{
	{
		ID:        "mlx5-connectx4-5-rdma",
		Drivers:   []string{"mlx5_core"},
		Devices:   []string{"15b3:1013", "15b3:1014", "15b3:1015", "15b3:1016", "15b3:1017", "15b3:1018", "15b3:1019", "15b3:101a", "15b3:101b", "15b3:101c", "15b3:101d", "15b3:101e", "15b3:101f"},
		Preferred: ModeRDMA,
		Verified:  true,
		Reason:    "Mellanox ConnectX-4/5 with the VPP RDMA plugin is verified",
	},
	{
		ID:             "igb-dpdk-uio",
		Drivers:        []string{"igb"},
		Devices:        []string{"8086:1533", "8086:1539", "8086:1521", "8086:1522", "8086:1523", "8086:1524", "8086:1525", "8086:1526", "8086:150e", "8086:10d3"},
		Preferred:      ModeDPDKUIO,
		AllowedModes:   []Mode{ModeDPDKUIO, ModeAFPacket},
		ForbiddenModes: []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA, ModeDPDKVFIO},
		Reason:         "igb (I210/I211/I350) has a DPDK PMD; AF_XDP is not verified and its TX path is unsafe on this family",
	},
	{
		ID:             "igc-dpdk-uio",
		Drivers:        []string{"igc"},
		Devices:        []string{"8086:15f2", "8086:15f3", "8086:15f8", "8086:15f9", "8086:125b", "8086:125c", "8086:125d", "8086:125e", "8086:125f"},
		Preferred:      ModeDPDKUIO,
		AllowedModes:   []Mode{ModeDPDKUIO, ModeAFPacket},
		ForbiddenModes: []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA, ModeDPDKVFIO},
		Reason:         "igc (I225/I226) has a DPDK PMD; AF_XDP copy TX crashed VPP on this family and is forbidden",
	},
	{
		ID:        "ixgbe-dpdk-uio",
		Drivers:   []string{"ixgbe", "ixgbevf"},
		Preferred: ModeDPDKUIO,
		Reason:    "ixgbe has a DPDK PMD; zero-copy is only used when a probe proved it on this exact model",
	},
	{
		ID:        "i40e-dpdk-uio",
		Drivers:   []string{"i40e"},
		Preferred: ModeDPDKUIO,
		Reason:    "i40e has a DPDK PMD; zero-copy is only used when a probe proved it on this exact model",
	},
	{
		ID:        "ice-dpdk-uio",
		Drivers:   []string{"ice"},
		Preferred: ModeDPDKUIO,
		Reason:    "ice has a DPDK PMD; zero-copy is only used when a probe proved it on this exact model",
	},
	{
		ID:             "e1000e-dpdk-uio",
		Drivers:        []string{"e1000e"},
		Preferred:      ModeDPDKUIO,
		AllowedModes:   []Mode{ModeDPDKUIO, ModeAFPacket},
		ForbiddenModes: []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA, ModeDPDKVFIO},
		Reason:         "e1000e has a DPDK PMD and no verified, safe AF_XDP path",
	},
	{
		ID:             "r8169-compat",
		Drivers:        []string{"r8169"},
		Preferred:      ModeAFPacket,
		AllowedModes:   []Mode{ModeAFPacket},
		ForbiddenModes: []Mode{ModeAFXDPZeroCopy, ModeAFXDPCopy, ModeRDMA, ModeDPDKUIO, ModeDPDKVFIO},
		Reason:         "r8169 has no supported PMD and no verified AF_XDP path; only the compatibility mode is safe",
	},
	// Virtual NICs are physical netdevs only from the kernel's point of view.
	// They have no PCI data port to bind and no verified kernel-bypass path, so
	// the compatibility mode is the only supported takeover.
	{
		ID:           "virtio-compat",
		Drivers:      []string{"virtio_net", "virtio-net"},
		Preferred:    ModeAFPacket,
		AllowedModes: []Mode{ModeAFPacket},
		Reason:       "virtio-net is a paravirtual NIC; only the compatibility mode is supported",
	},
	{
		ID:           "vmxnet3-compat",
		Drivers:      []string{"vmxnet3"},
		Preferred:    ModeAFPacket,
		AllowedModes: []Mode{ModeAFPacket},
		Reason:       "vmxnet3 is a paravirtual NIC; only the compatibility mode is supported",
	},
	{
		ID:           "hyperv-compat",
		Drivers:      []string{"hv_netvsc"},
		Preferred:    ModeAFPacket,
		AllowedModes: []Mode{ModeAFPacket},
		Reason:       "Hyper-V netvsc is a paravirtual NIC; only the compatibility mode is supported",
	},
	{
		ID:           "xen-compat",
		Drivers:      []string{"xen-netfront", "xennet"},
		Preferred:    ModeAFPacket,
		AllowedModes: []Mode{ModeAFPacket},
		Reason:       "Xen netfront is a paravirtual NIC; only the compatibility mode is supported",
	},
}

// dpdkPMDDrivers is the static DPDK PMD table used for NICs the rule library
// does not know. A driver with a shipped PMD is eligible for DPDK; anything
// else falls through to the compatibility path.
var dpdkPMDDrivers = map[string]bool{
	"igb":       true,
	"igc":       true,
	"ixgbe":     true,
	"ixgbevf":   true,
	"i40e":      true,
	"iavf":      true,
	"ice":       true,
	"e1000":     true,
	"e1000e":    true,
	"mlx4_core": true,
	"mlx5_core": true,
	"bnxt_en":   true,
	"qede":      true,
	"sfc":       true,
	"atlantic":  true,
	"txgbe":     true,
	"ngbe":      true,
	"txgbevf":   true,
	"hinic":     true,
	"nfb":       true,
}

// ParseDeviceID normalises a "vendor:device" pair as sysfs prints it
// ("0x8086 0x125c" or "8086:125c") into the lower-case "8086:125c" form.
func ParseDeviceID(vendor, device string) string {
	vendor = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(vendor), "0x")))
	device = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(device), "0x")))
	if vendor == "" || device == "" {
		return ""
	}
	return vendor + ":" + device
}

// MatchRule returns the first rule matching the driver or the PCI device id.
// Driver matches are exact; a device id match also requires the rule's driver
// list to contain the observed driver when the rule lists one, so a re-used
// vendor id cannot select a rule for unrelated hardware.
func MatchRule(driver, vendorDevice string) (Rule, bool) {
	for _, rule := range builtinRules {
		if len(rule.Devices) > 0 && rule.matchesDevice(vendorDevice) {
			if len(rule.Drivers) == 0 || rule.matchesDriver(driver) {
				return rule, true
			}
			continue
		}
		if rule.matchesDriver(driver) {
			return rule, true
		}
	}
	return Rule{}, false
}

// ModeAllowed is the single authority for "may this port use this mode?".
//
// For a driver/device that matches the rule library the rule's allow-list (and
// explicit forbid-list) decides. Unknown hardware follows the documented
// fallback: a driver with a DPDK PMD may use DPDK, everything else falls back
// to AF_PACKET. AF_PACKET is always permitted because it is the compatibility
// mode that keeps the port forwarding, and it is the only mode that can be
// assumed for hardware whose native path is unknown.
//
// A stale dpdk-verification.json or active.json entry is not input to this
// function: records are observations, the rule library is the policy.
func ModeAllowed(driver, vendorDevice string, mode Mode) bool {
	mode = Mode(strings.ToLower(strings.TrimSpace(string(mode))))
	if mode == "" || !mode.Valid() {
		return false
	}
	if mode == ModeAFPacket {
		return true
	}
	if rule, ok := MatchRule(driver, vendorDevice); ok {
		return rule.Allows(mode)
	}
	if mode == ModeDPDKUIO || mode == ModeDPDKVFIO {
		return HasDPDKPMD(driver)
	}
	return false
}

// ModeAllowedByRule is ModeAllowed for callers that already matched the rule.
func ModeAllowedByRule(rule Rule, matched bool, driver string, mode Mode) bool {
	mode = Mode(strings.ToLower(strings.TrimSpace(string(mode))))
	if mode == "" || !mode.Valid() {
		return false
	}
	if mode == ModeAFPacket {
		return true
	}
	if matched {
		return rule.Allows(mode)
	}
	if mode == ModeDPDKUIO || mode == ModeDPDKVFIO {
		return HasDPDKPMD(driver)
	}
	return false
}

// HasDPDKPMD reports whether the static PMD table lists this kernel driver.
func HasDPDKPMD(driver string) bool {
	return dpdkPMDDrivers[strings.ToLower(strings.TrimSpace(driver))]
}

// Rules returns a copy of the rule library for diagnostics and tests.
func Rules() []Rule {
	rules := make([]Rule, len(builtinRules))
	copy(rules, builtinRules)
	return rules
}

// PMDDrivers returns a copy of the DPDK PMD table for diagnostics and tests.
func PMDDrivers() []string {
	drivers := make([]string, 0, len(dpdkPMDDrivers))
	for driver := range dpdkPMDDrivers {
		drivers = append(drivers, driver)
	}
	return drivers
}
