package vpp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/proxy"
)

// The two VPP peers of a proxy service network. The Linux peers are
// lypxhin*/lypxhout*, so these prefixes only match the VPP-owned TAPs.
const (
	proxyServiceIngressPrefix = "lypxin"
	proxyServiceEgressPrefix  = "lypxout"
)

// proxyServiceNetworkPresence reports which service TAPs the live VPP topology
// provided for one proxy egress.
type proxyServiceNetworkPresence struct {
	Ingress bool
	Egress  bool
}

func (presence proxyServiceNetworkPresence) complete() bool {
	return presence.Ingress && presence.Egress
}

// resolveProxyServiceNetworkInterfaces maps the interface names frozen into a
// plan onto the proxy service TAPs that exist in the current VPP topology.
//
// The plan is generated once from the egress identity, but the same operation
// list can be executed later (repeat apply, reconciliation cleanup, rollback
// restore, or a flush after the egress was re-created). By then the TAPs may
// carry a different suffix, or may not exist at all. The VPP object identity is
// the TAP, not the string in the persisted plan, so the live name wins whenever
// the planned one is absent. A missing TAP is reported through presence instead
// of an error: apply must create it and cleanup must treat it as already gone.
func (channel *vppctlChannel) resolveProxyServiceNetworkInterfaces(ctx context.Context, network proxy.ProxyServiceNetwork) (proxy.ProxyServiceNetwork, proxyServiceNetworkPresence, error) {
	output, err := channel.runReadOnlyCommand(ctx, "show", "interface")
	if err != nil {
		return network, proxyServiceNetworkPresence{}, fmt.Errorf("proxy service network %q: read the live VPP interface inventory: %w", network.EgressID, err)
	}
	names := parseVPPInterfaceNames(output)
	ingress, ingressPresent, err := resolveProxyServiceInterfaceName(names, network.IngressVPPInterface, proxyServiceIngressPrefix)
	if err != nil {
		return network, proxyServiceNetworkPresence{}, fmt.Errorf("proxy service network %q ingress TAP: %w", network.EgressID, err)
	}
	egress, egressPresent, err := resolveProxyServiceInterfaceName(names, network.EgressVPPInterface, proxyServiceEgressPrefix)
	if err != nil {
		return network, proxyServiceNetworkPresence{}, fmt.Errorf("proxy service network %q egress TAP: %w", network.EgressID, err)
	}
	if ingressPresent {
		network.IngressVPPInterface = ingress
	}
	if egressPresent {
		network.EgressVPPInterface = egress
	}
	return network, proxyServiceNetworkPresence{Ingress: ingressPresent, Egress: egressPresent}, nil
}

// resolveProxyServiceInterfaceName prefers the planned name, then falls back to
// the single live interface with the service prefix. More than one candidate is
// ambiguous and fails closed instead of guessing a TAP.
func resolveProxyServiceInterfaceName(live map[string]bool, planned, prefix string) (string, bool, error) {
	planned = strings.TrimSpace(planned)
	if planned != "" && live[planned] {
		return planned, true, nil
	}
	candidates := make([]string, 0, 2)
	for name := range live {
		if len(name) > len(prefix) && strings.HasPrefix(strings.ToLower(name), prefix) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		return planned, false, nil
	case 1:
		return candidates[0], true, nil
	default:
		return planned, false, fmt.Errorf("cannot resolve the live %s* TAP: the plan expects %q but VPP has %s", prefix, planned, strings.Join(candidates, ", "))
	}
}

// ensureProxyServiceNetworkTAPs runs the idempotent TAP block before an apply
// readback. VPP fails `show interface address <missing>` with retval 1, so the
// readback must never be the first command that mentions the interface.
func (channel vppctlChannel) ensureProxyServiceNetworkTAPs(ctx context.Context, operation Operation, network proxy.ProxyServiceNetwork) error {
	commands := proxyServiceNetworkTAPCommands(network)
	if len(commands) == 0 {
		return nil
	}
	_, err := channel.doCommands(ctx, Operation{
		Name:           operation.Name + ".tap-repair",
		RequestID:      operation.RequestID,
		Resource:       operation.Resource,
		Payload:        operation.Payload,
		VPPCtlCommands: commands,
	})
	if err != nil {
		return fmt.Errorf("proxy service network %q: create/repair the service TAPs: %w", network.EgressID, err)
	}
	return nil
}

