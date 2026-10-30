package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// interfaceRoleReference is one desired-state consumer that requires a port to
// keep a particular role. A port can only be one thing; accepting a role that
// contradicts an existing LAN line, WAN line, NAT binding or DHCP service
// creates a desired generation that can never converge (the WAN address and the
// LAN address race on the same VPP interface).
type interfaceRoleReference struct {
	Kind string
	ID   string
	Role string
}

// validateInterfaceRoleReferences rejects an interface role change while
// another desired resource still references the port as the opposite role.
// It is intentionally checked before persistence so an impossible desired
// generation is never written and cannot poison every later apply. Every
// conflicting consumer is listed so the operator can fix them in one pass.
func (server *Server) validateInterfaceRoleReferences(ctx context.Context, payload map[string]any) error {
	interfaceID := strings.TrimSpace(firstNonEmpty(stringField(payload, "interface_id"), stringField(payload, "id")))
	interfaceID = server.canonicalInterfaceReference(ctx, interfaceID)
	if interfaceID == "" {
		return nil
	}
	role, _ := interfaceDesiredRole(payload)
	if role != "lan" && role != "wan" {
		return nil
	}
	// A repeated save of the same role is idempotent and may keep consumers
	// whose required role matches. A direction transition retires the line the
	// consumers were written against, so every consumer must be updated first.
	previousRole := server.storedInterfaceRole(ctx, payload)
	roleTransition := previousRole != "" && !strings.EqualFold(previousRole, role)
	references, err := server.interfaceRoleReferencesExcept(ctx, interfaceID, stringField(payload, "id"))
	if err != nil {
		return fmt.Errorf("interface %s: reference scan failed: %w", interfaceID, err)
	}
	conflicts := make([]interfaceRoleReference, 0, len(references))
	for _, reference := range references {
		if !roleTransition && strings.EqualFold(reference.Role, role) {
			continue
		}
		conflicts = append(conflicts, reference)
	}
	if len(conflicts) == 0 {
		return nil
	}
	labels := make([]string, 0, len(conflicts))
	for _, reference := range conflicts {
		labels = append(labels, reference.Kind+" "+reference.ID)
	}
	return fmt.Errorf("interface %s is referenced by %s; update or delete it first", interfaceID, strings.Join(labels, ", "))
}

// storedInterfaceRole returns the role currently persisted for the interface
// document, resolving the same port aliases the PATCH handler merges. It lets
// the cross-resource guard tell an idempotent role save (same role) from a
// direction transition, which must name every dependent object explicitly.
func (server *Server) storedInterfaceRole(ctx context.Context, payload map[string]any) string {
	id := strings.TrimSpace(stringField(payload, "id"))
	if id == "" {
		return ""
	}
	if current, found, err := server.desiredItem(ctx, "interface", id); err == nil && found {
		role, _ := interfaceDesiredRole(current)
		return role
	}
	for _, alias := range server.interfaceDocumentAliases(ctx, id) {
		if current, found, err := server.desiredItem(ctx, "interface", alias); err == nil && found {
			role, _ := interfaceDesiredRole(current)
			return role
		}
	}
	return ""
}

func (server *Server) interfaceRoleReferences(ctx context.Context, interfaceID string) ([]interfaceRoleReference, error) {
	return server.interfaceRoleReferencesExcept(ctx, interfaceID, "")
}

