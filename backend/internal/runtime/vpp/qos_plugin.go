package vpp

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/flow"
)

// QoSTimeout bounds one capability probe. The probe only runs read-only CLI
// commands and must never hold an apply plan for long.
const QoSTimeout = 5 * time.Second

// Operator-facing names of the three dataplane facilities traffic-control
// compiles into. They are separate because a build can provide one without the
// others, and a policy must only be accepted when every facility it needs is
// really present.
const (
	CoreQoSRequirement  = "VPP core QoS CLI (qos record/store/egress-map/mark)"
	FlowRateRequirement = "ly_route_smart_qos_plugin.so (set ly-route flow-rate)"
	ShapingRequirement  = "ly_route_smart_qos_plugin.so fq-codel shaping (set ly-route smart-qos)"
	PolicerRequirement  = "policer_plugin.so (policer add/input/output)"
)

// QoSCapability is the observed truth about what the running VPP build can
// enforce for traffic-control.
//
// The three facts are probed independently:
//
//   - CoreQoS: `qos record/store/egress-map/mark`. VPP 26.06 folded the former
//     qos_plugin into vnet core, so these commands exist in every 26.06 build
//     and are *not* listed by `show plugins`. Older builds advertise them as
//     qos_plugin.so; the plugin inventory is accepted as equivalent evidence.
//     Probing for qos_plugin.so alone (the pre-fix behaviour) reports every
//     26.06 build as unable to do QoS, which is wrong.
//   - FlowRate: per-rule token-bucket policing via `set ly-route flow-rate`.
//     Built-in Smart QoS shaping is retired at product level, but the same
//     plugin (ly_route_smart_qos_plugin.so) owns the traffic-control rate rule,
//     so a dataplane image that omits it cannot enforce rate limits.
//   - Shaping: interface-level fq-codel queueing via
//     `set ly-route smart-qos interface ... rate`. The command is compiled into
//     the same plugin as flow-rate; a build that ships an older plugin without
//     the shaping CLI must report it as missing instead of accepting a policy
//     that cannot be enforced.
//   - Policer: interface-level 1r2c policing via policer_plugin.so, used by
//     policies that rate-limit a whole attachment without a match.
//
// Unknown is a first-class value: a probe that cannot run must never be
// reported as supported.
type QoSCapability struct {
	CoreQoS  CapabilityFact `json:"core_qos"`
	FlowRate CapabilityFact `json:"flow_rate"`
	Shaping  CapabilityFact `json:"shaping"`
	Policer  CapabilityFact `json:"policer"`
	Source   string         `json:"source,omitempty"`
}

// FullQoSCapability is the capability of a dataplane that provides every
// traffic-control facility. Tests and image gates use it as the positive
// reference.
func FullQoSCapability() QoSCapability {
	return QoSCapability{
		CoreQoS:  KnownFact(true),
		FlowRate: KnownFact(true),
		Shaping:  KnownFact(true),
		Policer:  KnownFact(true),
		Source:   "declared",
	}
}

// UnsupportedQoSCapability builds an all-missing capability with an explicit
// reason per facility. Used when a caller already knows the dataplane cannot
// enforce anything (tests, image gates).
func UnsupportedQoSCapability(reason string) QoSCapability {
	reason = strings.TrimSpace(reason)
	missing := CapabilityFact{Known: true, Detail: reason}
	return QoSCapability{
		CoreQoS:  missing,
		FlowRate: missing,
		Shaping:  missing,
		Policer:  missing,
		Source:   "declared",
	}
}

// Status collapses the facts into the four-state capability contract. Any
// known-missing facility is unsupported; all facts must be positive for
// supported; otherwise the answer is unknown.
func (capability QoSCapability) Status() CapabilityStatus {
	facts := []CapabilityFact{capability.CoreQoS, capability.FlowRate, capability.Shaping, capability.Policer}
	allKnown := true
	for _, fact := range facts {
		if !fact.Known {
			allKnown = false
			continue
		}
		if !fact.Value {
			return CapabilityUnsupported
		}
	}
	if allKnown {
		return CapabilitySupported
	}
	return CapabilityUnknown
}

// Missing lists the facilities this build does not provide. It is empty when
// nothing is known to be missing, including the unknown case: callers that
// must fail closed have to check Status as well.
func (capability QoSCapability) Missing() []string {
	missing := make([]string, 0, 4)
	if capability.CoreQoS.Known && !capability.CoreQoS.Value {
		missing = append(missing, CoreQoSRequirement)
	}
	if capability.FlowRate.Known && !capability.FlowRate.Value {
		missing = append(missing, FlowRateRequirement)
	}
	if capability.Shaping.Known && !capability.Shaping.Value {
		missing = append(missing, ShapingRequirement)
	}
	if capability.Policer.Known && !capability.Policer.Value {
		missing = append(missing, PolicerRequirement)
	}
	return missing
}

