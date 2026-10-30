package vpp

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/selfheal"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

type routePolicyVPPCTLSpec struct {
	policyID int
	aclID    int
	tableID  int
	priority int
	tag      string
	ingress  string
	via      string
	acl      string
	apply    bool
	policy   trafficpolicy.RoutePolicy
}

func (channel vppctlChannel) doRoutePolicyLifecycle(ctx context.Context, operation Operation) (Reply, error) {
	spec, err := parseRoutePolicyVPPCTLSpec(operation)
	if err != nil {
		return Reply{}, err
	}
	// A path that resolves to `local`/receive (VPP `dpo-receive`) or that does
	// not name this policy's service TAP can never reach the proxy egress; it
	// must fail here instead of being installed and silently dropping every
	// matched flow.
	if err := validateRoutePolicyABFVia(spec); err != nil {
		return Reply{}, err
	}
	// A plan is compiled once from the egress identity, but the proxy service
	// TAP may have been re-created by a VPP restart or an egress rebuild. The
	// live VPP interface is the object identity, so rewrite the ABF path (and
	// every command that names it) to the current interface before the
	// delete/apply readback.
	spec, replacedFrom, replacedTo, err := channel.resolveRoutePolicyLiveProxyPath(ctx, spec)
	if err != nil {
		return Reply{}, err
	}
	if replacedFrom != "" && replacedTo != "" && replacedFrom != replacedTo {
		operation.VPPCtlCommands = replaceVPPCTLInterfaceToken(operation.VPPCtlCommands, replacedFrom, replacedTo)
	}
	var results []VPPCTLCommandResult
	if !strings.HasSuffix(operation.Name, ".replay") {
		results, err = channel.removeRoutePolicyState(ctx, operation, spec)
		if err != nil || !spec.apply {
			return routePolicyLifecycleReply(operation, results), err
		}
	} else if !spec.apply {
		return routePolicyLifecycleReply(operation, nil), nil
	}
	created, err := channel.runServiceChainCommands(ctx, operation, routePolicyCreateACLCommand(spec))
	if err != nil {
		return Reply{}, err
	}
	actualACLID, err := allocatedServiceChainACLID(created)
	if err != nil {
		return Reply{}, err
	}
	commands := make([]string, 0, len(operation.VPPCtlCommands)-1)
	for _, raw := range operation.VPPCtlCommands {
		command := strings.TrimSpace(strings.TrimPrefix(raw, "?"))
		command = strings.ReplaceAll(command, "$LY_ROUTE_LAN_INTERFACE", strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_INTERFACE")))
		switch {
		case strings.HasPrefix(command, "set acl-plugin acl "):
			continue
		case strings.HasPrefix(command, "abf policy add "):
			// The ACL plugin allocates a runtime index even when the declarative
			// command carries a stable identity. Replace the numeric token after
			// the `acl` keyword, rather than relying on the locally recomputed
			// stable ID matching the serialized command byte-for-byte.
			command = replaceRoutePolicyACLReference(command, actualACLID)
		case strings.HasPrefix(command, "show acl-plugin acl index "):
			command = fmt.Sprintf("show acl-plugin acl index %d", actualACLID)
		}
		commands = append(commands, command)
	}
	preNATCommands, preNATApplied, err := channel.preNATRoutePolicyCommands(ctx, operation, spec)
	if err != nil {
		return Reply{}, err
	}
	if preNATApplied {
		commands = append(commands, preNATCommands...)
	}
	applied, err := channel.runServiceChainCommands(ctx, operation, commands...)
	if err != nil {
		return Reply{}, err
	}
	results = append(results, created...)
	results = append(results, applied...)
	policyOutput := resultStdoutLast(results, fmt.Sprintf("show abf policy %d", spec.policyID))
	actualObservedACL, present := observedABFACLID(policyOutput)
	if !present || actualObservedACL != actualACLID {
		return Reply{}, snapshotDecodeError("route policy %q ABF readback does not reference allocated ACL %d: observed=%d present=%t policy=%q commands=%q", operation.Resource, actualACLID, actualObservedACL, present, strings.TrimSpace(policyOutput), strings.Join(commands, " | "))
	}
	aclOutput := resultStdoutLast(results, fmt.Sprintf("show acl-plugin acl index %d", actualACLID))
	if !strings.Contains(aclOutput, "tag {"+spec.tag+"}") {
		return Reply{}, snapshotDecodeError("route policy %q ACL tag readback is missing: %q", operation.Resource, aclOutput)
	}
	attachOutput := resultStdoutLast(results, "show abf attach "+spec.ingress)
	if !routePolicyAttached(attachOutput, spec.policyID) {
		return Reply{}, snapshotDecodeError("route policy %q ABF attachment readback is missing: %q", operation.Resource, attachOutput)
	}
	if strings.TrimSpace(resultStdoutLast(results, fmt.Sprintf("show ip fib table %d", spec.tableID))) == "" {
		return Reply{}, snapshotDecodeError("route policy %q private FIB readback is empty", operation.Resource)
	}
	if preNATApplied && !strings.Contains(resultStdoutLast(results, "show ly-route pre-nat-route"), fmt.Sprintf("rule id %d", spec.policyID)) {
		return Reply{}, snapshotDecodeError("route policy %q pre-NAT route readback is missing", operation.Resource)
	}
	return routePolicyLifecycleReply(operation, results), nil
}

// doPreNATRoutePolicyLifecycle is the production lifecycle for ordinary IPv4
// route policies. It deliberately does not create, attach, or delete ABF/ACL
// objects. Older persisted operations may still contain those commands, but
// the pre-NAT plugin is sufficient for the same policy semantics and avoids a
// VPP 25.x ABF/FIB teardown crash during a full policy rebuild.
func (channel vppctlChannel) doPreNATRoutePolicyLifecycle(ctx context.Context, operation Operation) (Reply, error) {
	spec, err := parseRoutePolicyVPPCTLSpec(operation)
	if err != nil {
		return Reply{}, err
	}
	if routePolicyNativeDeleteOperation(operation) {
		return channel.doPreNATRoutePolicyDelete(ctx, operation, spec)
	}
	if !spec.apply {
		return routePolicyLifecycleReply(operation, nil), nil
	}

	addressCommand := "show interface address " + spec.ingress
	addressResults, err := channel.runServiceChainCommands(ctx, operation, addressCommand)
	if err != nil {
		return Reply{}, err
	}
	lanPrefix, err := preNATLANPrefix(resultStdoutLast(addressResults, addressCommand))
	if err != nil {
		return Reply{}, snapshotDecodeError("route policy %q cannot install pre-NAT classifier: %v", spec.policy.ID, err)
	}

	classifier, applied, err := buildPreNATRoutePolicyCommands(spec, lanPrefix)
	if err != nil {
		return Reply{}, err
	}
	if !applied {
		return routePolicyLifecycleReply(operation, addressResults), nil
	}

	commands := preNATRoutePolicyApplyCommands(operation, spec, classifier)
	fibCommand := fmt.Sprintf("show ip fib table %d", spec.tableID)
	commands = append(commands, fibCommand)
	results, err := channel.runServiceChainCommands(ctx, operation, commands...)
	if err != nil {
		return Reply{}, err
	}
	if repair := preNATRoutePolicyRepairCommands(operation, spec, results); len(repair) > 0 {
		repaired, repairErr := channel.runServiceChainCommands(ctx, operation, repair...)
		results = append(results, repaired...)
		if repairErr != nil {
			return Reply{}, repairErr
		}
	}
	results = append(addressResults, results...)
	if strings.TrimSpace(resultStdoutLast(results, fibCommand)) == "" {
		return Reply{}, snapshotDecodeError("route policy %q native private FIB readback is empty", operation.Resource)
	}
	// The classifier rule only selects the private table. Unless the table's
	// default prefix carries the resolved WAN path, every classified packet
	// falls into the table miss and is dropped while every "the rule exists"
	// check still passes. A failed `ip route add` (for example because the WAN
	// underlay was not ready yet after a reboot) must fail the apply, never
	// commit an unreachable policy.
	if expected, required := routePolicyTenantDefaultVia(operation); required {
		if err := verifyRoutePolicyTenantDefault(results, spec.tableID, expected); err != nil {
			return Reply{}, err
		}
	}
	preNATOutput := resultStdoutLast(results, "show ly-route pre-nat-route")
	_, found, readbackErr := preNATRoutePolicySummaryForID(preNATOutput, spec.policyID)
	if readbackErr == nil && !found {
		// The plugin rejects a rule whose private table does not exist yet. The
		// table is created in this same transaction just before the classifier,
		// so a first-attempt miss means the table creation landed late; replay
		// the classifier once now that the table is there.
		replay, replayErr := channel.runServiceChainCommands(ctx, operation, classifier...)
		results = append(results, replay...)
		if replayErr != nil {
			return Reply{}, replayErr
		}
		preNATOutput = resultStdoutLast(results, "show ly-route pre-nat-route")
		_, found, readbackErr = preNATRoutePolicySummaryForID(preNATOutput, spec.policyID)
	}
	if readbackErr != nil {
		return Reply{}, readbackErr
	}
	if !found {
		return Reply{}, snapshotDecodeError("route policy %q native pre-NAT readback is missing", operation.Resource)
	}
	return routePolicyLifecycleReply(operation, results), nil
}

func preNATRoutePolicyRepairCommands(operation Operation, spec routePolicyVPPCTLSpec, results []VPPCTLCommandResult) []string {
	expected, required := routePolicyTenantDefaultVia(operation)
	if !required {
		return nil
	}
	paths, err := policyTableFIBPaths(results, spec.tableID)
	if err == nil && fibPathsSatisfyDefault(paths, expected) {
		return nil
	}
	// Re-issue the table population unconditionally. The lifecycle strips the
	// best-effort "?" marker, so a write that VPP still refuses fails the apply
	// through repairErr instead of silently leaving an empty tenant table.
	commands := make([]string, 0, 2)
	for _, raw := range routePolicyNativeTableCommands(operation) {
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		if strings.HasPrefix(command, "ip route add ") {
			commands = append(commands, command)
		}
	}
	if len(commands) > 0 {
		commands = append(commands, fmt.Sprintf("show ip fib table %d", spec.tableID))
	}
	return commands
}

func preNATRoutePolicyApplyCommands(operation Operation, spec routePolicyVPPCTLSpec, classifier []string) []string {
	if strings.HasSuffix(operation.Name, ".replay") {
		return append([]string(nil), classifier...)
	}
	return preNATRoutePolicyRefreshCommands(operation, spec, classifier)
}

func preNATRoutePolicyRefreshCommands(operation Operation, spec routePolicyVPPCTLSpec, classifier []string) []string {
	commands := []string{
		fmt.Sprintf("?set ly-route pre-nat-route del id %d", spec.policyID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/1", spec.tableID),
		fmt.Sprintf("?ip route del table %d 128.0.0.0/1", spec.tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", spec.tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/32", spec.tableID),
		// PPPoE reconnects and WAN-group changes replace interface DPOs. Merely
		// replacing the default route can leave the private FIB stacked on an old
		// DPO, so destroy and recreate the now-unreferenced table before replay.
		fmt.Sprintf("?ip table del %d", spec.tableID),
	}
	commands = append(commands, routePolicyNativeTableCommands(operation)...)
	return append(commands, classifier...)
}

func (channel vppctlChannel) doPreNATRoutePolicyDelete(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec) (Reply, error) {
	preNATCommand := fmt.Sprintf("?set ly-route pre-nat-route del id %d", spec.policyID)
	fibCommand := fmt.Sprintf("show ip fib table %d", spec.tableID)
	results, err := channel.runServiceChainCommands(ctx, operation,
		preNATCommand,
		fmt.Sprintf("?ip route del table %d 0.0.0.0/1", spec.tableID),
		fmt.Sprintf("?ip route del table %d 128.0.0.0/1", spec.tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", spec.tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/32", spec.tableID),
		fmt.Sprintf("?ip table del %d", spec.tableID),
		"show ly-route pre-nat-route",
		fibCommand,
	)
	if err != nil {
		return Reply{}, err
	}
	if _, found, readbackErr := preNATRoutePolicySummaryForID(resultStdoutLast(results, "show ly-route pre-nat-route"), spec.policyID); readbackErr != nil {
		return Reply{}, readbackErr
	} else if found {
		return Reply{}, snapshotDecodeError("route policy %q native pre-NAT rule remains after deletion", operation.Resource)
	}
	// Removing the classifier is not enough: a retained tenant table that
	// still owns a forwarding default can keep steering traffic for any other
	// rule that names it, and shows up as a live policy in later audits. The
	// table object itself is retained by VPP 26.06 safety rules
	// (skipDeletedTableCommand), so require the routes, not the header, to be
	// gone.
	if fibTableHasForwardingDefault(resultStdoutLast(results, fibCommand)) {
		return Reply{}, snapshotDecodeError("route policy %q tenant table %d still forwards after deletion", operation.Resource, spec.tableID)
	}
	return routePolicyLifecycleReply(operation, results), nil
}

func routePolicySupportsNativePreNAT(policy trafficpolicy.RoutePolicy) bool {
	action := strings.ToLower(strings.TrimSpace(policy.Action))
	if action == "" {
		action = "route"
	}
	if action == "deny" {
		return false
	}
	// A WAN-group policy has no single WANPath: its private FIB resolves
	// through the group table. The native pre-NAT classifier is the only
	// classifier that runs before NAT rewrites the LAN source, so a group
	// policy must use it. Falling back to ABF makes the source match
	// impossible after NAT and the WAN group never carries LAN traffic.
	if policy.Path == nil && strings.TrimSpace(policy.Egress) == "" && strings.TrimSpace(policy.NextHop) == "" {
		// A policy without any target cannot be classified. A policy that names
		// only a next hop can: the native table is built from the same
		// `ip route add ... via <next-hop>` command the ABF builder emits, and
		// the readback already expects the native classifier for it. Falling
		// back to ABF here made the apply fail with "pre-NAT classifier is
		// missing" (and, on this VPP build, took the ACL teardown path that
		// crashed the data plane).
		return false
	}
	// A proxy-service handoff keeps the full ABF policy/attach lifecycle even
	// though the pre-NAT classifier is also installed by that lifecycle.  The
	// ABF object is the readback that proves the proxy route exists; silently
	// replacing it with the native-only path let an apply commit while the
	// proxy steering was absent.
	if routePolicyUsesProxyServiceInterface(policy) {
		return false
	}
	sources, sourceErr := preNATIPv4Selectors(policy.Match.Sources)
	destinations, destinationErr := preNATIPv4Selectors(policy.Match.Destinations)
	_, sourcePortErr := preNATPortRanges(policy.Match.SourcePorts)
	_, destinationPortErr := preNATPortRanges(policy.Match.DestPorts)
	return sourceErr == nil && destinationErr == nil && sourcePortErr == nil && destinationPortErr == nil && len(sources) > 0 && len(destinations) > 0
}

func routePolicyNativeDeleteOperation(operation Operation) bool {
	if strings.Contains(operation.Name, ".pre-delete") || strings.HasSuffix(operation.Name, ".rollback-delete") {
		return true
	}
	// A plain delete plan that still owns ABF objects is not native: the ABF
	// policy/ACL must be detached and removed before the classifier and the
	// tenant table go away. The old rule treated every `abf policy del`
	// operation as native and skipped the ABF teardown entirely, leaving a
	// dangling policy that blackholed the policy destination.
	if routePolicyOperationOwnsABF(operation) {
		return false
	}
	return operationHasCommand(operation, "ip table del")
}

// routePolicyOperationOwnsABF reports whether an operation creates, attaches
// or deletes an ABF object. Proxy route policies are the only plan shape that
// does; ordinary/native policies use the pre-NAT classifier alone.
func routePolicyOperationOwnsABF(operation Operation) bool {
	for _, raw := range operation.VPPCtlCommands {
		command := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?")))
		switch {
		case strings.HasPrefix(command, "abf policy add "),
			strings.HasPrefix(command, "abf policy del "),
			strings.HasPrefix(command, "abf attach ip4 "):
			return true
		}
	}
	return false
}

func routePolicyNativeTableCommands(operation Operation) []string {
	commands := make([]string, 0, len(operation.VPPCtlCommands))
	for _, raw := range operation.VPPCtlCommands {
		trimmed := strings.TrimSpace(raw)
		command := strings.TrimSpace(strings.TrimPrefix(trimmed, "?"))
		switch {
		case strings.HasPrefix(command, "ip table add "),
			strings.HasPrefix(command, "set ip flow-hash table "),
			strings.HasPrefix(command, "ip route add "):
			commands = append(commands, trimmed)
		}
	}
	return commands
}

func replaceRoutePolicyACLReference(command string, aclID int) string {
	fields := strings.Fields(command)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] != "acl" {
			continue
		}
		if _, err := strconv.Atoi(fields[index+1]); err != nil {
			continue
		}
		fields[index+1] = strconv.Itoa(aclID)
		return strings.Join(fields, " ")
	}
	return command
}

