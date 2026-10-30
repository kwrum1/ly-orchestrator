package vpp

import (
	"context"
	"strings"

	"ly-route/backend/internal/runtime/nat"
)

// nat44RuntimeState is the subset of the live NAT44 inventory that decides
// whether initialization must mutate VPP. Rebuilding interface roles and pool
// addresses unconditionally produced the repeated
//
//	set interface nat44: del lyroute-enp4s0 failed
//	nat44 add interface address: del lyroute-enp4s0 address failed
//	nat44 add address: NAT address already in use
//
// sequence that preceded the J1900 SIGSEGV in nat44_original_dst_lookup.
type nat44RuntimeState struct {
	Known         bool
	Inside        map[string]bool
	Outside       map[string]bool
	OutputFeature map[string]bool
	Pool          []nat44PoolAddressEntry
}

func nat44StateShowCommands(behavior nat.Behavior) []string {
	if behavior == nat.BehaviorFullCone {
		return []string{"show nat44 ei interfaces", "show nat44 ei addresses"}
	}
	return []string{"show nat44 interfaces", "show nat44 addresses"}
}

// readNAT44RuntimeStateOn snapshots the live NAT44 roles and pool before an
// apply. A read failure (for example a disabled plugin) is not fatal: the
// returned state is Known=false and callers must not guess mutations.
func readNAT44RuntimeStateOn(ctx context.Context, channel Channel, transactionID string, behavior nat.Behavior) (nat44RuntimeState, []VPPCTLCommandResult, error) {
	commands := nat44StateShowCommands(behavior)
	reply, err := doOperation(ctx, channel, Operation{
		Name:           "vpp.nat44.state",
		RequestID:      transactionID,
		Resource:       string(behavior),
		VPPCtlCommands: commands,
	})
	if err != nil {
		return nat44RuntimeState{}, nil, err
	}
	results := vppctlCommandResults(reply)
	state := nat44RuntimeState{Inside: map[string]bool{}, Outside: map[string]bool{}, OutputFeature: map[string]bool{}}
	seen := map[string]bool{}
	for _, result := range results {
		if result.Retval != 0 {
			continue
		}
		seen[result.Command] = true
		switch result.Command {
		case commands[0]:
			parseNAT44InterfaceRoles(result.Stdout, &state)
		case commands[1]:
			state.Pool = parseNAT44PoolAddresses(result.Stdout)
		}
	}
	if !seen[commands[0]] || !seen[commands[1]] {
		return nat44RuntimeState{}, results, nil
	}
	state.Known = true
	return state, results, nil
}

func (a Adapter) readNAT44RuntimeState(ctx context.Context, channel Channel, transactionID string, behavior nat.Behavior) nat44RuntimeState {
	state, _, err := readNAT44RuntimeStateOn(ctx, channel, transactionID, behavior)
	if err != nil {
		return nat44RuntimeState{}
	}
	return state
}

func parseNAT44InterfaceRoles(output string, state *nat44RuntimeState) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		for _, field := range fields[1:] {
			switch strings.ToLower(field) {
			case "in":
				state.Inside[name] = true
			case "out":
				state.Outside[name] = true
			case "output-feature":
				state.OutputFeature[name] = true
			}
		}
	}
}

func nat44StateHasPool(address string, state nat44RuntimeState, tenantVRF int) bool {
	address = strings.TrimSpace(address)
	for _, entry := range state.Pool {
		if entry.Address != address {
			continue
		}
		if !entry.VRFSet {
			return tenantVRF < 0
		}
		if entry.Independent {
			continue
		}
		if entry.VRF == tenantVRF {
			return true
		}
	}
	return false
}

func nat44StateHasInterfaceAddress(state nat44RuntimeState) bool {
	for _, entry := range state.Pool {
		if !entry.VRFSet {
			return true
		}
	}
	return false
}

