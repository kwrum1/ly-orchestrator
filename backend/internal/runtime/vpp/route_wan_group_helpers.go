package vpp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

func isRoutePolicyApplyOperation(operation Operation) bool {
	if operation.Name != "vpp.route-policy" {
		return false
	}
	for _, command := range operation.VPPCtlCommands {
		if strings.Contains(strings.TrimSpace(strings.TrimPrefix(command, "?")), "abf policy add ") {
			return true
		}
	}
	return false
}

func routePolicyFullRebuildRequired(plan RouteWANGroupPlan, prior Snapshot) bool {
	if len(plan.Routes) == 0 && len(plan.DeleteRoutes) == 0 {
		return false
	}
	desired := plan.Routes
	if len(plan.RoutePolicyContext) > 0 {
		desired = plan.RoutePolicyContext
	}
	desiredGroups := make(map[string]trafficpolicy.WANGroup, len(plan.WANGroups)+len(plan.WANGroupsContext.RouteGroups()))
	for _, group := range plan.WANGroupsContext.RouteGroups() {
		desiredGroups[group.ID] = group
	}
	for _, group := range plan.WANGroups {
		desiredGroups[group.ID] = group
	}
	priorGroups := make(map[string]trafficpolicy.WANGroup, len(prior.WANGroups))
	for _, group := range prior.WANGroups {
		priorGroups[group.ID] = group
	}
	return completeRoutePolicyChain(desired, desiredGroups) || completeRoutePolicyChain(prior.RoutePolicies, priorGroups)
}

func completeRoutePolicyChain(routes []trafficpolicy.RoutePolicy, groups map[string]trafficpolicy.WANGroup) bool {
	if len(routes) < 2 {
		return false
	}
	// Source/port exceptions may precede a destination-only FIB suffix. Any
	// optimized suffix is still a dependency graph and must be rebuilt as one
	// unit when one of its tables changes.
	return len(buildRoutePolicyCommandOptions(routes, groups)) >= 2
}

func (a Adapter) routeWANGroupFailure(ctx context.Context, channel Channel, transactionID, operation string, cause error, prior Snapshot, current RouteWANGroupPlan) error {
	// Restore the previous generation against its own LAN inventory. Using the
	// failed generation's assignments would bind a restored policy to a LAN it
	// never belonged to (or to the default first LAN) when the failed apply
	// changed the LAN address of the policy's source.
	restoreAssignments := current.AddressAssignments
	if len(current.PriorAddressAssignments) > 0 {
		restoreAssignments = current.PriorAddressAssignments
	}
	restoreIngress := firstLANVPPInterface(restoreAssignments)
	if restoreIngress == "" {
		restoreIngress = strings.TrimSpace(current.IngressVPPInterface)
	}
	rollbackErr := errors.Join(cleanupRouteWANGroup(ctx, channel, transactionID, current), applyRouteWANGroupSnapshot(ctx, channel, transactionID, prior, restoreIngress, current.LocalDestinations, restoreAssignments))
	// Restore the previous classifier header explicitly. Replaying policy rules
	// cannot move the plugin's single ingress/prefix pair back; without this the
	// failed apply leaves classification pointing at the wrong LAN.
	if commands := preNATClassifierRestoreCommands(prior.PreNATClassifier); len(commands) > 0 {
		if err := restorePreNATClassifierOnChannel(ctx, channel, transactionID, prior.PreNATClassifier, commands); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	if len(prior.RoutePolicies) > 0 || len(prior.WANGroups) > 0 {
		rollbackRequest := routeWANGroupSnapshotRequest(transactionID, prior.RoutePolicies, prior.WANGroups)
		rollbackRequest.AllowMissing = true
		rollbackRequest.LocalDestinations = append([]string(nil), current.LocalDestinations...)
		// Rollback installs the prior classifier on the LAN that owns each prior
		// policy, so its own readback must expect the same per-policy binding.
		rollbackRequest.LANAddressAssignments = append([]AddressAssignment(nil), restoreAssignments...)
		rollbackRequest.RoutePolicyLANInterfaces = routePolicyIngressByID(prior.RoutePolicies, restoreAssignments)
		readback, err := a.Snapshot(ctx, rollbackRequest)
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		} else if !reflect.DeepEqual(readback.RoutePolicies, prior.RoutePolicies) || !reflect.DeepEqual(readback.WANGroups, prior.WANGroups) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback readback does not match prior snapshot"))
		}
	}
	result := RollbackSucceeded
	if rollbackErr != nil {
		result = RollbackFailed
	}
	return &RouteWANGroupLifecycleError{Operation: operation, Cause: cause, Rollback: rollbackErr, RollbackResult: result}
}

// firstLANVPPInterface returns the first configured LAN in a deterministic
// order. It only decides the generic interface rewrite; per-policy ingress is
// always resolved from the policy source.
func firstLANVPPInterface(assignments []AddressAssignment) string {
	for _, binding := range routePolicyLANBindings(assignments) {
		return binding.VPPInterface
	}
	return ""
}