// resolveRoutePolicyLiveProxyPath maps the proxy ingress TAP frozen into a
// plan onto the live TAP that exists in the current VPP topology.
//
// The apply is the last point where a wrong path can be caught. A plan that
// still names a TAP which was deleted (or that was compiled before the egress
// service network existed) must not be replayed verbatim: VPP rejects an
// unknown interface, and a policy that is skipped or half-installed leaves the
// LAN traffic with no service path. The live TAP is therefore resolved by its
// service /30, not by its name, and a missing service network fails the apply
// so the egress can be repaired and retried.
func (channel vppctlChannel) resolveRoutePolicyLiveProxyPath(ctx context.Context, spec routePolicyVPPCTLSpec) (routePolicyVPPCTLSpec, string, string, error) {
	path, ok := parseProxyIngressPath(spec.via)
	if !ok {
		// WAN paths, WAN-group tables and lookup DPOs are not service paths and
		// keep their planned target.
		return spec, "", "", nil
	}
	if !spec.apply {
		// Deletes read the live path back from the policy itself; a cleanup must
		// never fail because the egress (and therefore its TAP) is already gone.
		return spec, "", "", nil
	}
	output, err := channel.runReadOnlyCommand(ctx, "show", "interface")
	if err != nil {
		return spec, "", "", fmt.Errorf("route policy %q: read live VPP interfaces: %w", spec.policy.ID, err)
	}
	live := parseVPPInterfaceNames(output)
	addresses, addressErr := channel.proxyServiceIngressAddresses(ctx)
	if addressErr != nil {
		return spec, "", "", snapshotDecodeError("route policy %q: %v; refusing to install an unverified proxy ABF path", spec.policy.ID, addressErr)
	}
	if live[path.TAP] {
		if !interfaceHasNetwork(addresses[path.TAP], path.Network) {
			return spec, "", "", snapshotDecodeError("route policy %q: live service TAP %q does not carry %s yet; refusing to install an ABF path into an unconfigured service network", spec.policy.ID, path.TAP, path.Network)
		}
		return spec, "", "", nil
	}
	candidates := liveProxyServiceIngress(live, addresses, path.Network)
	switch len(candidates) {
	case 0:
		return spec, "", "", snapshotDecodeError("route policy %q: proxy service TAP %q (%s) is not live; refusing to install an ABF path that cannot reach the egress (proxy service network is not ready)", spec.policy.ID, path.TAP, path.Network)
	case 1:
	default:
		return spec, "", "", snapshotDecodeError("route policy %q: cannot resolve the live service TAP for %s: %s", spec.policy.ID, path.Network, strings.Join(candidates, ", "))
	}
	liveName := candidates[0]
	fields := strings.Fields(spec.via)
	fields[len(fields)-1] = liveName
	spec.via = strings.Join(fields, " ")
	return spec, path.TAP, liveName, nil
}