// interfaceRoleReferencesExcept scans every desired collection that owns a
// port. excludeID is the document being written: it is not its own consumer.
func (server *Server) interfaceRoleReferencesExcept(ctx context.Context, interfaceID, excludeID string) ([]interfaceRoleReference, error) {
	references := []interfaceRoleReference{}
	seen := map[string]struct{}{}
	add := func(kind, id, role string) {
		key := kind + "\x00" + id + "\x00" + role
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		references = append(references, interfaceRoleReference{Kind: kind, ID: id, Role: role})
	}
	itemLabel := func(item map[string]any) string {
		return firstNonEmpty(stringField(item, "id"), stringField(item, "name"), stringField(item, "display_name"))
	}
	interfaceItemLabel := func(item map[string]any) string {
		return firstNonEmpty(stringField(item, "display_name"), stringField(item, "name"), stringField(item, "id"))
	}

	// LAN lines: a plain interface document for this port is the port itself,
	// but a logical LAN document or a LAN bridge that lists the port as a
	// member is a separate object and must be retired first.
	interfaceItems, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return nil, err
	}
	for _, item := range interfaceItems {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		itemID := stringField(item, "id")
		if excludeID != "" && strings.EqualFold(itemID, excludeID) {
			continue
		}
		role, _ := interfaceDesiredRole(item)
		isBridge := stringField(item, "kind") == "lan_bridge" || stringField(item, "type") == "lan_bridge"
		if isBridge {
			matched := false
			for _, field := range []string{"bridge_members", "members"} {
				for _, member := range stringSliceField(item, field) {
					if server.interfaceReferenceMatches(ctx, member, interfaceID) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				if role == "" {
					role = "lan"
				}
				add("LAN line", interfaceItemLabel(item), role)
			}
			continue
		}
		// Documents keyed by the product id, the kernel netdev or a legacy
		// spelling of this same port are the port's own document.
		if itemID != "" && server.interfaceReferenceMatches(ctx, itemID, interfaceID) {
			continue
		}
		if server.interfaceReferenceMatches(ctx, stringField(item, "interface_id"), interfaceID) {
			if role == "" {
				role = "lan"
			}
			add("LAN line", interfaceItemLabel(item), role)
		}
	}

	// Bonds own their member ports.
	bonds, err := server.desiredItems(ctx, "interface_bond")
	if err != nil {
		return nil, err
	}
	for _, item := range bonds {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		for _, member := range stringSliceField(item, "members") {
			if server.interfaceReferenceMatches(ctx, member, interfaceID) {
				role, _ := interfaceDesiredRole(item)
				add("interface bond", itemLabel(item), role)
				break
			}
		}
	}

	// WAN lines own the port as WAN. Moving the port to LAN while the line
	// still points at it is the exact cross-resource contradiction that
	// produced the J1900 deadlock.
	wanIDs := map[string]struct{}{}
	wanLinks, err := server.desiredItems(ctx, "wan_link")
	if err != nil {
		return nil, err
	}
	for _, item := range wanLinks {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		if server.interfaceReferenceMatches(ctx, firstNonEmpty(stringField(item, "interface_id"), stringField(item, "system_name")), interfaceID) {
			add("wan link", itemLabel(item), "wan")
			if id := stringField(item, "id"); id != "" {
				wanIDs[id] = struct{}{}
			}
		}
	}
	// NAT static mappings and port maps name their outside/WAN interface (or a
	// WAN line id in the same field).
	for _, resource := range []struct {
		ResourceType string
		Kind         string
	}{
		{"port_map", "port map"},
		{"nat_static", "static NAT mapping"},
	} {
		items, err := server.desiredItems(ctx, resource.ResourceType)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if truthy(item["deleted"]) || !desiredEnabled(item) {
				continue
			}
			matched := false
			for _, field := range []string{"wan_interface", "interface_id"} {
				if server.interfaceReferenceMatches(ctx, stringField(item, field), interfaceID) {
					add(resource.Kind, itemLabel(item), "wan")
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			// A mapping may name only the WAN line that owns the port.
			if link := firstStringField(item, "wan_link", "wan_link_id"); link != "" {
				if _, usesWAN := wanIDs[link]; usesWAN {
					add(resource.Kind, itemLabel(item), "wan")
				}
			}
		}
	}
	// Proxy egress underlays ride a WAN line, so they inherit its port
	// requirement. List them explicitly: the operator must move or delete the
	// egress, not just the line it happens to use.
	egresses, err := server.desiredItems(ctx, "proxy_egress")
	if err != nil {
		return nil, err
	}
	for _, item := range egresses {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		underlay := firstStringField(item, "underlay_wan_id", "underlay_wan")
		_, usesWAN := wanIDs[underlay]
		if usesWAN || server.interfaceReferenceMatches(ctx, underlay, interfaceID) {
			add("proxy egress underlay", itemLabel(item), "wan")
		}
	}
	// DHCP services and static bindings require a LAN port.
	for _, resource := range []struct {
		ResourceType string
		Kind         string
	}{
		{"dhcp_server", "DHCP service"},
		{"dhcp_static_binding", "DHCP static binding"},
	} {
		items, err := server.desiredItems(ctx, resource.ResourceType)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if truthy(item["deleted"]) || !desiredEnabled(item) {
				continue
			}
			if server.interfaceReferenceMatches(ctx, stringField(item, "interface_id"), interfaceID) {
				add(resource.Kind, itemLabel(item), "lan")
			}
		}
	}
	// A DNS upstream may pin the physical interface used by its probe. Keeping
	// that reference after the port role is cleared leaves a probe bound to a
	// port the product no longer owns.
	upstreamIDs := map[string]struct{}{}
	upstreams, err := server.desiredItems(ctx, "dns_upstream")
	if err != nil {
		return nil, err
	}
	for _, item := range upstreams {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		if server.interfaceReferenceMatches(ctx, stringField(item, "interface_id"), interfaceID) {
			add("DNS upstream", itemLabel(item), "lan")
			if id := stringField(item, "id"); id != "" {
				upstreamIDs[id] = struct{}{}
			}
		}
	}
	// A DNS policy that selects a port-pinned upstream also depends on the
	// port, even though it never names the interface directly.
	if len(upstreamIDs) > 0 {
		policies, err := server.desiredItems(ctx, "dns_policy")
		if err != nil {
			return nil, err
		}
		for _, item := range policies {
			if truthy(item["deleted"]) || !desiredEnabled(item) {
				continue
			}
			if referencesAnyStringValue(item["policy"], "upstream_id", upstreamIDs) {
				add("DNS policy", itemLabel(item), "lan")
			}
		}
	}
	// Policies that explicitly pin a LAN ingress must not outlive the port's
	// LAN role. Most policies reference a WAN line/group instead, which is
	// covered transitively by the WAN-link scan above.
	for _, resource := range []struct {
		ResourceType string
		Kind         string
	}{
		{"route_policy", "route policy"},
		{"traffic_control", "traffic control policy"},
	} {
		items, err := server.desiredItems(ctx, resource.ResourceType)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if truthy(item["deleted"]) || !desiredEnabled(item) {
				continue
			}
			for _, field := range []string{"ingress_interface", "lan_interface"} {
				if server.interfaceReferenceMatches(ctx, stringField(item, field), interfaceID) {
					add(resource.Kind, itemLabel(item), "lan")
					break
				}
			}
		}
	}
	sort.SliceStable(references, func(left, right int) bool {
		if references[left].Kind != references[right].Kind {
			return references[left].Kind < references[right].Kind
		}
		return references[left].ID < references[right].ID
	})
	return references, nil
}

// referencesAnyStringValue walks a decoded JSON value and reports whether any
// object with the given key points at one of the wanted values.
func referencesAnyStringValue(value any, key string, wanted map[string]struct{}) bool {
	switch typed := value.(type) {
	case map[string]any:
		for field, child := range typed {
			if field == key {
				if text, ok := child.(string); ok {
					if _, match := wanted[strings.TrimSpace(text)]; match {
						return true
					}
				}
			}
			if referencesAnyStringValue(child, key, wanted) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if referencesAnyStringValue(child, key, wanted) {
				return true
			}
		}
	}
	return false
}

// interfaceReferenceMatches resolves every accepted spelling (product id,
// kernel netdev, logical name, MAC, PCI) before comparing.
func (server *Server) interfaceReferenceMatches(ctx context.Context, value, interfaceID string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.EqualFold(value, interfaceID) {
		return true
	}
	return strings.EqualFold(server.canonicalPortReference(ctx, value), interfaceID)
}