func describeProxyServiceNetworkPresence(network proxy.ProxyServiceNetwork, presence proxyServiceNetworkPresence) string {
	missing := make([]string, 0, 2)
	if !presence.Ingress {
		missing = append(missing, strings.TrimSpace(network.IngressVPPInterface))
	}
	if !presence.Egress {
		missing = append(missing, strings.TrimSpace(network.EgressVPPInterface))
	}
	return "missing " + strings.Join(missing, ", ")
}

// proxyServiceNetworkOperationWithInterfaces returns a copy of the operation
// whose service TAP references use the resolved live names. When the TAPs are
// already live the create/rename commands are dropped: replaying them is the
// noise (and the duplicate-TAP risk) this resolver exists to avoid.
func proxyServiceNetworkOperationWithInterfaces(operation Operation, planned, resolved proxy.ProxyServiceNetwork, live bool) Operation {
	if planned.IngressVPPInterface == resolved.IngressVPPInterface &&
		planned.EgressVPPInterface == resolved.EgressVPPInterface && !live {
		return operation
	}
	original := operation.VPPCtlCommands
	rewritten := make([]string, 0, len(original))
	for _, raw := range original {
		command := strings.TrimSpace(raw)
		if live && proxyServiceNetworkTAPCreateCommand(command, planned) {
			continue
		}
		command = replaceProxyServiceInterfaceName(command, planned.IngressVPPInterface, resolved.IngressVPPInterface)
		command = replaceProxyServiceInterfaceName(command, planned.EgressVPPInterface, resolved.EgressVPPInterface)
		rewritten = append(rewritten, command)
	}
	operation.VPPCtlCommands = rewritten
	if steering, ok := operation.Payload.(proxy.VPPSteeringInstruction); ok {
		steering.ServiceNetwork = resolved
		operation.Payload = steering
	}
	return operation
}

// proxyServiceNetworkTAPCreateCommand reports whether command creates or
// renames one of the two service TAPs. The state/MTU/address commands are kept:
// they are idempotent and re-assert the desired configuration on a live TAP.
func proxyServiceNetworkTAPCreateCommand(command string, network proxy.ProxyServiceNetwork) bool {
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?")))
	if len(fields) < 3 {
		return false
	}
	switch {
	case fields[0] == "create" && fields[1] == "tap":
		id := fieldAfter(fields, "id")
		if id == "" {
			return false
		}
		return id == strconv.Itoa(network.IngressTapID) || id == strconv.Itoa(network.EgressTapID)
	case fields[0] == "set" && fields[1] == "interface" && fields[2] == "name" && len(fields) >= 4:
		name := fields[3]
		return name == "tap"+strconv.Itoa(network.IngressTapID) || name == "tap"+strconv.Itoa(network.EgressTapID)
	}
	return false
}

// replaceProxyServiceInterfaceName swaps a whole CLI token. Interface names are
// exact tokens in every command the planner emits, so this cannot corrupt a
// longer name that merely shares a prefix.
func replaceProxyServiceInterfaceName(command, from, to string) string {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return command
	}
	fields := strings.Fields(command)
	changed := false
	for index, field := range fields {
		if field == from {
			fields[index] = to
			changed = true
		}
	}
	if !changed {
		return command
	}
	return strings.Join(fields, " ")
}

// proxyServiceNetworkInterfaceResolver is implemented by the live VPPCTL
// channel. Test doubles and non-VPP channels keep the legacy behaviour.
type proxyServiceNetworkInterfaceResolver interface {
	resolveProxyServiceNetworkInterfaces(context.Context, proxy.ProxyServiceNetwork) (proxy.ProxyServiceNetwork, proxyServiceNetworkPresence, error)
}

// proxyServiceNetworkVerificationOperation rebuilds the operation the channel
// actually executed. The evidence hash must still come from the plan copy: a
// live rename is not a plan change.
func proxyServiceNetworkVerificationOperation(operation Operation, network proxy.ProxyServiceNetwork) Operation {
	if steering, ok := operation.Payload.(proxy.VPPSteeringInstruction); ok {
		steering.ServiceNetwork = network
		operation.Payload = steering
	}
	return operation
}