// replaceVPPCTLInterfaceToken swaps one whole CLI token in every command. The
// planner emits interface names as standalone tokens, so a deleted/re-created
// TAP cannot be confused with a longer name sharing the prefix.
func replaceVPPCTLInterfaceToken(commands []string, from, to string) []string {
	if from == "" || to == "" || from == to {
		return commands
	}
	rewritten := make([]string, 0, len(commands))
	for _, raw := range commands {
		fields := strings.Fields(raw)
		changed := false
		for index, field := range fields {
			if field == from {
				fields[index] = to
				changed = true
			}
		}
		if changed {
			rewritten = append(rewritten, strings.Join(fields, " "))
			continue
		}
		rewritten = append(rewritten, raw)
	}
	return rewritten
}

// repairRoutePolicyEmptyABF restores the desired ABF path on an owned policy
// whose live path list is empty. VPP refuses (and can crash on) deleting such a
// policy, so repair first and let the normal delete/rebuild continue. Only an
// ACL tagged with this policy's own ly-route tag may be repaired.
func (channel vppctlChannel) repairRoutePolicyEmptyABF(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec, aclID int) ([]VPPCTLCommandResult, error) {
	inventory, err := channel.runServiceChainCommands(ctx, operation, "show acl-plugin acl")
	if err != nil {
		return nil, err
	}
	tag := aclInventoryTag(resultStdoutLast(inventory, "show acl-plugin acl"), aclID)
	if tag != spec.tag {
		return nil, snapshotDecodeError("route policy %q has 0 live ABF paths and ACL %d is not owned by ly-route (tag %q); refusing unsafe repair", operation.Resource, aclID, tag)
	}
	via := strings.TrimSpace(spec.via)
	if via == "" {
		return nil, snapshotDecodeError("route policy %q has 0 live ABF paths and no desired next hop; delete the proxy egress or the policy and re-apply", operation.Resource)
	}
	repaired, err := channel.runServiceChainCommands(ctx, operation,
		fmt.Sprintf("abf policy add id %d acl %d via %s", spec.policyID, aclID, via),
		fmt.Sprintf("show abf policy %d", spec.policyID))
	if err != nil {
		return nil, err
	}
	output := resultStdoutLast(repaired, fmt.Sprintf("show abf policy %d", spec.policyID))
	if observedABFPathCount(output) == 0 {
		return nil, snapshotDecodeError("route policy %q has 0 live ABF paths and the desired next hop %q was not accepted; delete the proxy egress or the policy and re-apply", operation.Resource, via)
	}
	return append(inventory, repaired...), nil
}

