package vpp

import (
	"context"
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/proxy"
)

// doProxyServiceNetwork keeps repeated proxy applies from replaying NAT44
// pool/interface mutations that are already live. Every apply used to log the
// same "NAT address already in use", "set interface nat44: del ... failed" and
// "interface already configured" sequence, which is the noise that preceded the
// J1900 NAT44 crash.
func (channel vppctlChannel) doProxyServiceNetwork(ctx context.Context, operation Operation, steering proxy.VPPSteeringInstruction) (Reply, error) {
	network := steering.ServiceNetwork
	if strings.TrimSpace(network.EgressID) == "" {
		network = proxy.ServiceNetworkForEgressID(steering.EgressID)
	}
	// The persisted plan froze the TAP names at generation time. Resolve them
	// against the live topology before any readback so a replayed generation
	// cannot `show interface address` an interface that no longer exists.
	resolved, presence, err := channel.resolveProxyServiceNetworkInterfaces(ctx, network)
	if err != nil {
		return Reply{}, err
	}
	deleting := isProxyServiceNetworkDelete(operation)
	if !deleting && !presence.complete() {
		// `show interface address <missing>` is not optional on VPP; it is the
		// exact retval 1 this path used to fail with. Create/repair the TAPs
		// first, then resolve again, so the readback can only run against a
		// live interface.
		if err := channel.ensureProxyServiceNetworkTAPs(ctx, operation, network); err != nil {
			return Reply{}, err
		}
		resolved, presence, err = channel.resolveProxyServiceNetworkInterfaces(ctx, network)
		if err != nil {
			return Reply{}, err
		}
		if !presence.complete() {
			return Reply{}, fmt.Errorf("proxy service network %q has no live service TAP after create/repair: %s; the configuration was not committed", network.EgressID, describeProxyServiceNetworkPresence(network, presence))
		}
	}
	// Cleanup of TAPs that are already gone stays idempotent: its delete
	// commands are optional and the frozen plan name simply matches nothing.
	operation = proxyServiceNetworkOperationWithInterfaces(operation, network, resolved, presence.complete() && !deleting)
	network = resolved
	steering.ServiceNetwork = resolved
	if strings.TrimSpace(network.NATPoolAddress) == "" || network.OutboundTableID <= 0 {
		reply, runErr := channel.doCommands(ctx, operation)
		if runErr != nil {
			return reply, runErr
		}
		return replyWithResolvedProxyServiceNetwork(reply, resolved), nil
	}
	preflight := Operation{
		Name:           "vpp.nat44.state.proxy",
		RequestID:      operation.RequestID,
		Resource:       steering.EgressID,
		VPPCtlCommands: []string{"show nat44 interfaces", "show nat44 addresses"},
	}
	preflightReply, err := channel.doCommands(ctx, preflight)
	if err != nil {
		// A missing NAT44 inventory must not prevent the first configuration.
		return channel.doCommands(ctx, operation)
	}
	results := vppctlCommandResults(preflightReply)
	roles, addresses := proxyNATPreflightOutput(results)
	underlayInterface := proxySteeringUnderlayInterface(steering)
	if isProxyServiceNetworkDelete(operation) {
		// Removing the last egress that used this WAN must not leave the
		// output-feature role behind: proxy traffic is the only consumer of the
		// combined `output-feature in out` attachment, and the plain gateway
		// NAT44 role is `out`. The revert is driven by the live inventory so a
		// WAN that never carried the role is not touched.
		revert := proxyUnderlayRoleRevertCommands(underlayInterface, roles)
		if len(revert) == 0 {
			return channel.doCommands(ctx, operation)
		}
		patched := operation
		patched.VPPCtlCommands = append(append([]string(nil), revert...), operation.VPPCtlCommands...)
		return channel.doCommands(ctx, patched)
	}
	filtered := operation
	filtered.VPPCtlCommands = filterProxyNATCommands(operation.VPPCtlCommands, roles, addresses, steering)
	reply, err := channel.doCommands(ctx, filtered)
	if err != nil {
		return reply, err
	}
	reply = replyWithResolvedProxyServiceNetwork(reply, resolved)
	if err := verifyProxyUnderlayRoleCommand(reply, underlayInterface); err != nil {
		return reply, err
	}
	return reply, nil
}