// nat44ReconcileCommands computes only the mutations required to move the live
// state to the desired one. When the fake/live inventory already equals the
// target, the result is empty. Every changed interface/address is emitted as a
// single delete/add pair, in that order.
func nat44ReconcileCommands(plan NAT44Plan, state nat44RuntimeState) []string {
	commands := make([]string, 0, 16)
	if !state.Known {
		return nat44InitializeForPlan(plan)
	}
	if plan.Behavior == nat.BehaviorFullCone {
		// NAT44-EI and NAT44-ED are separate plugins; switching behavior always
		// needs the explicit disable/enable pair.
		commands = append(commands, natInitializeCommands(plan.Behavior)...)
	} else if len(state.Inside) == 0 && len(state.Outside) == 0 && len(state.Pool) == 0 {
		// A readable but entirely empty inventory means the plugin is not
		// configured yet (or was disabled by a restart). Enable it once.
		commands = append(commands, natInitializeCommands(plan.Behavior)...)
	}
	lan := strings.TrimSpace(plan.IngressVPPInterface)
	outside := nat44OutsideInterfaces(plan)
	staticPool := nat44PoolAddressMap(plan.PoolAddresses)

	// Plugin enable is only needed when the NAT44 namespace is not readable.
	// A readable empty inventory already proves the plugin is enabled.
	desiredInside := plan.LANVPPInterfaces
	if len(desiredInside) == 0 && lan != "" && !nat44InterfaceListed(plan.DeleteInterfaces, lan) {
		desiredInside = []string{lan}
	}
	commands = append(commands, nat44RoleReconcileCommandsForPlan(desiredInside, outside, plan.DeleteInterfaces, plan.OutputFeatureInterfaces, plan.Behavior, state)...)

	for _, address := range plan.DeletePoolAddresses {
		address = strings.TrimSpace(address)
		if address == "" || staticPoolContains(staticPool, address) {
			continue
		}
		if !nat44StateHasPool(address, state, 0) && !nat44StateHasPool(address, state, -1) {
			continue
		}
		commands = append(commands,
			"?nat44 add address "+address+" tenant-vrf 0 del",
			"?nat44 add address "+address+" del",
		)
	}

	// The interface-address form is legacy/runtime state. Remove it only when
	// it is actually present; issuing the delete every apply is what produced
	// the "del <address> failed" storm on a static WAN.
	legacyInterfaceAddress := nat44StateHasInterfaceAddress(state)
	for _, wan := range outside {
		if plan.Behavior == nat.BehaviorFullCone {
			if !nat44StateHasInterfaceAddress(state) && len(state.Pool) == 0 {
				commands = append(commands, "?nat44 ei add interface address "+wan)
			}
			continue
		}
		if address, ok := staticPool[wan]; ok {
			if nat44StateHasPool(address, state, 0) {
				continue
			}
			if legacyInterfaceAddress {
				commands = append(commands, "?nat44 add interface address "+wan+" del")
			}
			commands = append(commands, "?nat44 add address "+address+" tenant-vrf 0")
			continue
		}
		if !legacyInterfaceAddress {
			commands = append(commands, "?nat44 add interface address "+wan)
		}
	}
	return dedupeNAT44Commands(commands)
}

// NAT44RolePlan is the role-only subset of NAT44Plan. The LAN control-plane
// lifecycle uses it to reconcile the same live state as the NAT44 graph
// without carrying pool or mapping mutations (the graph owns those).
type NAT44RolePlan struct {
	Behavior          nat.Behavior `json:"behavior,omitempty"`
	LANInterfaces     []string     `json:"lan_interfaces,omitempty"`
	OutsideInterfaces []string     `json:"outside_interfaces,omitempty"`
}

// nat44RoleReconcileCommands is the single implementation of NAT44 interface
// role reconciliation. It is shared by the NAT44 graph and the LAN
// control-plane lifecycle. Only real differences are emitted; a repeated
// apply is empty, so VPP never logs `set interface nat44: del ... failed`.
func nat44RoleReconcileCommands(insideInterfaces, outsideInterfaces []string, behavior nat.Behavior, state nat44RuntimeState) []string {
	return nat44RoleReconcileCommandsFor(insideInterfaces, outsideInterfaces, nil, behavior, state)
}

