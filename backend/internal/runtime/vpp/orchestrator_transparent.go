package vpp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"ly-route/backend/internal/orchestrator"
)

const maxTransparentOrchestratorRuleVariants = 4096

type TransparentOrchestratorConfig struct {
	Generation string
	Topology   orchestrator.TopologyView
	Policy     *orchestrator.PolicyView
}

type TransparentOrchestratorReadback struct {
	Generation string `json:"generation"`
	State      string `json:"state"`
	// Default is the terminal action the plugin is actually enforcing.  It is
	// read back from `show ly-route orchestrator` so the control plane can
	// compare the configuration's default action with the data-plane one
	// instead of trusting a generation string alone.
	Default string `json:"default,omitempty"`
	// Bypass reflects the fault-bypass flag inside the plugin.  Bypass does
	// not change the committed generation; it only makes the data path skip
	// policy evaluation until the flag is cleared.
	Bypass        bool   `json:"bypass"`
	BypassCause   string `json:"bypass_cause,omitempty"`
	BypassReason  string `json:"bypass_reason,omitempty"`
	BypassForced  bool   `json:"bypass_forced,omitempty"`
	BypassPackets uint64 `json:"bypass_packets,omitempty"`
	// GroupBypass and GroupProbes are the per-group view reported by newer
	// plugin builds.  Older builds report neither and the slices stay empty.
	GroupBypass  []GroupBypassReadback `json:"group_bypass,omitempty"`
	GroupProbes  []GroupProbeReadback  `json:"group_probes,omitempty"`
	ProbeTargets int                   `json:"probe_targets,omitempty"`
	Output       string                `json:"output"`
}

// GroupBypassReadback is one `group-bypass` line from the plugin.
type GroupBypassReadback struct {
	Name    string
	Enabled bool
	Forced  bool
	Cause   string
	Reason  string
	Packets uint64
}

// GroupProbeReadback is one `group-probe` line from the plugin.  The counters
// are cumulative since the plugin loaded or since the passive target was
// (re)configured; the control plane derives the window from deltas.
type GroupProbeReadback struct {
	Group           string
	DstIP           string
	DstPort         int
	Protocol        string
	Attempts        uint64
	Syn             uint64
	SynAck          uint64
	RST             uint64
	Responses       uint64
	ICMPUnreachable uint64
	WindowSeconds   float64
}

// GroupProbeTarget installs one passive request/response tuple inside the
// plugin for a group.
type GroupProbeTarget struct {
	Group         string
	DstIP         string
	DstPort       int
	Protocol      string
	WindowSeconds float64
}


// ParseTransparentOrchestratorBypass extracts the fault-bypass state from
// `show ly-route orchestrator` output.  Older plugin builds do not print these
// lines, in which case the bypass is reported as off.
func ParseTransparentOrchestratorBypass(output string) (enabled bool, cause, reason string, forced bool, packets uint64) {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "bypass ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		switch fields[1] {
		case "state":
			enabled = len(fields) >= 3 && fields[2] == "on"
		case "cause":
			if len(fields) >= 3 {
				cause = fields[2]
			}
		case "reason":
			if len(trimmed) > len("bypass reason ") {
				reason = strings.TrimSpace(trimmed[len("bypass reason "):])
			}
		case "forced":
			forced = len(fields) >= 3 && fields[2] == "yes"
		case "packets":
			if len(fields) >= 3 {
				packets, _ = strconv.ParseUint(fields[2], 10, 64)
			}
		}
	}
	if cause == "none" {
		cause = ""
	}
	if reason == "none" {
		reason = ""
	}
	return enabled, cause, reason, forced, packets
}