// aclInventoryTag returns the tag of one ACL in `show acl-plugin acl` output.
func aclInventoryTag(output string, aclID int) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "acl-index" || fields[2] != "count" {
			continue
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil || id != aclID {
			continue
		}
		if len(fields) >= 6 && fields[4] == "tag" {
			return strings.Trim(fields[5], "{}")
		}
		return ""
	}
	return ""
}

func (channel vppctlChannel) removeRoutePolicyState(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec) ([]VPPCTLCommandResult, error) {
	preNATRemoved, preNATErr := channel.runServiceChainCommands(ctx, operation,
		fmt.Sprintf("?set ly-route pre-nat-route del id %d", spec.policyID),
		"show ly-route pre-nat-route",
	)
	if preNATErr != nil {
		return nil, preNATErr
	}
	if preNATRulePresent(resultStdoutLast(preNATRemoved, "show ly-route pre-nat-route"), spec.policyID) {
		return nil, snapshotDecodeError("route policy %q pre-NAT rule %d remains after deletion", operation.Resource, spec.policyID)
	}
	results, err := channel.runServiceChainCommands(ctx, operation,
		fmt.Sprintf("show abf policy %d", spec.policyID),
		"show ip fib summary")
	if err != nil {
		return nil, err
	}
	results = append(preNATRemoved, results...)
	policyOutput := resultStdout(results, fmt.Sprintf("show abf policy %d", spec.policyID))
	if actualACLID, present := observedABFACLID(policyOutput); present {
		paths := observedABFPathCount(policyOutput)
		if paths == 0 {
			// VPP cannot delete an ABF policy whose path list is already empty:
			// `abf policy del` requires at least one via token and the plugin
			// dereferences the invalid path list. Re-add the desired path first,
			// which turns the stale 0-path object back into a deletable policy,
			// then continue with the normal detach/delete/rebuild sequence.
			repaired, repairErr := channel.repairRoutePolicyEmptyABF(ctx, operation, spec, actualACLID)
			if repairErr != nil {
				return nil, repairErr
			}
			results = append(results, repaired...)
			policyOutput = resultStdoutLast(results, fmt.Sprintf("show abf policy %d", spec.policyID))
			paths = observedABFPathCount(policyOutput)
			selfheal.Record(ctx, "abf")
		}
		if paths != 1 {
			return nil, snapshotDecodeError("route policy %q has %d live ABF paths; refusing unsafe in-place deletion", operation.Resource, paths)
		}
		via := routePolicyABFDeleteViaWithInterfaces(spec, policyOutput, resultStdout(results, "show ip fib summary"), channel.liveInterfaceIndexes(ctx))
		if spec.apply && normalizeABFVia(via) != normalizeABFVia(spec.via) {
			// The single live path is not the path the plan wants: `local`
			// (dpo-receive) from an older generation, or a TAP that was
			// replaced. The detach/delete/rebuild below is the repair.
			selfheal.Record(ctx, "abf")
		}
		detached, detachErr := channel.detachRoutePolicyABF(ctx, operation, spec)
		results = append(results, detached...)
		if detachErr != nil {
			return nil, detachErr
		}
		removed, removeErr := channel.deleteRoutePolicyABFPolicy(ctx, operation, spec, actualACLID, via)
		results = append(results, removed...)
		if removeErr != nil {
			return nil, removeErr
		}
		aclRemoved, aclErr := channel.runServiceChainCommands(ctx, operation,
			fmt.Sprintf("delete acl-plugin acl index %d", actualACLID),
			fmt.Sprintf("show acl-plugin acl index %d", actualACLID))
		if aclErr != nil {
			return nil, aclErr
		}
		results = append(results, aclRemoved...)
		if strings.TrimSpace(resultStdoutLast(results, fmt.Sprintf("show acl-plugin acl index %d", actualACLID))) != "" {
			return nil, snapshotDecodeError("route policy %q ACL %d remains after deletion", operation.Resource, actualACLID)
		}
	}
	inventory, err := channel.runServiceChainCommands(ctx, operation, "show acl-plugin acl")
	if err != nil {
		return nil, err
	}
	results = append(results, inventory...)
	for _, id := range taggedServiceChainACLIDs(resultStdoutLast(results, "show acl-plugin acl"), spec.tag) {
		removed, removeErr := channel.runServiceChainCommands(ctx, operation, fmt.Sprintf("delete acl-plugin acl index %d", id))
		if removeErr != nil {
			return nil, removeErr
		}
		results = append(results, removed...)
	}
	cleaned, err := channel.runServiceChainCommands(ctx, operation,
		// VPP 25.x can retain a recursive-resolution lock when a private FIB
		// still owns its default/sentinel paths.  Remove those owned paths
		// explicitly before asking VPP to destroy the table; otherwise an
		// apparently successful `ip table del` leaves a ghost FIB behind.
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", spec.tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/32", spec.tableID),
		fmt.Sprintf("?ip table del %d", spec.tableID),
		fmt.Sprintf("show abf policy %d", spec.policyID),
		"show acl-plugin acl",
		"show abf attach "+spec.ingress,
		fmt.Sprintf("show ip fib table %d", spec.tableID))
	if err != nil {
		return nil, err
	}
	results = append(results, cleaned...)
	if _, present := observedABFACLID(resultStdoutLast(results, fmt.Sprintf("show abf policy %d", spec.policyID))); present {
		return nil, snapshotDecodeError("route policy %q ABF policy remains after deletion", operation.Resource)
	}
	if len(taggedServiceChainACLIDs(resultStdoutLast(results, "show acl-plugin acl"), spec.tag)) != 0 {
		return nil, snapshotDecodeError("route policy %q tagged ACL remains after deletion", operation.Resource)
	}
	if routePolicyAttached(resultStdoutLast(results, "show abf attach "+spec.ingress), spec.policyID) {
		return nil, snapshotDecodeError("route policy %q remains attached after deletion", operation.Resource)
	}
	if fibTableHasForwardingDefault(resultStdoutLast(results, fmt.Sprintf("show ip fib table %d", spec.tableID))) {
		return nil, snapshotDecodeError("route policy %q private FIB still forwards after deletion", operation.Resource)
	}
	return results, nil
}