// Summary is the operator-facing sentence used by runtime/status and by
// validation errors.
func (capability QoSCapability) Summary() string {
	switch capability.Status() {
	case CapabilitySupported:
		return "traffic-control dataplane verified: VPP core QoS, ly_route_smart_qos_plugin.so flow-rate and fq-codel shaping, and policer_plugin.so are available"
	case CapabilityUnsupported:
		return "traffic-control unsupported: missing " + strings.Join(capability.Missing(), ", ")
	default:
		details := make([]string, 0, 4)
		for _, item := range []struct {
			name string
			fact CapabilityFact
		}{
			{CoreQoSRequirement, capability.CoreQoS},
			{FlowRateRequirement, capability.FlowRate},
			{ShapingRequirement, capability.Shaping},
			{PolicerRequirement, capability.Policer},
		} {
			if !item.fact.Known {
				detail := strings.TrimSpace(item.fact.Detail)
				if detail == "" {
					detail = "not observed"
				}
				details = append(details, item.name+": "+detail)
			}
		}
		return "traffic-control capability unknown: " + strings.Join(details, "; ")
	}
}

// qosRequirementsForKind maps a compiled traffic-control object kind to the
// dataplane facilities it needs. ACL-backed blocking needs none of them: it
// runs on acl_plugin, which is part of the base dataplane.
func qosRequirementsForKind(kind string) []string {
	switch strings.TrimSpace(kind) {
	case "vpp.qos.classify", "vpp.qos.record", "vpp.qos.store", "vpp.qos.egress-map", "vpp.qos.mark":
		return []string{CoreQoSRequirement}
	case "vpp.behavior.rate":
		return []string{FlowRateRequirement}
	case "vpp.shape":
		return []string{ShapingRequirement}
	case "vpp.policer":
		return []string{PolicerRequirement}
	default:
		return nil
	}
}

func (capability QoSCapability) available(requirement string) bool {
	switch requirement {
	case CoreQoSRequirement:
		return capability.CoreQoS.Known && capability.CoreQoS.Value
	case FlowRateRequirement:
		return capability.FlowRate.Known && capability.FlowRate.Value
	case ShapingRequirement:
		return capability.Shaping.Known && capability.Shaping.Value
	case PolicerRequirement:
		return capability.Policer.Known && capability.Policer.Value
	default:
		return true
	}
}

// MissingQoSCapabilities returns the facilities required by the compiled object
// groups that the running build does not provide. A nil result means every
// requested action can be enforced (or that the request needs no QoS facility).
func MissingQoSCapabilities(capability QoSCapability, groups []flow.VPPObjectGroup) []string {
	seen := map[string]struct{}{}
	missing := make([]string, 0, 4)
	for _, group := range groups {
		for _, requirement := range qosRequirementsForKind(group.Kind) {
			if capability.available(requirement) {
				continue
			}
			if _, duplicate := seen[requirement]; duplicate {
				continue
			}
			seen[requirement] = struct{}{}
			missing = append(missing, requirement)
		}
	}
	sort.Strings(missing)
	return missing
}

// SupportedQoSGroups keeps the object groups the running build can enforce and
// returns the ones it cannot, so a plan can drop the unenforceable subset
// instead of failing the whole generation or silently keeping dead objects.
func SupportedQoSGroups(capability QoSCapability, groups []flow.VPPObjectGroup) (supported, unsupported []flow.VPPObjectGroup, missing []string) {
	seen := map[string]struct{}{}
	supported = make([]flow.VPPObjectGroup, 0, len(groups))
	for _, group := range groups {
		groupMissing := MissingQoSCapabilities(capability, []flow.VPPObjectGroup{group})
		if len(groupMissing) > 0 {
			unsupported = append(unsupported, group)
			for _, requirement := range groupMissing {
				if _, duplicate := seen[requirement]; duplicate {
					continue
				}
				seen[requirement] = struct{}{}
				missing = append(missing, requirement)
			}
			continue
		}
		supported = append(supported, group)
	}
	sort.Strings(missing)
	return supported, unsupported, missing
}