// ParseTransparentOrchestratorGroupBypass extracts the per-group bypass rows
// from `show ly-route orchestrator` output:
//
//	group-bypass grp-b state on forced no cause auto packets 12 reason probe_failed
func ParseTransparentOrchestratorGroupBypass(output string) []GroupBypassReadback {
	result := []GroupBypassReadback{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 || fields[0] != "group-bypass" {
			continue
		}
		row := GroupBypassReadback{Name: fields[1]}
		for index := 2; index+1 < len(fields); index++ {
			switch fields[index] {
			case "state":
				row.Enabled = fields[index+1] == "on"
				index++
			case "forced":
				row.Forced = fields[index+1] == "yes"
				index++
			case "cause":
				row.Cause = fields[index+1]
				index++
			case "packets":
				row.Packets, _ = strconv.ParseUint(fields[index+1], 10, 64)
				index++
			case "reason":
				row.Reason = strings.Join(fields[index+1:], " ")
				index = len(fields)
			}
		}
		if row.Cause == "none" {
			row.Cause = ""
		}
		if row.Reason == "none" {
			row.Reason = ""
		}
		result = append(result, row)
	}
	return result
}

// ParseTransparentOrchestratorGroupProbes extracts the passive-analysis rows:
//
//	group-probe grp-a dst 192.168.10.82 port 53 proto udp attempts 4 syn 0
//	  synack 0 rst 0 responses 2 icmp-unreachable 0 window 10
func ParseTransparentOrchestratorGroupProbes(output string) []GroupProbeReadback {
	result := []GroupProbeReadback{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 || fields[0] != "group-probe" {
			continue
		}
		row := GroupProbeReadback{Group: fields[1]}
		for index := 2; index+1 < len(fields); index++ {
			value := fields[index+1]
			switch fields[index] {
			case "dst":
				row.DstIP = value
			case "port":
				row.DstPort, _ = strconv.Atoi(value)
			case "proto":
				row.Protocol = value
			case "attempts":
				row.Attempts, _ = strconv.ParseUint(value, 10, 64)
			case "syn":
				row.Syn, _ = strconv.ParseUint(value, 10, 64)
			case "synack":
				row.SynAck, _ = strconv.ParseUint(value, 10, 64)
			case "rst":
				row.RST, _ = strconv.ParseUint(value, 10, 64)
			case "responses":
				row.Responses, _ = strconv.ParseUint(value, 10, 64)
			case "icmp-unreachable":
				row.ICMPUnreachable, _ = strconv.ParseUint(value, 10, 64)
			case "window":
				row.WindowSeconds, _ = strconv.ParseFloat(value, 64)
			}
			index++
		}
		result = append(result, row)
	}
	return result
}

// ParseTransparentOrchestratorProbeTargets reads the installed passive-target
// count so the supervisor can re-install them after a VPP restart.
func ParseTransparentOrchestratorProbeTargets(output string) int {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "probe-targets" {
			count, _ := strconv.Atoi(fields[1])
			return count
		}
	}
	return 0
}

// BuildTransparentOrchestratorGroupBypassCommands toggles one group's bypass
// flag.  The committed generation and policy are untouched, exactly like the
// global flag.
func BuildTransparentOrchestratorGroupBypassCommands(group string, enabled bool, cause, reason string) []string {
	group = transparentBypassToken(group, "unknown")
	state := "off"
	if enabled {
		state = "on"
	}
	cause = transparentBypassToken(cause, "manual")
	reason = transparentBypassToken(reason, "unspecified")
	return []string{
		fmt.Sprintf("set ly-route orchestrator bypass group %s %s cause %s reason %s", group, state, cause, reason),
		"show ly-route orchestrator",
	}
}