// nat44RoleReconcileCommandsFor is the full reconciliation. deleteInterfaces
// names ports whose role this generation retired; their live in/out/
// output-feature attachments are the residue that used to survive a cleared
// role. Only explicitly named interfaces are touched: unrelated live roles
// (for example a DNS service-network TAP) are left to their own owner.
func nat44RoleReconcileCommandsFor(insideInterfaces, outsideInterfaces, deleteInterfaces []string, behavior nat.Behavior, state nat44RuntimeState) []string {
	return nat44RoleReconcileCommandsForPlan(insideInterfaces, outsideInterfaces, deleteInterfaces, nil, behavior, state)
}

// nat44RoleReconcileCommandsForPlan additionally owns the output-feature role
// of an outside interface. outputFeatureInterfaces names the WAN ports that
// carry a proxy egress: their traffic leaves VPP after the tenant FIB has
// routed it, so the NAT44 translation must run in the interface output arc
// (`output-feature in out`). When the last egress that used a WAN disappears
// the port is returned to the plain outside role, leaving no output-feature
// behind.
func nat44RoleReconcileCommandsForPlan(insideInterfaces, outsideInterfaces, deleteInterfaces, outputFeatureInterfaces []string, behavior nat.Behavior, state nat44RuntimeState) []string {
	if !state.Known || behavior == nat.BehaviorFullCone {
		return nil
	}
	commands := make([]string, 0, 8)
	desiredInside := make(map[string]struct{}, len(insideInterfaces))
	for _, inside := range insideInterfaces {
		if inside = strings.TrimSpace(inside); inside != "" {
			desiredInside[inside] = struct{}{}
		}
	}
	desiredOutside := make(map[string]struct{}, len(outsideInterfaces))
	for _, outside := range outsideInterfaces {
		if outside = strings.TrimSpace(outside); outside != "" {
			desiredOutside[outside] = struct{}{}
		}
	}
	for _, stale := range deleteInterfaces {
		stale = strings.TrimSpace(stale)
		if stale == "" {
			continue
		}
		if _, wanted := desiredInside[stale]; wanted {
			continue
		}
		if _, wanted := desiredOutside[stale]; wanted {
			continue
		}
		if state.OutputFeature[stale] {
			// The output-feature toggle removes the combined in/out entry.
			commands = append(commands, "?set interface nat44 out "+stale+" output-feature del")
		} else {
			if state.Outside[stale] {
				commands = append(commands, "?set interface nat44 out "+stale+" del")
			}
			if state.Inside[stale] {
				commands = append(commands, "?set interface nat44 in "+stale+" del")
			}
		}
	}
	for _, inside := range insideInterfaces {
		inside = strings.TrimSpace(inside)
		if inside == "" {
			continue
		}
		// VPP 26.06 represents an outside interface with the output feature as
		// `output-feature in out`. Removing it requires the single
		// `out <intf> output-feature del` toggle: the follow-up `out ... del`
		// and `in ... del` forms report "interface couldn't be found". Use the
		// toggle once, then add the desired inside role if a WAN exists.
		hadOutsideRole := state.Outside[inside] || state.OutputFeature[inside]
		if state.OutputFeature[inside] {
			// A combined outside/output-feature role is removed by this single
			// toggle; `out ... del` afterwards would fail.
			commands = append(commands, "?set interface nat44 out "+inside+" output-feature del")
		} else if state.Outside[inside] {
			// A plain outside role (no output feature) is removed directly.
			commands = append(commands, "?set interface nat44 out "+inside+" del")
		}
		if len(outsideInterfaces) == 0 {
			// No WAN: nothing can be translated. An inside-only interface is
			// detached; an outside/output-feature interface was just removed by
			// the toggle above.
			if state.Inside[inside] && !hadOutsideRole {
				commands = append(commands, "?set interface nat44 in "+inside+" del")
			}
			continue
		}
		// A WAN exists: ensure the LAN is on the inside path. A clean inside
		// role needs no command.
		if state.Inside[inside] && !hadOutsideRole {
			continue
		}
		commands = append(commands, "?set interface nat44 in "+inside)
	}
	for _, outside := range outsideInterfaces {
		outside = strings.TrimSpace(outside)
		if outside == "" {
			continue
		}
		wantOutputFeature := nat44InterfaceListed(outputFeatureInterfaces, outside)
		// A port that used to be a LAN may still carry the inside role. Remove
		// it before adding the WAN outside role; leaving both makes
		// nat44-ed-out2in drop the traffic the new WAN should forward.
		if state.Inside[outside] && !state.OutputFeature[outside] {
			commands = append(commands, "?set interface nat44 in "+outside+" del")
		}
		if wantOutputFeature {
			if state.OutputFeature[outside] {
				// Already `output-feature in out`: a repeated apply is empty.
				continue
			}
			// VPP 26.06 rejects `out <intf> output-feature` on a port that
			// already owns the plain outside role ("nat: error interface
			// already configured", printed as `set interface nat44: add ...
			// failed` with exit code 0). The role must be replaced, not
			// re-added; otherwise the command is silently dropped and the
			// proxy readback fails on a role that was never installed.
			if state.Outside[outside] {
				commands = append(commands, "?set interface nat44 out "+outside+" del")
			}
			commands = append(commands, "?set interface nat44 out "+outside+" output-feature")
			continue
		}
		if state.OutputFeature[outside] {
			// The last proxy egress that needed output-feature is gone. The
			// toggle removes the combined in/out entry, so restore the plain
			// outside role the gateway NAT44 graph owns.
			commands = append(commands,
				"?set interface nat44 out "+outside+" output-feature del",
				"?set interface nat44 out "+outside,
			)
			continue
		}
		// An already plain `out` role is left untouched, so repeated applies
		// are empty.
		if state.Outside[outside] {
			continue
		}
		commands = append(commands, "?set interface nat44 out "+outside)
	}
	return dedupeNAT44Commands(commands)
}

