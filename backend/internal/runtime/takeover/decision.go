package takeover

import "strings"

// Facts are the backend-owned observations the decision uses. There is
// deliberately no field for a client request: the API cannot influence the
// selected mode.
type Facts struct {
	Interface        string
	Driver           string
	VendorDevice     string
	Physical         bool
	Management       bool
	SharedManagement bool
	// RDMAReady is true when the VPP RDMA plugin is loaded and usable.
	RDMAReady bool
	// UIOAvailable/VFIOAvailable describe the kernel binding transports.
	UIOAvailable  bool
	VFIOAvailable bool
	// IOMMUIsolated is true when the device has its own intel IOMMU group.
	IOMMUIsolated bool
	// VFIOPolicyAllowed is the internal transport policy. VFIO is only ever
	// considered when it is isolated *and* this flag is set.
	VFIOPolicyAllowed bool
	DPDKPlugin        bool
	Hugepages         bool
}

// Candidate is one attempt in the ordered plan.
type Candidate struct {
	Mode      Mode
	Transport string
	Verified  bool
	Reason    string
}

// Plan is the ordered takeover plan for one port. It is always produced by the
// backend; the API only exposes it read-only.
type Plan struct {
	Interface  string
	RuleID     string
	RuleReason string
	// Skipped is non-empty when the port is deliberately not taken over
	// (management port, not a physical port).
	Skipped string
	// Dropped lists modes that a runtime record claimed but the rule library
	// refuses for this device family. It is diagnostic evidence: a non-empty
	// list means a stale verification/active record tried to drive the port.
	Dropped    []Mode
	Candidates []Candidate
}

// Primary returns the first candidate the engine should attempt.
func (plan Plan) Primary() (Candidate, bool) {
	if len(plan.Candidates) == 0 {
		return Candidate{}, false
	}
	return plan.Candidates[0], true
}

// Modes returns the ordered mode list without reasons. Tests and diagnostics
// use it; the runtime uses Candidates so it can record why.
func (plan Plan) Modes() []Mode {
	modes := make([]Mode, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		modes = append(modes, candidate.Mode)
	}
	return modes
}

// PrefersDPDK reports whether the plan starts with a DPDK attempt.
func (plan Plan) PrefersDPDK() bool {
	primary, ok := plan.Primary()
	if !ok {
		return false
	}
	return primary.Mode == ModeDPDKUIO || primary.Mode == ModeDPDKVFIO
}