// SetTransparentOrchestratorGroupBypass applies one group's bypass and verifies
// the group row in the readback.  Other groups keep their own state.
func (adapter Adapter) SetTransparentOrchestratorGroupBypass(ctx context.Context, requestID, group string, enabled bool, cause, reason string) (TransparentOrchestratorReadback, error) {
	if adapter.Client == nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "vpp.transparent-orchestrator.group-bypass", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	commands := BuildTransparentOrchestratorGroupBypassCommands(group, enabled, cause, reason)
	operation := Operation{Name: "vpp.transparent-orchestrator.group-bypass", RequestID: requestID, Resource: group, VPPCtlCommands: commands}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) == 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: group bypass results are missing", ErrSnapshotIncomplete)
	}
	show := payload.CommandResults[len(payload.CommandResults)-1]
	if show.Command != "show ly-route orchestrator" || show.Retval != 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: group bypass readback is missing", ErrSnapshotIncomplete)
	}
	state, generation, defaultAction := ParseTransparentOrchestratorOutput(show.Stdout)
	bypass, bypassCause, bypassReason, forced, packets := ParseTransparentOrchestratorBypass(show.Stdout)
	groups := ParseTransparentOrchestratorGroupBypass(show.Stdout)
	found := false
	for _, row := range groups {
		if row.Name != group {
			continue
		}
		found = true
		if row.Enabled != enabled {
			return TransparentOrchestratorReadback{}, fmt.Errorf("%w: group %s bypass state is %t, want %t", ErrSnapshotIncomplete, group, row.Enabled, enabled)
		}
	}
	if !found {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: group %s bypass row is missing", ErrSnapshotIncomplete, group)
	}
	return TransparentOrchestratorReadback{
		Generation: generation, State: state, Default: defaultAction,
		Bypass: bypass, BypassCause: bypassCause, BypassReason: bypassReason,
		BypassForced: forced, BypassPackets: packets, Output: show.Stdout,
		GroupBypass: groups,
		GroupProbes: ParseTransparentOrchestratorGroupProbes(show.Stdout),
		ProbeTargets: ParseTransparentOrchestratorProbeTargets(show.Stdout),
	}, nil
}

// BuildTransparentOrchestratorProbeCommands replaces the passive probe target
// set.  Targets are runtime state, not part of the committed generation, so a
// restart is recovered by re-issuing them.
func BuildTransparentOrchestratorProbeCommands(targets []GroupProbeTarget) []string {
	commands := []string{"set ly-route orchestrator probe clear"}
	for _, target := range targets {
		protocol := strings.ToLower(strings.TrimSpace(target.Protocol))
		if protocol != "udp" {
			protocol = "tcp"
		}
		window := target.WindowSeconds
		if window < 1 {
			window = 10
		}
		commands = append(commands, fmt.Sprintf(
			"set ly-route orchestrator probe group %s dst %s port %d proto %s window %.0f",
			transparentBypassToken(target.Group, "unknown"),
			strings.TrimSpace(target.DstIP), target.DstPort, protocol, window))
	}
	commands = append(commands, "show ly-route orchestrator")
	return commands
}

// SetTransparentOrchestratorProbes installs the passive targets and verifies
// the count the plugin reports.
func (adapter Adapter) SetTransparentOrchestratorProbes(ctx context.Context, requestID string, targets []GroupProbeTarget) error {
	if adapter.Client == nil {
		return VPPError{Operation: "vpp.transparent-orchestrator.probes", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	commands := BuildTransparentOrchestratorProbeCommands(targets)
	operation := Operation{Name: "vpp.transparent-orchestrator.probes", RequestID: requestID, Resource: "probes", VPPCtlCommands: commands}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) == 0 {
		return fmt.Errorf("%w: passive probe results are missing", ErrSnapshotIncomplete)
	}
	show := payload.CommandResults[len(payload.CommandResults)-1]
	if show.Command != "show ly-route orchestrator" || show.Retval != 0 {
		return fmt.Errorf("%w: passive probe readback is missing", ErrSnapshotIncomplete)
	}
	if observed := ParseTransparentOrchestratorProbeTargets(show.Stdout); observed != len(targets) {
		return fmt.Errorf("%w: plugin reports %d passive targets, want %d", ErrSnapshotIncomplete, observed, len(targets))
	}
	return nil
}

func transparentBypassToken(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', strings.ContainsRune("-_.:/@", character):
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
		if builder.Len() >= 128 {
			break
		}
	}
	if builder.Len() == 0 {
		return fallback
	}
	return builder.String()
}

// BuildTransparentOrchestratorBypassCommands toggles the plugin bypass flag
// without touching the candidate or the committed generation.
func BuildTransparentOrchestratorBypassCommands(enabled bool, cause, reason string) []string {
	cause = transparentBypassToken(cause, "manual")
	reason = transparentBypassToken(reason, "unspecified")
	if enabled {
		return []string{
			fmt.Sprintf("set ly-route orchestrator bypass on cause %s reason %s", cause, reason),
			"show ly-route orchestrator",
		}
	}
	return []string{
		fmt.Sprintf("set ly-route orchestrator bypass off cause %s reason %s", cause, reason),
		"show ly-route orchestrator",
	}
}