// SupportedQoSTargets mirrors SupportedQoSGroups for the flat target list that
// BuildOperations consumes.
func SupportedQoSTargets(capability QoSCapability, targets []flow.Target) (supported, unsupported []flow.Target) {
	supported = make([]flow.Target, 0, len(targets))
	for _, target := range targets {
		if len(MissingQoSCapabilities(capability, []flow.VPPObjectGroup{{Kind: target.Kind}})) > 0 {
			unsupported = append(unsupported, target)
			continue
		}
		supported = append(supported, target)
	}
	return supported, unsupported
}

// ProbeQoSCapability observes the running VPP build. It is read-only: one
// `show plugins` inventory and, when the inventory does not already prove the
// core QoS commands, one argument-less `qos record` that fails with a usage
// error instead of touching dataplane state.
func ProbeQoSCapability(ctx context.Context, binary string) (QoSCapability, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "vppctl"
	}
	probeCtx, cancel := context.WithTimeout(ctx, QoSTimeout)
	defer cancel()
	capability := QoSCapability{Source: "runtime_probe"}
	output, err := exec.CommandContext(probeCtx, binary, "show", "plugins").Output()
	if err != nil {
		return capability, fmt.Errorf("probe VPP plugin inventory: %w", err)
	}
	inventory := string(output)
	capability.FlowRate = KnownFact(pluginLoaded(inventory, "ly_route_smart_qos_plugin.so"))
	capability.Policer = KnownFact(pluginLoaded(inventory, "policer_plugin.so"))
	shaping, shapingErr := probeShapeQoS(probeCtx, binary)
	if shapingErr != nil {
		return capability, shapingErr
	}
	capability.Shaping = shaping
	if pluginLoaded(inventory, "qos_plugin.so") {
		// Pre-26.06 builds still ship the QoS features in a plugin.
		capability.CoreQoS = KnownFact(true)
		return capability, nil
	}
	core, err := probeCoreQoS(probeCtx, binary)
	if err != nil {
		return capability, err
	}
	capability.CoreQoS = core
	return capability, nil
}

// pluginLoaded reports whether the `show plugins` inventory carries exactly
// this plugin. A substring match is wrong: ly_route_smart_qos_plugin.so also
// contains "qos_plugin.so", which would make every product build look like it
// ships the retired VPP QoS plugin.
func pluginLoaded(inventory, name string) bool {
	for _, field := range strings.Fields(inventory) {
		if strings.Trim(field, ",") == name {
			return true
		}
	}
	return false
}

// probeCoreQoS distinguishes "command exists and rejected the missing
// interface" from "command does not exist". VPP prints CLI parse errors on
// stdout and exits 0, so the text is the authoritative signal.
// probeShapeQoS proves the fq-codel shaping CLI exists and self-identifies.
// `state locked` is the normal state before the first interface is shaped, so
// only the algorithm/qualification contract decides availability.
func probeShapeQoS(ctx context.Context, binary string) (CapabilityFact, error) {
	output, err := exec.CommandContext(ctx, binary, "show", "ly-route", "smart-qos").CombinedOutput()
	if err != nil {
		return UnknownFact("vppctl show ly-route smart-qos failed: " + err.Error()), fmt.Errorf("probe VPP fq-codel shaping CLI: %w", err)
	}
	text := strings.TrimSpace(string(output))
	switch {
	case text == "":
		return UnknownFact("vppctl show ly-route smart-qos produced no output"), nil
	case strings.Contains(text, "unknown input"), strings.Contains(text, "unknown CLI command"), strings.Contains(text, "no such command"):
		return KnownFact(false), nil
	case strings.Contains(text, "algorithm fq-codel") && strings.Contains(text, "qualification production"):
		return KnownFact(true), nil
	default:
		return KnownFact(false), nil
	}
}

func probeCoreQoS(ctx context.Context, binary string) (CapabilityFact, error) {
	output, err := exec.CommandContext(ctx, binary, "qos", "record").CombinedOutput()
	if err != nil {
		return UnknownFact("vppctl qos record failed: " + err.Error()), fmt.Errorf("probe VPP core QoS CLI: %w", err)
	}
	text := strings.TrimSpace(string(output))
	switch {
	case text == "":
		return UnknownFact("vppctl qos record produced no output"), nil
	case strings.Contains(text, "unknown input"), strings.Contains(text, "unknown CLI command"), strings.Contains(text, "no such command"):
		return KnownFact(false), nil
	default:
		// Any other answer proves the CLI command exists: VPP printed a parse
		// or usage error instead of "unknown input".
		return KnownFact(true), nil
	}
}