// preNATRulePresent reports whether `show ly-route pre-nat-route` still lists
// the classifier rule. It intentionally tolerates an empty/disabled inventory:
// absence is the only fact a delete needs to prove.
func preNATRulePresent(output string, policyID int) bool {
	wanted := strconv.Itoa(policyID)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "rule" && fields[1] == "id" && fields[2] == wanted {
			return true
		}
	}
	return false
}

// routePolicyAttachCandidates lists every product interface that may carry the
// policy. The planned ingress is always checked; the live `show interface`
// inventory adds any other LAN/service interface so a policy attached by an
// older generation (or to a renamed LAN) is not missed.
func (channel vppctlChannel) routePolicyAttachCandidates(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec) []string {
	seen := map[string]struct{}{}
	add := func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			seen[name] = struct{}{}
		}
	}
	add(spec.ingress)
	for _, raw := range operation.VPPCtlCommands {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?")))
		if len(fields) == 0 || fields[0] != "abf" || fields[1] != "attach" || fields[2] != "ip4" {
			continue
		}
		switch {
		case fields[3] == "policy" && len(fields) >= 8:
			add(fields[7])
		case fields[3] == "del" && len(fields) >= 7:
			add(fields[len(fields)-1])
		}
	}
	if output, err := channel.runReadOnlyCommand(ctx, "show", "interface"); err == nil {
		for name := range parseVPPInterfaceNames(output) {
			lowered := strings.ToLower(name)
			if strings.HasPrefix(lowered, "lyroute-") || strings.HasPrefix(lowered, "lypx") {
				add(name)
			}
		}
	}
	interfaces := make([]string, 0, len(seen))
	for name := range seen {
		interfaces = append(interfaces, name)
	}
	sort.Strings(interfaces)
	return interfaces
}