func (adapter Adapter) SetTransparentOrchestratorBypass(ctx context.Context, requestID string, enabled bool, cause, reason string) (TransparentOrchestratorReadback, error) {
	if adapter.Client == nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "vpp.transparent-orchestrator.bypass", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	commands := BuildTransparentOrchestratorBypassCommands(enabled, cause, reason)
	operation := Operation{Name: "vpp.transparent-orchestrator.bypass", RequestID: requestID, Resource: "active", VPPCtlCommands: commands}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) == 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator bypass results are missing", ErrSnapshotIncomplete)
	}
	show := payload.CommandResults[len(payload.CommandResults)-1]
	if show.Command != "show ly-route orchestrator" || show.Retval != 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator bypass readback is missing", ErrSnapshotIncomplete)
	}
	state, generation, defaultAction := ParseTransparentOrchestratorOutput(show.Stdout)
	bypass, bypassCause, bypassReason, forced, packets := ParseTransparentOrchestratorBypass(show.Stdout)
	if bypass != enabled {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator bypass state is %t, want %t", ErrSnapshotIncomplete, bypass, enabled)
	}
	return TransparentOrchestratorReadback{
		Generation: generation, State: state, Default: defaultAction,
		Bypass: bypass, BypassCause: bypassCause, BypassReason: bypassReason,
		BypassForced: forced, BypassPackets: packets, Output: show.Stdout,
		GroupBypass: ParseTransparentOrchestratorGroupBypass(show.Stdout),
		GroupProbes: ParseTransparentOrchestratorGroupProbes(show.Stdout),
		ProbeTargets: ParseTransparentOrchestratorProbeTargets(show.Stdout),
	}, nil
}

// and the default action from `show ly-route orchestrator` output.
func ParseTransparentOrchestratorOutput(output string) (state, generation, defaultAction string) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "state":
			state = fields[1]
		case "generation":
			generation = fields[1]
		case "default":
			defaultAction = fields[1]
		}
	}
	return state, generation, defaultAction
}

func BuildTransparentOrchestratorDisableCommands() []string {
	return []string{"set ly-route orchestrator disable", "show ly-route orchestrator"}
}

func BuildTransparentOrchestratorCommands(config TransparentOrchestratorConfig) ([]string, error) {
	generation := strings.TrimSpace(config.Generation)
	if generation == "" || strings.ContainsAny(generation, " \t\r\n") {
		return nil, errors.New("transparent orchestrator generation is invalid")
	}
	wan, lan, err := transparentBoundaryInterfaces(config.Topology)
	if err != nil {
		return nil, err
	}
	groups := append([]orchestrator.GroupView(nil), config.Topology.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	commands := []string{
		"set ly-route orchestrator candidate clear",
		fmt.Sprintf("set ly-route orchestrator candidate boundary wan %s lan %s", wan, lan),
	}
	knownGroups := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		wanFacing, lanFacing, groupErr := transparentGroupInterfaces(group)
		if groupErr != nil {
			return nil, groupErr
		}
		knownGroups[group.Name] = struct{}{}
		commands = append(commands, fmt.Sprintf("set ly-route orchestrator candidate group %s wan-facing %s lan-facing %s", group.Name, wanFacing, lanFacing))
	}
	defaultAction := orchestrator.ActionDirect
	if config.Policy != nil {
		policyCommands, action, buildErr := transparentPolicyCommands(*config.Policy, knownGroups)
		if buildErr != nil {
			return nil, buildErr
		}
		commands = append(commands, policyCommands...)
		defaultAction = action
	}
	commands = append(commands,
		fmt.Sprintf("set ly-route orchestrator candidate default %s", defaultAction),
		fmt.Sprintf("set ly-route orchestrator commit generation %s", generation),
		"show ly-route orchestrator",
	)
	return commands, nil
}

