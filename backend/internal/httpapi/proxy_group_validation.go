package httpapi

import (
	"context"
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/proxy"
)

// normalizeProxyGroupPayload is the ctx-free half of proxy group validation.
// A group always has an explicit strategy and a de-duplicated member list so
// the compiler never has to guess.
func normalizeProxyGroupPayload(payload map[string]any) error {
	if strings.TrimSpace(stringField(payload, "name")) == "" {
		return fmt.Errorf("proxy group requires a name")
	}
	strategy := strings.TrimSpace(stringField(payload, "strategy"))
	if strategy == "" {
		strategy = string(proxy.GroupStrategyLatency)
	}
	switch proxy.GroupStrategy(strategy) {
	case proxy.GroupStrategyLatency, proxy.GroupStrategyLoadBalance:
	default:
		return fmt.Errorf("proxy group strategy must be latency or loadbalance")
	}
	payload["strategy"] = strategy
	seen := map[string]struct{}{}
	members := make([]string, 0, len(stringSliceField(payload, "members")))
	for _, rawMember := range stringSliceField(payload, "members") {
		member := strings.TrimSpace(rawMember)
		if member == "" {
			continue
		}
		if _, duplicate := seen[member]; duplicate {
			continue
		}
		seen[member] = struct{}{}
		members = append(members, member)
	}
	if len(members) == 0 {
		return fmt.Errorf("proxy group requires at least one member node")
	}
	payload["members"] = members
	topN := intField(payload, "top_n")
	if topN < 0 {
		return fmt.Errorf("proxy group top_n must not be negative")
	}
	if topN > len(members) {
		// Shrinking the member list (or a subscription retiring nodes) must not
		// dead-end on a stale top_n: clamp it to the new member count.
		topN = len(members)
	}
	if topN > 0 {
		payload["top_n"] = topN
	} else {
		delete(payload, "top_n")
	}
	if probeURL := strings.TrimSpace(stringField(payload, "probe_url")); probeURL != "" {
		if err := proxy.ValidateObservatoryProbeURL(probeURL); err != nil {
			return fmt.Errorf("proxy group probe_url: %w", err)
		}
		payload["probe_url"] = probeURL
	} else {
		delete(payload, "probe_url")
	}
	return nil
}

// validateProxyGroupMembers rejects members that are not live proxy nodes.
// Subscription refresh owns node documents, so a stale member must be fixed
// before the group can be saved instead of failing later at apply time.
func (server *Server) validateProxyGroupMembers(ctx context.Context, payload map[string]any) error {
	nodes, err := server.desiredItems(ctx, "proxy_node")
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(nodes))
	for _, item := range nodes {
		if !desiredEnabled(item) {
			continue
		}
		known[strings.TrimSpace(stringField(item, "id"))] = struct{}{}
	}
	for _, member := range stringSliceField(payload, "members") {
		if _, exists := known[strings.TrimSpace(member)]; !exists {
			return fmt.Errorf("proxy group member %q is not an enabled proxy node", member)
		}
	}
	return nil
}

// proxyDeleteReferenceConflict refuses to delete a node group or node that an
// enabled egress (or group) still depends on.
func (server *Server) proxyDeleteReferenceConflict(ctx context.Context, resourceType, id string) (string, error) {
	switch resourceType {
	case "proxy_group":
		egresses, err := server.desiredItems(ctx, "proxy_egress")
		if err != nil {
			return "", err
		}
		for _, item := range egresses {
			if !desiredEnabled(item) || strings.TrimSpace(stringField(item, "group_id")) != id {
				continue
			}
			return fmt.Sprintf("proxy group %s is still used by proxy egress %s; delete or re-point it first", id, stringField(item, "id")), nil
		}
	case "proxy_node":
		groups, err := server.desiredItems(ctx, "proxy_group")
		if err != nil {
			return "", err
		}
		for _, item := range groups {
			if !desiredEnabled(item) {
				continue
			}
			for _, member := range stringSliceField(item, "members") {
				if strings.TrimSpace(member) == id {
					return fmt.Sprintf("proxy node %s is still a member of proxy group %s; remove it from the group first", id, stringField(item, "id")), nil
				}
			}
		}
	}
	return "", nil
}

// validateProxyGroupReference rejects an egress that names a missing or
// disabled group. The generic desired-state path and the legacy egress write
// path both call it so neither can persist a dangling reference.
func (server *Server) validateProxyGroupReference(ctx context.Context, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil
	}
	groups, err := server.desiredItems(ctx, "proxy_group")
	if err != nil {
		return err
	}
	for _, item := range groups {
		if strings.TrimSpace(stringField(item, "id")) != groupID {
			continue
		}
		if !desiredEnabled(item) {
			return fmt.Errorf("proxy group %q is disabled", groupID)
		}
		return nil
	}
	return fmt.Errorf("proxy group %q does not exist", groupID)
}