// replyWithResolvedProxyServiceNetwork records the names the channel actually
// executed against so the supplemental verifier can compare the plan operation
// with the live readback without mistaking a legitimate rename for drift.
func replyWithResolvedProxyServiceNetwork(reply Reply, network proxy.ProxyServiceNetwork) Reply {
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok {
		return reply
	}
	payload.ProxyServiceNetwork = &network
	reply.Payload = payload
	return reply
}

func proxyNATPreflightOutput(results []VPPCTLCommandResult) (string, string) {
	roles := ""
	addresses := ""
	for _, result := range results {
		if result.Retval != 0 {
			continue
		}
		switch result.Command {
		case "show nat44 interfaces":
			if roles == "" {
				roles = result.Stdout
			}
		case "show nat44 addresses":
			if addresses == "" {
				addresses = result.Stdout
			}
		}
	}
	return roles, addresses
}

func proxySteeringUnderlayInterface(steering proxy.VPPSteeringInstruction) string {
	underlay := strings.TrimSpace(steering.UnderlayRoute)
	if underlay == "" {
		underlay = strings.TrimSpace(steering.ServiceNetwork.UnderlayRoute)
	}
	return proxyUnderlayInterface(underlay)
}

func isProxyServiceNetworkDelete(operation Operation) bool {
	return strings.Contains(operation.Name, "rollback-delete") ||
		strings.Contains(operation.Name, "delete") ||
		operationHasCommand(operation, "delete tap")
}

// proxyNATRoleState parses a `show nat44 interfaces` inventory into the role
// sets the command planner needs.
func proxyNATRoleState(roles string) nat44RuntimeState {
	state := nat44RuntimeState{Inside: map[string]bool{}, Outside: map[string]bool{}, OutputFeature: map[string]bool{}}
	parseNAT44InterfaceRoles(roles, &state)
	return state
}

// proxyUnderlayRoleCommands returns the minimal commands that move a proxy
// underlay WAN to the NAT44 role proxy steering needs (`output-feature in out`).
//
// VPP 26.06 rejects `set interface nat44 out <wan> output-feature` on a port
// that already carries the plain outside role: the CLI answers
// "nat: error interface already configured" and prints
// `set interface nat44: add <wan> failed`, but still exits 0, so the caller
// cannot rely on the exit code. The plain role must therefore be removed first
// and the combined role added. A WAN that already has the output feature
// produces no command at all, which keeps repeated applies silent.
func proxyUnderlayRoleCommands(underlay, roles string) []string {
	underlay = strings.TrimSpace(underlay)
	if underlay == "" {
		return nil
	}
	state := proxyNATRoleState(roles)
	if state.OutputFeature[underlay] {
		return nil
	}
	commands := make([]string, 0, 2)
	if state.Outside[underlay] {
		commands = append(commands, "?set interface nat44 out "+underlay+" del")
	}
	return append(commands, "?set interface nat44 out "+underlay+" output-feature")
}

// proxyUnderlayRoleRevertCommands returns the commands that restore the plain
// outside role of a WAN that no longer carries a proxy egress. The
// output-feature toggle removes the combined in/out entry, so the gateway role
// is re-added explicitly.
func proxyUnderlayRoleRevertCommands(underlay, roles string) []string {
	underlay = strings.TrimSpace(underlay)
	if underlay == "" {
		return nil
	}
	if !proxyNATRoleState(roles).OutputFeature[underlay] {
		return nil
	}
	return []string{
		"?set interface nat44 out " + underlay + " output-feature del",
		"?set interface nat44 out " + underlay,
	}
}

// isProxyUnderlayRoleCommand reports whether command mutates the underlay's
// NAT44 outside/output-feature role. Every form the steering planner can emit
// (and the legacy single-command form still present in persisted plans) is
// replaced by the state-driven pair so VPP never sees an `add` that conflicts
// with a live plain outside role.
func isProxyUnderlayRoleCommand(command, underlay string) bool {
	underlay = strings.TrimSpace(underlay)
	if underlay == "" {
		return false
	}
	prefix := "set interface nat44 out " + underlay
	if command != prefix && !strings.HasPrefix(command, prefix+" ") {
		return false
	}
	switch strings.TrimSpace(strings.TrimPrefix(command, prefix)) {
	case "", "del", "output-feature", "output-feature del":
		return true
	}
	return false
}