func transparentBoundaryInterfaces(topology orchestrator.TopologyView) (string, string, error) {
	var wan, lan string
	for _, item := range topology.Interfaces {
		name := item.Port
		if item.Bond != nil {
			name = item.Bond.Name
		}
		name = transparentVPPInterface(name)
		switch item.Role {
		case orchestrator.RoleWAN:
			if wan != "" {
				return "", "", orchestrator.ErrDuplicateWAN
			}
			wan = name
		case orchestrator.RoleLAN:
			if lan != "" {
				return "", "", orchestrator.ErrDuplicateLAN
			}
			lan = name
		}
	}
	if wan == "" {
		return "", "", orchestrator.ErrMissingWAN
	}
	if lan == "" {
		return "", "", orchestrator.ErrMissingLAN
	}
	return wan, lan, nil
}

func transparentGroupInterfaces(group orchestrator.GroupView) (string, string, error) {
	var wan, lan string
	for _, port := range group.Ports {
		if port.Bond != nil {
			return "", "", orchestrator.ErrGroupBond
		}
		switch port.Direction {
		case orchestrator.DirectionWANFacing:
			wan = transparentVPPInterface(port.Interface)
		case orchestrator.DirectionLANFacing:
			lan = transparentVPPInterface(port.Interface)
		}
	}
	if wan == "" || lan == "" {
		return "", "", fmt.Errorf("%w: %q", orchestrator.ErrGroupDirection, group.Name)
	}
	return wan, lan, nil
}

func transparentVPPInterface(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "lyroute-") {
		return name
	}
	return "lyroute-" + name
}

type transparentPrefix struct {
	prefix netip.Prefix
}

func transparentPolicyCommands(policy orchestrator.PolicyView, knownGroups map[string]struct{}) ([]string, orchestrator.ActionKind, error) {
	objects := make(map[string][]netip.Prefix, len(policy.IPObjects))
	for _, object := range policy.IPObjects {
		for _, raw := range object.Prefixes {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, "", fmt.Errorf("policy object %q contains invalid normalized prefix %q", object.ID, raw)
			}
			objects[object.ID] = append(objects[object.ID], prefix.Masked())
		}
	}
	groups := append([]orchestrator.PolicyGroupView(nil), policy.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Position < groups[j].Position })
	commands := []string{}
	variants := 0
	for _, group := range groups {
		rules := append([]orchestrator.PolicyRuleView(nil), group.Rules...)
		sort.Slice(rules, func(i, j int) bool { return rules[i].Sequence < rules[j].Sequence })
		for _, rule := range rules {
			sources, err := transparentSelectorPrefixes(rule.Match.Sources, objects)
			if err != nil {
				return nil, "", fmt.Errorf("rule %q source selector: %w", rule.ID, err)
			}
			destinations, err := transparentSelectorPrefixes(rule.Match.Destinations, objects)
			if err != nil {
				return nil, "", fmt.Errorf("rule %q destination selector: %w", rule.ID, err)
			}
			sourcePorts := transparentPortRanges(rule.Match.SourcePorts)
			destinationPorts := transparentPortRanges(rule.Match.DestinationPorts)
			protocol, family, err := transparentProtocol(rule.Match.Protocol)
			if err != nil {
				return nil, "", err
			}
			target := "none"
			if rule.Action.Kind == orchestrator.ActionVia {
				if _, ok := knownGroups[rule.Action.Group]; !ok {
					return nil, "", fmt.Errorf("rule %q references unknown orchestration group %q", rule.ID, rule.Action.Group)
				}
				target = rule.Action.Group
			}
			for _, source := range sources {
				for _, destination := range destinations {
					if source.prefix.Addr().BitLen() != destination.prefix.Addr().BitLen() || (family != 0 && source.prefix.Addr().BitLen() != family) {
						continue
					}
					for _, sourcePort := range sourcePorts {
						for _, destinationPort := range destinationPorts {
							variants++
							if variants > maxTransparentOrchestratorRuleVariants {
								return nil, "", fmt.Errorf("transparent orchestrator policy expands beyond %d VPP rule variants", maxTransparentOrchestratorRuleVariants)
							}
							addressFamily := "ip4"
							if source.prefix.Addr().Is6() {
								addressFamily = "ip6"
							}
							commands = append(commands, fmt.Sprintf(
								"set ly-route orchestrator candidate rule id %s group-position %d sequence %d action %s target %s family %s src %s dst %s proto %d sport %d-%d dport %d-%d",
								rule.ID, group.Position, rule.Sequence, rule.Action.Kind, target, addressFamily, source.prefix, destination.prefix, protocol,
								sourcePort.Start, sourcePort.End, destinationPort.Start, destinationPort.End,
							))
						}
					}
				}
			}
		}
	}
	if policy.Default.Kind != orchestrator.ActionDirect && policy.Default.Kind != orchestrator.ActionDrop {
		return nil, "", fmt.Errorf("transparent orchestrator default action %q is not terminal", policy.Default.Kind)
	}
	return commands, policy.Default.Kind, nil
}

