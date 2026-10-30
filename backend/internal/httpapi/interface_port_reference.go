package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"ly-route/backend/internal/persistence"
)

// Plan B interface contract for desired state.
//
// The product id (`eth3`) is the only spelling that may be stored or returned
// by the control plane. The kernel netdev (`ens224`) is an internal attribute:
// it is kept in `system_name` because only the dataplane layer (VPP attach,
// LCP taps, `ping -I`) needs it, and it is resolved there through the identity
// service, never taken from a user-visible field.
//
// Every write accepts a product id, the kernel netdev name, the logical name
// the operator gave the port, its MAC address or its PCI address; all of them
// resolve to the product id before validation and persistence.

// portReferenceFields lists, per desired resource type, the payload keys that
// name a physical port. A key suffixed with "[]" holds a list of ports.
var portReferenceFields = map[string][]string{
	"management_network":  {"interface_id"},
	"interface_bond":      {"members[]"},
	"dhcp_server":         {"interface_id"},
	"dhcp_static_binding": {"interface_id"},
	"port_map":            {"wan_interface", "interface_id"},
	"nat_static":          {"wan_interface", "interface_id"},
}

// normalizePortReferences rewrites every port reference in a write payload to
// the canonical product id. Values that do not resolve to a port on this device
// are left untouched: some of those fields also accept a WAN line id, and
// rejecting them here would break NAT and port-map editing.
func (server *Server) normalizePortReferences(ctx context.Context, resourceType string, payload map[string]any) {
	fields, ok := portReferenceFields[resourceType]
	if !ok || len(payload) == 0 {
		return
	}
	for _, field := range fields {
		if strings.HasSuffix(field, "[]") {
			key := strings.TrimSuffix(field, "[]")
			values := stringSliceField(payload, key)
			if len(values) == 0 {
				continue
			}
			canonical := make([]string, 0, len(values))
			for _, value := range values {
				canonical = append(canonical, server.canonicalPortReference(ctx, value))
			}
			payload[key] = canonical
			continue
		}
		value := strings.TrimSpace(stringField(payload, field))
		if value == "" {
			continue
		}
		payload[field] = server.canonicalPortReference(ctx, value)
	}
}

// canonicalPortReference resolves a product id, kernel netdev, logical name,
// MAC or PCI address to the product id. Unresolvable values are returned
// unchanged so a WAN line id in the same field keeps working.
func (server *Server) canonicalPortReference(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if id := server.canonicalInterfaceReference(ctx, value); isProductInterfaceID(id) {
		return id
	}
	return value
}

// normalizeDesiredPortReferences canonicalizes a read result. It must not call
// canonicalInterfaceReference: that helper reads desired state, and this runs
// inside desiredItems.
func (server *Server) normalizeDesiredPortReferences(ctx context.Context, resourceType string, items []map[string]any) []map[string]any {
	switch resourceType {
	case "interface":
		return server.canonicalizeInterfaceDesiredItems(ctx, items)
	case "wan_link":
		return server.canonicalizeWANLinkDesiredItems(ctx, items)
	}
	fields, ok := portReferenceFields[resourceType]
	if !ok || len(items) == 0 {
		return items
	}
	for _, item := range items {
		for _, field := range fields {
			list := strings.HasSuffix(field, "[]")
			key := strings.TrimSuffix(field, "[]")
			if list {
				values := stringSliceField(item, key)
				if len(values) == 0 {
					continue
				}
				canonical := make([]string, 0, len(values))
				for _, value := range values {
					canonical = append(canonical, server.canonicalStoredPortReference(ctx, value))
				}
				item[key] = canonical
				continue
			}
			value := strings.TrimSpace(stringField(item, key))
			if value == "" {
				continue
			}
			item[key] = server.canonicalStoredPortReference(ctx, value)
		}
	}
	return items
}

// canonicalStoredPortReference is the read-side resolver: identities only, no
// desired-state lookup. A value that is already a product id stays as it is and
// an unknown token (a WAN line id) is returned unchanged.
func (server *Server) canonicalStoredPortReference(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if isProductInterfaceID(value) {
		return value
	}
	if id := server.canonicalInterfaceID(ctx, value); isProductInterfaceID(id) {
		return id
	}
	return value
}

// canonicalizeInterfaceDesiredItems makes every stored interface document speak
// the product id. A document written before the migration (keyed `lab30`, with
// the kernel name in `id`/`interface_id`) is rewritten in place; when both
// spellings exist the richer document wins so a migration bug cannot drop a LAN
// address.
func (server *Server) canonicalizeInterfaceDesiredItems(ctx context.Context, items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return items
	}
	result := make([]map[string]any, 0, len(items))
	seen := map[string]int{}
	for _, item := range items {
		clone := cloneObject(item)
		canonical := server.canonicalInterfaceID(ctx, firstNonEmpty(
			stringField(clone, "id"), stringField(clone, "interface_id"), stringField(clone, "system_name"), stringField(clone, "name")))
		if !isProductInterfaceID(canonical) {
			canonical = server.canonicalInterfaceID(ctx, firstNonEmpty(
				stringField(clone, "system_name"), stringField(clone, "interface_id"), stringField(clone, "name")))
		}
		if isProductInterfaceID(canonical) {
			clone["id"] = canonical
			clone["interface_id"] = canonical
			clone["name"] = canonical
			if linux := server.interfaceLinuxName(ctx, canonical); linux != "" {
				clone["system_name"] = linux
			}
		}
		key := stringField(clone, "id")
		if key == "" {
			result = append(result, clone)
			continue
		}
		if index, ok := seen[key]; ok {
			if desiredDocumentWeight(clone) > desiredDocumentWeight(result[index]) {
				result[index] = clone
			}
			continue
		}
		seen[key] = len(result)
		result = append(result, clone)
	}
	return result
}