// detachRoutePolicyABF removes the policy from every interface that reports
// it. VPP refuses to delete a policy that is still attached, and a policy
// attached to a NIC that the plan no longer names would otherwise survive the
// delete and keep its path alive.
func (channel vppctlChannel) detachRoutePolicyABF(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec) ([]VPPCTLCommandResult, error) {
	var results []VPPCTLCommandResult
	for _, ingress := range channel.routePolicyAttachCandidates(ctx, operation, spec) {
		showCommand := "show abf attach " + ingress
		observed, err := channel.runServiceChainCommands(ctx, operation, "?"+showCommand)
		results = append(results, observed...)
		if err != nil {
			return results, err
		}
		if !routePolicyAttached(resultStdoutLast(results, showCommand), spec.policyID) {
			continue
		}
		detached, detachErr := channel.runServiceChainCommands(ctx, operation,
			fmt.Sprintf("abf attach ip4 del policy %d %s", spec.policyID, ingress),
			"?"+showCommand)
		results = append(results, detached...)
		if detachErr != nil {
			return results, detachErr
		}
		if routePolicyAttached(resultStdoutLast(results, showCommand), spec.policyID) {
			return results, snapshotDecodeError("route policy %q remains attached to %q; refusing unsafe policy deletion", operation.Resource, ingress)
		}
	}
	return results, nil
}

// deleteRoutePolicyABFPolicy removes a live ABF policy. The exact-path form is
// preferred, but a dangling path (the service TAP was deleted first) cannot be
// reconstructed; the bare `abf policy del id <id>` form is the documented
// cleanup for that shape. Both are proven by an ABF readback.
func (channel vppctlChannel) deleteRoutePolicyABFPolicy(ctx context.Context, operation Operation, spec routePolicyVPPCTLSpec, aclID int, via string) ([]VPPCTLCommandResult, error) {
	showCommand := fmt.Sprintf("show abf policy %d", spec.policyID)
	attempts := make([][]string, 0, 2)
	if strings.TrimSpace(via) != "" {
		attempts = append(attempts, []string{
			fmt.Sprintf("abf policy del id %d acl %d via %s", spec.policyID, aclID, via),
			showCommand,
		})
	}
	attempts = append(attempts, []string{
		fmt.Sprintf("abf policy del id %d", spec.policyID),
		showCommand,
	})
	var results []VPPCTLCommandResult
	var lastErr error
	for _, commands := range attempts {
		removed, err := channel.runServiceChainCommands(ctx, operation, commands...)
		results = append(results, removed...)
		if err != nil {
			lastErr = err
			continue
		}
		if _, present := observedABFACLID(resultStdoutLast(results, showCommand)); !present {
			return results, nil
		}
		lastErr = snapshotDecodeError("route policy %q ABF policy %d remains after `%s`", operation.Resource, spec.policyID, commands[0])
	}
	if lastErr == nil {
		lastErr = snapshotDecodeError("route policy %q ABF policy %d could not be deleted", operation.Resource, spec.policyID)
	}
	return results, lastErr
}