func transparentSelectorPrefixes(selectors []string, objects map[string][]netip.Prefix) ([]transparentPrefix, error) {
	result := []transparentPrefix{}
	for _, selector := range selectors {
		if selector == "any" {
			result = append(result,
				transparentPrefix{prefix: netip.MustParsePrefix("0.0.0.0/0")},
				transparentPrefix{prefix: netip.MustParsePrefix("::/0")},
			)
			continue
		}
		prefixes, ok := objects[selector]
		if !ok {
			return nil, fmt.Errorf("unknown IP object %q", selector)
		}
		for _, prefix := range prefixes {
			result = append(result, transparentPrefix{prefix: prefix})
		}
	}
	if len(result) == 0 {
		return nil, errors.New("selector is empty")
	}
	return result, nil
}

func transparentPortRanges(ranges []orchestrator.PortRangeInput) []orchestrator.PortRangeInput {
	if len(ranges) == 0 {
		return []orchestrator.PortRangeInput{{Start: 0, End: 65535}}
	}
	return ranges
}

func transparentProtocol(protocol orchestrator.Protocol) (number uint8, family int, err error) {
	switch protocol {
	case orchestrator.ProtocolAny:
		return 0, 0, nil
	case orchestrator.ProtocolTCP:
		return 6, 0, nil
	case orchestrator.ProtocolUDP:
		return 17, 0, nil
	case orchestrator.ProtocolICMP:
		return 1, 32, nil
	case orchestrator.ProtocolICMPv6:
		return 58, 128, nil
	default:
		return 0, 0, fmt.Errorf("unsupported transparent orchestrator protocol %q", protocol)
	}
}

func (adapter Adapter) ApplyTransparentOrchestrator(ctx context.Context, requestID string, config TransparentOrchestratorConfig) (TransparentOrchestratorReadback, error) {
	commands, err := BuildTransparentOrchestratorCommands(config)
	if err != nil {
		return TransparentOrchestratorReadback{}, err
	}
	if adapter.Client == nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "vpp.transparent-orchestrator", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	operation := Operation{Name: "vpp.transparent-orchestrator", RequestID: requestID, Resource: config.Generation, VPPCtlCommands: commands}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) == 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator command results are missing", ErrSnapshotIncomplete)
	}
	show := payload.CommandResults[len(payload.CommandResults)-1]
	if show.Command != "show ly-route orchestrator" || show.Retval != 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator readback command is missing", ErrSnapshotIncomplete)
	}
	if err := verifyTransparentOrchestratorReadback(show.Stdout, config, commands); err != nil {
		return TransparentOrchestratorReadback{}, err
	}
	state, generation, defaultAction := ParseTransparentOrchestratorOutput(show.Stdout)
	bypass, bypassCause, bypassReason, bypassForced, bypassPackets := ParseTransparentOrchestratorBypass(show.Stdout)
	if state == "" {
		state = "running"
	}
	if generation == "" {
		generation = config.Generation
	}
	return TransparentOrchestratorReadback{Generation: generation, State: state, Default: defaultAction, Bypass: bypass, BypassCause: bypassCause, BypassReason: bypassReason, BypassForced: bypassForced, BypassPackets: bypassPackets, Output: show.Stdout, GroupBypass: ParseTransparentOrchestratorGroupBypass(show.Stdout), GroupProbes: ParseTransparentOrchestratorGroupProbes(show.Stdout), ProbeTargets: ParseTransparentOrchestratorProbeTargets(show.Stdout)}, nil
}

