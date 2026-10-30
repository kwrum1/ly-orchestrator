package httpapi

import (
	"context"
	"fmt"
	"strings"

	"ly-route/backend/internal/persistence"
)

// interfaceDesiredRole returns the authoritative role of an interface
// document. An explicitly present gateway_role/role key wins even when its
// value is null or empty: a cleared role must not be resurrected from the
// derived mode_role field left by an older generation.
func interfaceDesiredRole(item map[string]any) (string, bool) {
	for _, key := range []string{"gateway_role", "role"} {
		if value, present := item[key]; present {
			text, _ := value.(string)
			return strings.ToLower(strings.TrimSpace(text)), true
		}
	}
	if modeRole, ok := item["mode_role"].(map[string]any); ok {
		if gateway, ok := modeRole["gateway"].(string); ok && strings.TrimSpace(gateway) != "" {
			return strings.ToLower(strings.TrimSpace(gateway)), false
		}
	}
	return "", false
}

// interfaceRoleClearRequested reports whether the incoming patch explicitly
// clears the role. Both null and an empty string mean "no role"; the merge step
// has not run yet, so this only looks at the caller's payload.
func interfaceRoleClearRequested(payload map[string]any) bool {
	provided := false
	for _, key := range []string{"gateway_role", "role"} {
		value, present := payload[key]
		if !present {
			continue
		}
		provided = true
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "", "none", "unconfigured":
			continue
		}
		// A non-empty role anywhere in the patch means this is a role set or
		// change, not a clear. Only return true when every supplied role field
		// is null/empty.
		return false
	}
	return provided
}

// interfaceAddressPayloadProvided reports whether the caller supplied a usable
// address together with a role change. A role change that omits an address
// retires the address owned by the old role; an explicit new CIDR is the new
// role's address and must survive normalization.
func interfaceAddressPayloadProvided(payload map[string]any) bool {
	for _, key := range []string{"cidr", "ip_cidr", "address", "ip"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// clearInterfaceRoleDerivedFields removes the fields the compiler derives from
// a role. It never touches work_mode/active_path: role edits must not change
// the takeover mode of an already attached port.
func clearInterfaceRoleDerivedFields(payload map[string]any, clearRole bool) {
	keys := []string{"mode_role", "candidate_scopes", "role_configured"}
	if clearRole {
		keys = append(keys, "gateway_role", "role")
	}
	for _, key := range keys {
		delete(payload, key)
	}
}

// interfaceLineScopedFields are the document fields that only describe the
// line a port currently carries: the operator-visible label, the address the
// role owns, the address-family plans and the runtime readback of the applied
// generation. They are not port attributes; a role transition must not let the
// previous line survive behind the new role.
var interfaceLineScopedFields = []string{
	"display_name",
	"cidr", "ip_cidr", "address", "ip", "gateway",
	"ipv4_address", "ipv6_address", "ipv4", "ipv6",
	"dns_servers", "nat",
	"bridge_members", "members",
	"runtime_state", "capabilities", "current", "degraded", "degraded_reason",
	"route_ready", "pppoe_state", "pppoe_interface", "pppoe_session", "pppoe_session_id",
	"ipv6_ready", "addresses",
	"address_assignment_proven", "address_assignment_vpp_interface_matches",
	"runtime_verified", "runtime_probe",
	"applied_commands", "failed_commands", "readback_state",
}

func deleteInterfaceLineScopedFields(payload map[string]any) {
	for _, key := range interfaceLineScopedFields {
		delete(payload, key)
	}
}

// interfaceRoleCallerLineFields keeps only the line-scoped values supplied by
// this request. A role transition deletes the old line first and then restores
// these, so a caller that creates the new LAN line in the same PATCH (label,
// CIDR, IPv6 plan) keeps exactly its own values instead of inheriting the old
// direction's residue.
func interfaceRoleCallerLineFields(payload map[string]any) map[string]any {
	fields := make(map[string]any)
	for _, key := range interfaceLineScopedFields {
		if value, ok := payload[key]; ok {
			fields[key] = value
		}
	}
	return fields
}

// prepareInterfaceRoleClear enforces the cross-resource contract for clearing a
// port role and returns the dependent DHCP documents to delete in the same
// persistence transaction.
//
// WAN lines, static NAT/port maps, DNS upstreams and policies are explicit
// operator objects: silently deleting them would hide a routing/NAT outage.
// They are rejected with an actionable 422. A DHCP service is only meaningful
// on a LAN port and is owned by that role, so it (and its static bindings) is
// cascaded in the same generation as the role clear.
func (server *Server) prepareInterfaceRoleClear(ctx context.Context, interfaceID string) ([]persistence.ConfigKey, []map[string]any, error) {
	canonical := server.canonicalInterfaceReference(ctx, interfaceID)
	if canonical == "" {
		canonical = strings.TrimSpace(interfaceID)
	}
	if canonical != "" && canonical == server.managementInterfaceID(ctx) && !server.managementNetworkShared(ctx) {
		return nil, nil, fmt.Errorf("management interface role cannot be cleared")
	}
	references, err := server.interfaceRoleReferences(ctx, canonical)
	if err != nil {
		return nil, nil, fmt.Errorf("interface %s: reference scan failed: %w", canonical, err)
	}
	for _, reference := range references {
		switch reference.Kind {
		case "DHCP service", "DHCP static binding":
			// Cascaded below, including disabled documents that still name the
			// port. A disabled service must not keep a dangling port reference
			// that would silently come back if re-enabled.
			continue
		default:
			return nil, nil, fmt.Errorf("interface %s is still referenced by %s %s; delete or move it before clearing the role", canonical, reference.Kind, reference.ID)
		}
	}
	deletes := make([]persistence.ConfigKey, 0)
	cascaded := make([]map[string]any, 0)
	for _, resourceType := range []string{"dhcp_server", "dhcp_static_binding"} {
		items, itemErr := server.desiredItems(ctx, resourceType)
		if itemErr != nil {
			return nil, nil, fmt.Errorf("interface %s: %s reference scan failed: %w", canonical, resourceType, itemErr)
		}
		for _, item := range items {
			if truthy(item["deleted"]) {
				continue
			}
			if !server.interfaceReferenceMatches(ctx, stringField(item, "interface_id"), canonical) {
				continue
			}
			id := stringField(item, "id")
			if id == "" {
				continue
			}
			deletes = append(deletes, persistence.ConfigKey{ResourceType: resourceType, ResourceID: id})
			cascaded = append(cascaded, map[string]any{"resource_type": resourceType, "id": id, "reason": "port role cleared"})
		}
	}
	return deletes, cascaded, nil
}