// priorPreNATClassifier returns the classifier state that must be restored.
// Older snapshots carried no header; in that case the live header is left
// untouched instead of being cleared (which would break a healthy generation).
func priorPreNATClassifier(prior Snapshot) (*PreNATClassifierState, error) {
	return prior.PreNATClassifier, nil
}

func cleanupRouteWANGroup(ctx context.Context, channel Channel, transactionID string, plan RouteWANGroupPlan) error {
	var cleanup []error
	ingress := strings.TrimSpace(plan.IngressVPPInterface)
	if ingress == "" {
		ingress = configuredLANVPPInterface()
	}
	for _, route := range routePoliciesForVPPDelete(routePolicyIDs(plan.Routes), plan.Routes) {
		operation := Operation{Name: "vpp.route-policy.rollback-delete", RequestID: transactionID, Resource: route.ID, Payload: route, VPPCtlCommands: deleteRoutePolicyCommands(route.ID)}
		operation = rewriteRoutePolicyIngress([]Operation{operation}, plan.AddressAssignments)[0]
		operation = rewriteOperationsInterface([]Operation{operation}, ingress)[0]
		if _, err := doOperation(ctx, channel, operation); err != nil {
			cleanup = append(cleanup, err)
		}
	}
	for _, group := range plan.WANGroups {
		if _, err := doOperation(ctx, channel, Operation{Name: "vpp.pbr.next-hop-group.rollback-delete", RequestID: transactionID, Resource: group.ID, VPPCtlCommands: deleteWANGroupCommands(group.ID)}); err != nil {
			cleanup = append(cleanup, err)
		}
	}
	return errors.Join(cleanup...)
}

// routePoliciesForVPPDelete returns policies in dependency-safe teardown
// order. The optimized FIB chain is installed from the terminal policy toward
// the highest-priority policy; it must be removed in the opposite direction.
func routePoliciesForVPPDelete(ids []string, states []trafficpolicy.RoutePolicy) []trafficpolicy.RoutePolicy {
	byID := make(map[string]trafficpolicy.RoutePolicy, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.ID) != "" {
			byID[state.ID] = state
		}
	}
	ordered := make([]trafficpolicy.RoutePolicy, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if state, ok := byID[id]; ok {
			ordered = append(ordered, state)
		} else {
			ordered = append(ordered, trafficpolicy.RoutePolicy{ID: id})
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority == ordered[j].Priority {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Priority < ordered[j].Priority
	})
	return ordered
}

func routePolicyIDs(routes []trafficpolicy.RoutePolicy) []string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		ids = append(ids, route.ID)
	}
	return ids
}

func applyRouteWANGroupSnapshot(ctx context.Context, channel Channel, transactionID string, snapshot Snapshot, ingressVPPInterface string, localDestinations []string, assignments []AddressAssignment) error {
	var replay []error
	ingress := strings.TrimSpace(ingressVPPInterface)
	if ingress == "" {
		ingress = configuredLANVPPInterface()
	}
	groups := make(map[string]trafficpolicy.WANGroup, len(snapshot.WANGroups))
	for _, group := range snapshot.WANGroups {
		groups[group.ID] = group
	}
	for _, group := range snapshot.WANGroups {
		operation := Operation{Name: "vpp.pbr.next-hop-group.rollback", RequestID: transactionID, Resource: group.ID, VPPCtlCommands: wanGroupCommands(group)}
		if _, err := doOperation(ctx, channel, operation); err != nil {
			replay = append(replay, err)
		}
	}
	routes := orderedRoutePoliciesForVPP(snapshot.RoutePolicies, groups)
	routeOptions := buildRoutePolicyCommandOptions(routes, groups)
	if len(assignments) > 0 {
		addRoutePolicyLocalDestinations(routeOptions, routes, assignments)
	} else {
		addRoutePolicyLocalDestinationPrefixes(routeOptions, routes, localDestinations)
	}
	if err := validateLargeRoutePolicyFallback(routes, routeOptions, groups); err != nil {
		return err
	}
	for _, route := range routes {
		option := routeOptions[route.ID]
		operation := Operation{Name: "vpp.route-policy.rollback", RequestID: transactionID, Resource: route.ID, Payload: route, VPPCtlCommands: routePolicyCommandsWithOptions(route, groups, option)}
		operation = rewriteRoutePolicyIngress([]Operation{operation}, assignments)[0]
		operation = rewriteOperationsInterface([]Operation{operation}, ingress)[0]
		if _, err := doOperation(ctx, channel, operation); err != nil {
			replay = append(replay, err)
		}
	}
	return errors.Join(replay...)
}

func routeWANGroupSnapshotRequest(transactionID string, routes []trafficpolicy.RoutePolicy, groups []trafficpolicy.WANGroup) SnapshotRequest {
	request := SnapshotRequest{TransactionID: transactionID}
	if len(groups) > 0 {
		request.Capabilities = append(request.Capabilities, SnapshotCapabilityWANGroups)
		for _, group := range groups {
			request.WANGroups = append(request.WANGroups, group.ID)
		}
		request.Candidates.WANGroups = append(request.Candidates.WANGroups, groups...)
	}
	if len(routes) > 0 {
		request.Capabilities = append(request.Capabilities, SnapshotCapabilityRoutePolicies)
		for _, route := range routes {
			request.RoutePolicies = append(request.RoutePolicies, route.ID)
		}
		request.Candidates.RoutePolicies = append(request.Candidates.RoutePolicies, routes...)
	}
	return request
}