// normalizeABFVia compares two ABF path descriptions token by token. VPP
// prints paths with a trailing comma and, for a receive path, no next hop at
// all ("local"), so byte equality would report a false repair on every apply.
func normalizeABFVia(via string) string {
	fields := strings.Fields(strings.ToLower(strings.ReplaceAll(via, ",", " ")))
	return strings.Join(fields, " ")
}

func routePolicyABFDeleteVia(spec routePolicyVPPCTLSpec, policyOutput, fibSummary string) string {
	return routePolicyABFDeleteViaWithInterfaces(spec, policyOutput, fibSummary, nil)
}

func routePolicyABFDeleteViaWithInterfaces(spec routePolicyVPPCTLSpec, policyOutput, fibSummary string, interfaces map[int]string) string {
	if via := observedABFDeleteVia(policyOutput); via != "" {
		return resolveABFDeleteViaInterface(via, interfaces)
	}
	// VPP 25.x normally prints only the internal fib-index for a recursive
	// ABF path. Resolve that index against the live FIB inventory. Never fall
	// back to the declarative table ID: a partial rollback can remove that
	// table while leaving an ABF object behind, and asking VPP to delete the
	// policy through a nonexistent table can crash the 25.10 ABF plugin.
	if fibIndex, ok := observedABFFibIndex(policyOutput); ok {
		if tableID, ok := ipv4TableIDForFibIndex(fibSummary, fibIndex); ok {
			return fmt.Sprintf("ip4-lookup-in-table %d", tableID)
		}
		return ""
	}
	if tableID, ok := routePolicyLookupTableID(spec.via); ok && ipv4FibSummaryContainsTable(fibSummary, tableID) {
		return strings.TrimSpace(spec.via)
	}
	return ""
}

// resolveABFDeleteViaInterface rewrites the `if_index:<n>` token VPP prints
// for a dangling/renamed ABF path into the live interface name. The bare
// `abf policy del id <id>` form fails with "Hop path must not be empty" for
// such a path, so the exact path must be resolved rather than skipped.
func resolveABFDeleteViaInterface(via string, interfaces map[int]string) string {
	fields := strings.Fields(via)
	if len(fields) != 2 || !strings.HasPrefix(fields[1], "if_index:") {
		return via
	}
	index, err := strconv.Atoi(strings.TrimPrefix(fields[1], "if_index:"))
	if err != nil {
		return via
	}
	if name := strings.TrimSpace(interfaces[index]); name != "" {
		return fields[0] + " " + name
	}
	return via
}

// liveInterfaceIndexes maps VPP sw_if_index to interface name for paths that
// print `if_index:<n>` instead of a name.
func (channel vppctlChannel) liveInterfaceIndexes(ctx context.Context) map[int]string {
	output, err := channel.runReadOnlyCommand(ctx, "show", "interface")
	if err != nil {
		return nil
	}
	indexes := map[int]string{}
	for _, line := range strings.Split(output, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "Name" {
			continue
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		indexes[index] = fields[0]
	}
	return indexes
}

var routePolicyFibIndexPattern = regexp.MustCompile("\\bfib-index:\\s*([0-9]+)\\b")
var ipv4FibSummaryPattern = regexp.MustCompile("(?m)^ipv4-VRF:([0-9]+),\\s*fib_index:([0-9]+),")
var routePolicyPathPattern = regexp.MustCompile(`(?m)^\s*path:\[[0-9]+\]`)

func observedABFPathCount(output string) int {
	return len(routePolicyPathPattern.FindAllString(output, -1))
}

func observedABFFibIndex(output string) (int, bool) {
	match := routePolicyFibIndexPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil
}

func ipv4TableIDForFibIndex(summary string, wanted int) (int, bool) {
	for _, match := range ipv4FibSummaryPattern.FindAllStringSubmatch(summary, -1) {
		tableID, tableErr := strconv.Atoi(match[1])
		fibIndex, fibErr := strconv.Atoi(match[2])
		if tableErr == nil && fibErr == nil && fibIndex == wanted {
			return tableID, true
		}
	}
	return 0, false
}

func ipv4FibSummaryContainsTable(summary string, wanted int) bool {
	for _, match := range ipv4FibSummaryPattern.FindAllStringSubmatch(summary, -1) {
		tableID, err := strconv.Atoi(match[1])
		if err == nil && tableID == wanted {
			return true
		}
	}
	return false
}

func routePolicyLookupTableID(via string) (int, bool) {
	fields := strings.Fields(strings.TrimSpace(via))
	if len(fields) != 2 || fields[0] != "ip4-lookup-in-table" {
		return 0, false
	}
	tableID, err := strconv.Atoi(fields[1])
	return tableID, err == nil
}

func parseRoutePolicyVPPCTLSpec(operation Operation) (routePolicyVPPCTLSpec, error) {
	id := strings.TrimSpace(operation.Resource)
	if id == "" {
		return routePolicyVPPCTLSpec{}, snapshotDecodeError("route policy resource is empty")
	}
	spec := routePolicyVPPCTLSpec{
		policyID: stableID("route-abf:"+id, 10000, 8999),
		aclID:    stableID("route-acl:"+id, 10000, 49999),
		tableID:  stableID("route-table:"+id, 50000, 49999),
		tag:      "ly-route-" + safeTag(id),
		ingress:  "lyroute-$LY_ROUTE_LAN_INTERFACE",
	}
	if policy, ok := operation.Payload.(trafficpolicy.RoutePolicy); ok {
		spec.policy = policy
		spec.via = routeNextHop(policy)
	}
	for _, raw := range operation.VPPCtlCommands {
		command := strings.TrimSpace(strings.TrimPrefix(raw, "?"))
		switch {
		case strings.HasPrefix(command, "set ly-route pre-nat-route interface "):
			fields := strings.Fields(command)
			if len(fields) >= 5 {
				spec.ingress = fields[4]
			}
		case strings.HasPrefix(command, "set ly-route pre-nat-route add "):
			spec.apply = true
		case strings.HasPrefix(command, "set acl-plugin acl index "):
			spec.acl = command
		case strings.HasPrefix(command, "abf policy add "):
			spec.apply = true
			if index := strings.Index(command, " via "); index >= 0 {
				spec.via = strings.TrimSpace(command[index+5:])
			}
		case strings.HasPrefix(command, "abf attach ip4 policy "):
			fields := strings.Fields(command)
			if len(fields) == 8 {
				spec.priority, _ = strconv.Atoi(fields[6])
				spec.ingress = fields[7]
			}
		case strings.HasPrefix(command, "abf attach ip4 del policy "):
			// The rollback/delete grammar also carries the concrete LAN VPP
			// interface as its final token. Without reading it, rollback falls
			// back to an unresolved environment placeholder.
			fields := strings.Fields(command)
			if len(fields) >= 7 {
				spec.ingress = fields[len(fields)-1]
			}
		}
	}
	// Native pre-NAT plans have no ACL or ABF object by design. Older route
	// plans may contain both representations while an appliance is upgraded,
	// so only require an ACL when the operation has no native classifier.
	if spec.apply && spec.acl == "" && !operationHasCommand(operation, "set ly-route pre-nat-route add") {
		return routePolicyVPPCTLSpec{}, snapshotDecodeError("route policy %q has no ACL command", id)
	}
	// New production plans rewrite the placeholder before execution.  Older
	// persisted cleanup operations may still carry it, so resolve both the
	// legacy Linux-interface variable and the explicit VPP-interface variable.
	if vppIngress := strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_VPP_INTERFACE")); vppIngress != "" {
		spec.ingress = strings.ReplaceAll(spec.ingress, "lyroute-$LY_ROUTE_LAN_INTERFACE", vppIngress)
	}
	spec.ingress = strings.ReplaceAll(spec.ingress, "$LY_ROUTE_LAN_INTERFACE", strings.TrimSpace(os.Getenv("LY_ROUTE_LAN_INTERFACE")))
	if spec.ingress == "" || spec.ingress == "lyroute-" || strings.Contains(spec.ingress, "$LY_ROUTE_LAN_INTERFACE") {
		return routePolicyVPPCTLSpec{}, snapshotDecodeError("route policy %q has no resolved LAN VPP ingress interface", id)
	}
	return spec, nil
}