// dedupeNAT44Commands keeps a single instance of each command while preserving
// the delete-before-add order. It makes one apply unable to emit the same
// interface/address mutation twice even if two WANs resolve to one interface.
func nat44InterfaceListed(values []string, name string) bool {
	name = strings.TrimSpace(name)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), name) {
			return true
		}
	}
	return false
}

func dedupeNAT44Commands(commands []string) []string {
	result := make([]string, 0, len(commands))
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if _, exists := seen[command]; exists {
			continue
		}
		seen[command] = struct{}{}
		result = append(result, command)
	}
	return result
}

// nat44IdempotentFailure classifies VPP's "already present"/"already absent"
// errors. These describe the desired end state, so an apply must treat them as
// success instead of retrying the same mutation.
func nat44IdempotentFailure(command, stderr string) bool {
	command = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(command, "?")))
	if !strings.HasPrefix(command, "nat44 ") && !strings.HasPrefix(command, "set interface nat44 ") && !strings.HasPrefix(command, "set interface nat44 ei ") && !strings.HasPrefix(command, "set interface nat44:") {
		return false
	}
	message := strings.ToLower(stderr)
	missing := strings.Contains(message, "not exist") ||
		strings.Contains(message, "no such address") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "does not exist")
	already := strings.Contains(message, "already in use") ||
		strings.Contains(message, "already exists") ||
		strings.Contains(message, "already present")
	hasDelete := strings.HasSuffix(command, " del") || strings.Contains(command, " del ")
	if hasDelete {
		return missing || already || strings.Contains(message, "failed")
	}
	return already || missing
}