func routeWANGroupSnapshotRequestForPlan(plan RouteWANGroupPlan) SnapshotRequest {
	request := routeWANGroupSnapshotRequest(plan.TransactionID, plan.Routes, plan.WANGroups)
	// The transaction removes dependent resources before repairing their WAN
	// groups. During a deletion-only route phase an unchanged policy may be
	// temporarily unresolved because its group is queued for repair later in
	// the same transaction. Keep proving healthy unchanged routes, but allow a
	// verified missing/drifted one to flow into the already-computed apply phase
	// instead of aborting before its dependency can be rebuilt.
	request.AllowMissing = len(plan.Routes) == 0 && len(plan.DeleteRoutes) > 0
	request.LocalDestinations = append([]string(nil), plan.LocalDestinations...)
	request.LANAddressAssignments = append([]AddressAssignment(nil), plan.AddressAssignments...)
	request.AbsentRoutePolicies = append([]string(nil), plan.DeleteRoutes...)
	request.AbsentWANGroups = append([]string(nil), plan.DeleteWANGroups...)
	for _, route := range plan.RoutePolicyContext {
		request.RoutePolicies = appendUnique(request.RoutePolicies, route.ID)
	}
	request.Candidates.RoutePolicies = appendUniqueRoutePolicies(request.Candidates.RoutePolicies, plan.RoutePolicyContext)
	request.Candidates.RoutePolicies = appendRoutesByID(request.Candidates.RoutePolicies, plan.DeleteRoutes, plan.DeleteRouteState)
	// Resolve the expected classifier only after every candidate (changed,
	// context and deleted) is present; otherwise an unchanged policy on a
	// non-default LAN falls back to the first LAN during readback.
	request.RoutePolicyLANInterfaces = routePolicyIngressByID(request.Candidates.RoutePolicies, plan.AddressAssignments)
	request.Candidates.WANGroups = appendWANGroupsByID(request.Candidates.WANGroups, plan.DeleteWANGroups, plan.DeleteWANState)
	request.Candidates.WANGroups = appendUniqueWANGroups(request.Candidates.WANGroups, plan.WANGroupsContext.RouteGroups())
	if len(plan.WANGroups)+len(plan.DeleteWANGroups) > 0 && !containsSnapshotCapability(request.Capabilities, SnapshotCapabilityWANGroups) {
		request.Capabilities = append(request.Capabilities, SnapshotCapabilityWANGroups)
	}
	if len(plan.Routes)+len(plan.DeleteRoutes) > 0 && !containsSnapshotCapability(request.Capabilities, SnapshotCapabilityRoutePolicies) {
		request.Capabilities = append(request.Capabilities, SnapshotCapabilityRoutePolicies)
	}
	return request
}

func appendUniqueRoutePolicies(existing, additions []trafficpolicy.RoutePolicy) []trafficpolicy.RoutePolicy {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	for _, route := range existing {
		if id := strings.TrimSpace(route.ID); id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, route := range additions {
		id := strings.TrimSpace(route.ID)
		if id == "" {
			continue
		}
		if _, found := seen[id]; found {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, route)
	}
	return existing
}

func appendUniqueWANGroups(groups, additions []trafficpolicy.WANGroup) []trafficpolicy.WANGroup {
	seen := make(map[string]struct{}, len(groups)+len(additions))
	for _, group := range groups {
		seen[group.ID] = struct{}{}
	}
	for _, group := range additions {
		if _, found := seen[group.ID]; found {
			continue
		}
		seen[group.ID] = struct{}{}
		groups = append(groups, group)
	}
	return groups
}

func containsSnapshotCapability(capabilities []SnapshotCapability, wanted SnapshotCapability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func deleteWANGroupCommands(id string) []string {
	tableID := wanGroupTableID(id)
	return []string{fmt.Sprintf("?ip table del %d", tableID), fmt.Sprintf("show ip fib table %d", tableID)}
}

func deleteRoutePolicyCommands(id string) []string {
	aclID := stableID("route-acl:"+id, 10000, 49999)
	policyID := stableID("route-abf:"+id, 10000, 8999)
	tableID := stableID("route-table:"+id, 50000, 49999)
	return []string{
		fmt.Sprintf("?abf attach ip4 del policy %d lyroute-$LY_ROUTE_LAN_INTERFACE", policyID),
		fmt.Sprintf("?abf policy del id %d", policyID),
		fmt.Sprintf("?delete acl-plugin acl index %d", aclID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/1", tableID),
		fmt.Sprintf("?ip route del table %d 128.0.0.0/1", tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/0", tableID),
		fmt.Sprintf("?ip route del table %d 0.0.0.0/32", tableID),
		fmt.Sprintf("?ip table del %d", tableID),
		fmt.Sprintf("show acl-plugin acl index %d", aclID),
		fmt.Sprintf("show abf policy %d", policyID),
		fmt.Sprintf("show ip fib table %d", tableID),
	}
}