func routePolicyCreateACLCommand(spec routePolicyVPPCTLSpec) string {
	command := strings.Replace(spec.acl, fmt.Sprintf(" index %d", spec.aclID), "", 1)
	// VPP parses the ACL tag before the rule list. The declarative command map
	// retains tags at the tail for readability, so normalize it for allocation.
	tagIndex := strings.LastIndex(command, " tag ")
	if tagIndex < 0 {
		return command
	}
	tag := command[tagIndex:]
	command = command[:tagIndex]
	for _, marker := range []string{" permit ", " deny "} {
		if ruleIndex := strings.Index(command, marker); ruleIndex >= 0 {
			return command[:ruleIndex] + tag + command[ruleIndex:]
		}
	}
	return command + tag
}

func observedABFACLID(output string) (int, bool) {
	lines := nonBlankLines(output)
	if len(lines) == 0 || strings.Contains(strings.ToLower(lines[0]), "invalid policy") {
		return 0, false
	}
	for _, field := range strings.Fields(lines[0]) {
		if strings.HasPrefix(field, "acl:") {
			id, err := strconv.Atoi(strings.TrimPrefix(field, "acl:"))
			return id, err == nil
		}
	}
	return 0, false
}

func observedABFDeleteVia(output string) string {
	attachedNextHop := false
	for _, line := range nonBlankLines(output) {
		fields := strings.Fields(line)
		if attachedNextHop {
			attachedNextHop = false
			if len(fields) >= 2 {
				nextHop := strings.TrimRight(fields[0], ",:")
				interfaceName := strings.TrimRight(fields[1], ",:")
				address, err := netip.ParseAddr(nextHop)
				if err == nil && address.Is4() && interfaceNameSafe(interfaceName) {
					return address.String() + " " + interfaceName
				}
			}
		}
		if len(fields) >= 5 && strings.HasPrefix(fields[0], "[@") && fields[2] == "via" {
			return fields[3] + " " + strings.TrimRight(fields[4], ",:")
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "attached-nexthop:") {
			attachedNextHop = true
			continue
		}
		if strings.Contains(lower, "lookup") && strings.Contains(lower, "table:") {
			for _, field := range fields {
				if strings.HasPrefix(field, "table:") {
					if _, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(field, "table:"), ",")); err == nil {
						return "ip4-lookup-in-table " + strings.TrimSuffix(strings.TrimPrefix(field, "table:"), ",")
					}
				}
			}
		}
	}
	if strings.Contains(output, "dpo-receive") || strings.Contains(output, "receive:") {
		return "local"
	}
	return ""
}

func routePolicyAttached(output string, policyID int) bool {
	for _, field := range strings.Fields(output) {
		if field == "policy:"+strconv.Itoa(policyID) {
			return true
		}
	}
	return false
}

func routePolicyLifecycleReply(operation Operation, results []VPPCTLCommandResult) Reply {
	return Reply{Operation: operation.Name, Payload: VPPCTLReplyPayload{CommandResults: results}}
}