// BuildPlan produces the fixed decision for one port.
//
// Order:
//  1. verified rule: AF_XDP zero-copy or RDMA is taken over directly;
//  2. DPDK (UIO first; VFIO only when the group is isolated and the internal
//     policy allows it);
//  3. AF_XDP copy mode;
//  4. AF_PACKET compatibility mode.
//
// The compatibility mode is always the final candidate, so a plan can never
// end without a path that keeps the port forwarding.
func BuildPlan(facts Facts) Plan {
	plan := Plan{Interface: strings.TrimSpace(facts.Interface)}
	if facts.Management && !facts.SharedManagement {
		plan.Skipped = "管理口不参与数据面接管"
		return plan
	}
	if !facts.Physical {
		plan.Skipped = "不是物理数据口，不参与接管"
		return plan
	}
	rule, ruleHit := MatchRule(facts.Driver, facts.VendorDevice)
	if ruleHit {
		plan.RuleID = rule.ID
		plan.RuleReason = rule.Reason
	}
	add := func(mode Mode, verified bool, reason string) {
		for _, existing := range plan.Candidates {
			if existing.Mode == mode {
				return
			}
		}
		plan.Candidates = append(plan.Candidates, Candidate{Mode: mode, Transport: mode.Transport(), Verified: verified, Reason: reason})
	}

	// Rule database first. Only a *verified* zero-copy/RDMA rule is taken over
	// directly, and only when the same rule also permits that mode. A stale
	// verification record cannot promote a mode the rule library forbids.
	if ruleHit && rule.Verified && rule.Allows(rule.Preferred) {
		switch rule.Preferred {
		case ModeRDMA:
			if facts.RDMAReady {
				add(ModeRDMA, true, rule.Reason)
			}
		case ModeAFXDPZeroCopy:
			add(ModeAFXDPZeroCopy, true, rule.Reason)
		}
	}

	dpdkEligible := dpdkPossible(facts, rule, ruleHit)
	if dpdkEligible {
		// UIO leads: it needs no IOMMU group and is the transport the product
		// verifies first. VFIO is a second DPDK transport, never the default.
		// When the UIO driver is unavailable the first DPDK transport is VFIO,
		// but only on an isolated group and only when policy allows it.
		uioAllowed := !ruleHit || rule.Allows(ModeDPDKUIO)
		vfioAllowed := (!ruleHit || rule.Allows(ModeDPDKVFIO)) && facts.VFIOAllowedNow()
		uioFirst := uioAllowed && (facts.UIOAvailable || !vfioAllowed)
		if uioFirst {
			add(ModeDPDKUIO, false, "DPDK UIO is the preferred high-performance transport")
		}
		if vfioAllowed {
			add(ModeDPDKVFIO, false, "DPDK VFIO is available on an isolated IOMMU group and allowed by policy")
		}
		if uioAllowed && !uioFirst {
			add(ModeDPDKUIO, false, "DPDK UIO remains the fallback transport")
		}
	} else if ruleHit && (rule.Preferred == ModeDPDKUIO || rule.Preferred == ModeDPDKVFIO) {
		// The device family has a DPDK rule but the environment cannot host
		// DPDK right now (plugin/hugepages). Do not attempt a bind that is
		// certain to fail; fall through to copy/compatibility.
		plan.RuleReason = strings.TrimSpace(plan.RuleReason + "; DPDK prerequisites are not met")
	}

	// AF_XDP copy is only a candidate when the rule library explicitly allows
	// it for this device family. Unknown hardware follows the documented
	// DPDK-PMD-or-AF_PACKET fallback and never receives an AF_XDP attempt: a
	// stale "af_xdp_copy verified" record is not evidence that the TX path works.
	if ruleHit && rule.Allows(ModeAFXDPCopy) {
		add(ModeAFXDPCopy, false, "AF_XDP copy mode keeps the kernel netdev")
	} else if ruleHit && rule.Forbids(ModeAFXDPCopy) {
		plan.Dropped = append(plan.Dropped, ModeAFXDPCopy)
	}
	// AF_PACKET is the guaranteed last resort: it must always exist so the
	// LAN/DHCP/DNS path can be restored even when every native/DPDK path fails.
	add(ModeAFPacket, false, "AF_PACKET compatibility mode (performance limited)")
	return plan
}

// VFIOAllowedNow is the single place that decides whether VFIO may be used.
func (facts Facts) VFIOAllowedNow() bool {
	return facts.VFIOAvailable && facts.IOMMUIsolated && facts.VFIOPolicyAllowed
}

// dpdkPossible reports whether a DPDK attempt makes sense for this port.
func dpdkPossible(facts Facts, rule Rule, ruleHit bool) bool {
	if facts.Driver == "" {
		return false
	}
	// Hugepages are a hard environment prerequisite: a bind without them can
	// only restart VPP and fail.
	if !facts.Hugepages {
		return false
	}
	if ruleHit {
		if rule.Preferred == ModeAFPacket {
			// Paravirtual/r8169-class NIC: no PCI port to bind.
			return false
		}
		if !rule.Allows(ModeDPDKUIO) && !rule.Allows(ModeDPDKVFIO) {
			return false
		}
		if rule.Preferred == ModeDPDKUIO || rule.Preferred == ModeDPDKVFIO {
			// The rule's first choice outranks the plugin flag. The DPDK
			// plugin is disabled in startup.conf until the first bind, so
			// requiring it here demoted every igb/igc port to AF_PACKET
			// before the rule-preferred attempt ever ran (WS-AK). The bind
			// re-enables the plugin and the transactional readback still
			// decides the takeover.
			return true
		}
		if !facts.DPDKPlugin {
			return false
		}
		return true
	}
	// Unknown hardware: the static PMD table is the only evidence.
	if !facts.DPDKPlugin {
		return false
	}
	return HasDPDKPMD(facts.Driver)
}

// Display classifies a plan for the read-only UI contract.
type Display string

const (
	DisplayHighPerformance Display = "high_performance"
	DisplayCompatibility   Display = "compatibility"
	DisplayRepair          Display = "needs_repair"
	DisplayManagement      Display = "management"
	DisplayPending         Display = "pending"
)

// DisplayFor maps an observed mode (and repair flag) to the three UI labels the
// product exposes: 高性能 / 兼容模式（性能受限）/ 修复中.
func DisplayFor(mode Mode, management, needsRepair bool) Display {
	switch {
	case management:
		return DisplayManagement
	case needsRepair:
		return DisplayRepair
	case mode == ModeAFPacket:
		return DisplayCompatibility
	case mode.Valid():
		return DisplayHighPerformance
	default:
		return DisplayPending
	}
}