// canonicalizeWANLinkDesiredItems keeps a WAN line's port reference on the
// product id while the kernel name stays available as the internal
// `system_name` attribute.
func (server *Server) canonicalizeWANLinkDesiredItems(ctx context.Context, items []map[string]any) []map[string]any {
	for _, item := range items {
		reference := firstNonEmpty(stringField(item, "interface_id"), stringField(item, "system_name"), stringField(item, "id"))
		canonical := server.canonicalStoredPortReference(ctx, reference)
		if !isProductInterfaceID(canonical) {
			canonical = server.canonicalStoredPortReference(ctx, firstNonEmpty(stringField(item, "system_name"), stringField(item, "interface_id")))
		}
		if !isProductInterfaceID(canonical) {
			continue
		}
		item["interface_id"] = canonical
		if linux := server.interfaceLinuxName(ctx, canonical); linux != "" {
			item["system_name"] = linux
		}
		if strings.TrimSpace(stringField(item, "display_name")) == "" {
			item["display_name"] = canonical
		}
	}
	return items
}

func desiredDocumentWeight(item map[string]any) int {
	weight := 0
	for key, value := range item {
		switch key {
		case "runtime_state", "capabilities", "degraded", "degraded_reason", "updated_at":
			continue
		}
		switch typed := value.(type) {
		case nil:
		case string:
			if strings.TrimSpace(typed) != "" {
				weight++
			}
		case bool:
			weight++
		case map[string]any:
			if len(typed) > 0 {
				weight++
			}
		case []any:
			if len(typed) > 0 {
				weight++
			}
		case []string:
			if len(typed) > 0 {
				weight++
			}
		default:
			weight++
		}
	}
	return weight
}

// deleteStaleInterfaceDocuments removes interface documents that name the same
// physical port under another key. Without it a PATCH that arrives as `eth2`
// while the stored row is `lab30` would leave two rows for one port and the API
// would list the port twice.
func (server *Server) staleInterfaceDocumentKeys(ctx context.Context, canonicalID string) []persistence.ConfigKey {
	if server.store == nil || !isProductInterfaceID(canonicalID) {
		return nil
	}
	documents, err := server.store.Configs(ctx, "interface")
	if err != nil {
		return nil
	}
	keys := make([]persistence.ConfigKey, 0)
	for _, document := range documents {
		if strings.EqualFold(document.ResourceID, canonicalID) {
			continue
		}
		// A document can be stale under an old product id while its payload
		// still names the same kernel netdev. Compare every identity field so
		// a NIC renumbering cannot leave two desired rows for one port.
		var item map[string]any
		_ = json.Unmarshal(document.Payload, &item)
		samePort := false
		for _, value := range []string{
			document.ResourceID,
			stringField(item, "id"),
			stringField(item, "interface_id"),
			stringField(item, "system_name"),
			stringField(item, "name"),
		} {
			if strings.TrimSpace(value) == "" {
				continue
			}
			if strings.EqualFold(server.canonicalInterfaceID(ctx, value), canonicalID) {
				samePort = true
				break
			}
		}
		if !samePort {
			continue
		}
		keys = append(keys, persistence.ConfigKey{ResourceType: "interface", ResourceID: document.ResourceID})
	}
	return keys
}

func (server *Server) deleteStaleInterfaceDocuments(ctx context.Context, canonicalID string) {
	for _, key := range server.staleInterfaceDocumentKeys(ctx, canonicalID) {
		if err := server.store.DeleteConfig(ctx, key.ResourceType, key.ResourceID); err != nil {
			continue
		}
		log.Printf("interface %s: removed stale desired document keyed %q", canonicalID, key.ResourceID)
	}
}

// portReferencePattern matches the spellings that can only mean a physical
// port: a product id, a kernel netdev name, a MAC address or a PCI address.
// A logical assignment id (`wan0`, `lan-bridge-1`) does not match, so the
// interface resource still accepts those documents.
var portReferencePattern = regexp.MustCompile(`(?i)^(eth[0-9]+|(ens|enp|eno|enx|ems|em|en|lab|bond|br|vlan|tap)[0-9][0-9a-z_.:-]*|[0-9a-f]{2}(:[0-9a-f]{2}){5}|[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7])$`)

// interfaceWriteReferenceProblem returns an operator-facing reason when an
// interface write clearly names a port this device does not have.
func (server *Server) interfaceWriteReferenceProblem(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "an interface is required"
	}
	if server.portIDExists(ctx, server.canonicalInterfaceReference(ctx, value)) {
		return ""
	}
	if !portReferencePattern.MatchString(value) {
		return ""
	}
	return fmt.Sprintf("interface %q is not a port on this device; pick one of the ids from the interface list", value)
}