func (adapter Adapter) ObserveTransparentOrchestrator(ctx context.Context, requestID string) (TransparentOrchestratorReadback, error) {
	if adapter.Client == nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "vpp.transparent-orchestrator.observe", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	operation := Operation{Name: "vpp.transparent-orchestrator.observe", RequestID: requestID, Resource: "active", VPPCtlCommands: []string{"show ly-route orchestrator"}}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return TransparentOrchestratorReadback{}, VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) != 1 || payload.CommandResults[0].Command != "show ly-route orchestrator" || payload.CommandResults[0].Retval != 0 {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator observation is incomplete", ErrSnapshotIncomplete)
	}
	output := payload.CommandResults[0].Stdout
	state, generation, defaultAction := ParseTransparentOrchestratorOutput(output)
	bypass, bypassCause, bypassReason, bypassForced, bypassPackets := ParseTransparentOrchestratorBypass(output)
	readback := TransparentOrchestratorReadback{State: state, Generation: generation, Default: defaultAction, Bypass: bypass, BypassCause: bypassCause, BypassReason: bypassReason, BypassForced: bypassForced, BypassPackets: bypassPackets, Output: output, GroupBypass: ParseTransparentOrchestratorGroupBypass(output), GroupProbes: ParseTransparentOrchestratorGroupProbes(output), ProbeTargets: ParseTransparentOrchestratorProbeTargets(output)}
	if readback.State == "" || (readback.State == "running" && readback.Generation == "") {
		return TransparentOrchestratorReadback{}, fmt.Errorf("%w: transparent orchestrator state/generation is missing", ErrSnapshotIncomplete)
	}
	return readback, nil
}

func (adapter Adapter) DisableTransparentOrchestrator(ctx context.Context, requestID string) error {
	if adapter.Client == nil {
		return VPPError{Operation: "vpp.transparent-orchestrator.disable", RequestID: requestID, Err: errors.New("vpp client is not configured")}
	}
	channel, err := adapter.Client.OpenChannel(ctx)
	if err != nil {
		return VPPError{Operation: "open_channel", RequestID: requestID, Err: err}
	}
	defer channel.Close()
	operation := Operation{Name: "vpp.transparent-orchestrator.disable", RequestID: requestID, Resource: "active", VPPCtlCommands: BuildTransparentOrchestratorDisableCommands()}
	reply, err := channel.Do(ctx, operation)
	if err != nil {
		return VPPError{Operation: operation.Name, RequestID: requestID, Err: err}
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok || len(payload.CommandResults) != len(operation.VPPCtlCommands) {
		return fmt.Errorf("%w: transparent orchestrator disable results are incomplete", ErrSnapshotIncomplete)
	}
	show := payload.CommandResults[len(payload.CommandResults)-1]
	if show.Command != "show ly-route orchestrator" || show.Retval != 0 || !strings.Contains(show.Stdout, "state locked") {
		return fmt.Errorf("%w: transparent orchestrator did not enter locked state", ErrSnapshotIncomplete)
	}
	return nil
}

func verifyTransparentOrchestratorReadback(output string, config TransparentOrchestratorConfig, commands []string) error {
	required := []string{"state running", "generation " + config.Generation}
	for _, command := range commands {
		switch {
		case strings.Contains(command, " candidate boundary "):
			required = append(required, strings.TrimPrefix(command, "set ly-route orchestrator candidate "))
		case strings.Contains(command, " candidate group "):
			required = append(required, strings.TrimPrefix(command, "set ly-route orchestrator candidate "))
		case strings.Contains(command, " candidate default "):
			required = append(required, strings.TrimPrefix(command, "set ly-route orchestrator candidate "))
		}
	}
	for _, fragment := range required {
		if !strings.Contains(output, fragment) {
			return fmt.Errorf("%w: transparent orchestrator readback is missing %q", ErrSnapshotIncomplete, fragment)
		}
	}
	expectedPolicies := 0
	for _, command := range commands {
		if strings.Contains(command, " candidate rule ") {
			expectedPolicies++
		}
	}
	observedPolicies := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "policy ") {
			observedPolicies++
		}
	}
	if observedPolicies != expectedPolicies {
		return fmt.Errorf("%w: transparent orchestrator readback has %s policy rows, want %s", ErrSnapshotIncomplete, strconv.Itoa(observedPolicies), strconv.Itoa(expectedPolicies))
	}
	return nil
}