func filterProxyNATCommands(commands []string, roles, addresses string, steering proxy.VPPSteeringInstruction) []string {
	network := steering.ServiceNetwork
	underlayInterface := proxySteeringUnderlayInterface(steering)
	egress := strings.TrimSpace(network.EgressVPPInterface)
	pool := strings.TrimSpace(network.NATPoolAddress)
	roleState := proxyNATRoleState(roles)
	wanOutputFeature := underlayInterface != "" && roleState.OutputFeature[underlayInterface]
	egressRole := egress != "" && (roleState.Inside[egress] || roleState.Outside[egress] || roleState.OutputFeature[egress])
	poolPresent := nat44PoolHasAddressForVRF(addresses, pool, network.OutboundTableID) || nat44PoolHasPlainAddress(addresses, pool)

	filtered := make([]string, 0, len(commands))
	roleEmitted := false
	for _, raw := range commands {
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		switch {
		case isProxyUnderlayRoleCommand(command, underlayInterface):
			// Replace the whole static role block with the one transition the
			// live inventory actually needs, before the operation's own
			// `show nat44 interfaces` readback runs.
			if roleEmitted {
				continue
			}
			roleEmitted = true
			filtered = append(filtered, proxyUnderlayRoleCommands(underlayInterface, roles)...)
			continue
		case strings.HasPrefix(command, "nat44 ei plugin disable") || command == "nat44 plugin enable":
			if wanOutputFeature {
				continue
			}
		case strings.HasPrefix(command, "nat44 add address "+pool+" tenant-vrf "):
			if poolPresent {
				continue
			}
		case strings.HasPrefix(command, "set interface nat44 in "+egress) ||
			strings.HasPrefix(command, "set interface nat44 out "+egress):
			if !egressRole {
				continue
			}
		}
		filtered = append(filtered, raw)
	}
	if !roleEmitted {
		// A persisted plan that predates the role command would otherwise
		// commit without ever installing the output feature the readback
		// requires. Insert the transition before the first readback.
		roleCommands := proxyUnderlayRoleCommands(underlayInterface, roles)
		if len(roleCommands) > 0 {
			inserted := false
			patched := make([]string, 0, len(filtered)+len(roleCommands))
			for _, raw := range filtered {
				command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
				if !inserted && strings.HasPrefix(command, "show ") {
					patched = append(patched, roleCommands...)
					inserted = true
				}
				patched = append(patched, raw)
			}
			if !inserted {
				patched = append(patched, roleCommands...)
			}
			filtered = patched
		}
	}
	return filtered
}

// verifyProxyUnderlayRoleCommand surfaces the real reason a proxy underlay role
// command did not take effect. VPP's CLI prints the failure reason on stdout
// and still exits 0, so a generic snapshot/readback error would otherwise hide
// "interface already configured" behind a misleading timeout. It also lets the
// apply fail immediately instead of two minutes later.
func verifyProxyUnderlayRoleCommand(reply Reply, underlay string) error {
	underlay = strings.TrimSpace(underlay)
	if underlay == "" {
		return nil
	}
	wanted := "set interface nat44 out " + underlay + " output-feature"
	for _, result := range vppctlCommandResults(reply) {
		if strings.TrimSpace(result.Command) != wanted {
			continue
		}
		if result.Retval == 0 && !vppctlOutputReportsFailure(result.Stdout) && !vppctlOutputReportsFailure(result.Stderr) {
			return nil
		}
		detail := strings.TrimSpace(firstNonEmpty(strings.TrimSpace(result.Stdout), strings.TrimSpace(result.Stderr)))
		if detail == "" {
			detail = fmt.Sprintf("vppctl returned retval %d", result.Retval)
		}
		return fmt.Errorf("proxy service underlay %q NAT44 output-feature role failed: %s", underlay, detail)
	}
	return nil
}

func vppctlOutputReportsFailure(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"failed", "error", "invalid", "not found", "does not exist"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
