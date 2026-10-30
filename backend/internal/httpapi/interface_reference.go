package httpapi

import (
	"context"
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/vpp"
)

// Interface references in desired state.
//
// Operators see three names for the same port: the product id (`eth3`), the
// kernel netdev (`ens224`) and the logical name they gave it (`LAN10`). Every
// resource that points at a port must accept all of them and store one
// canonical value, otherwise a later lookup silently misses and the whole plan
// fails to compile.
//
// The canonical value is the product id: it is derived from the PCI address, so
// it survives a netdev rename (udev, driver rebind) and it is what
// /api/v1/interfaces exposes as `id`.

// canonicalInterfaceReference resolves a product id, kernel netdev name, logical
// display name, MAC or PCI address to the product interface id. Unresolvable
// values are returned unchanged so validation can reject them with a message
// naming what the operator typed.
func (server *Server) canonicalInterfaceReference(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if id := server.canonicalInterfaceID(ctx, value); isProductInterfaceID(id) {
		return id
	}
	// Logical names live on the interface desired documents (display_name) and
	// on the inventory aliases, not in the identity service.
	if items, err := server.desiredItems(ctx, "interface"); err == nil {
		for _, item := range items {
			candidates := []string{
				stringField(item, "display_name"),
				stringField(item, "name"),
				stringField(item, "alias"),
				stringField(item, "id"),
				stringField(item, "interface_id"),
				stringField(item, "system_name"),
			}
			matched := false
			for _, candidate := range candidates {
				if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), value) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			resolved := server.resolveInterfaceID(ctx, firstNonEmpty(
				stringField(item, "interface_id"), stringField(item, "id"), stringField(item, "system_name")))
			if resolved == "" {
				continue
			}
			if id := server.canonicalInterfaceID(ctx, resolved); isProductInterfaceID(id) {
				return id
			}
		}
	}
	for _, item := range server.interfaceAliasSnapshot(ctx) {
		actual := strings.TrimSpace(firstNonEmpty(stringField(item, "system_name"), stringField(item, "id"), stringField(item, "name")))
		if actual == "" {
			continue
		}
		if strings.EqualFold(actual, value) || strings.EqualFold(strings.TrimSpace(stringField(item, "display_name")), value) {
			if id := server.canonicalInterfaceID(ctx, actual); isProductInterfaceID(id) {
				return id
			}
		}
	}
	return value
}

// interfaceReferenceProblem returns an operator-facing reason when a reference
// cannot be resolved to a port this device has. Empty means "acceptable".
func (server *Server) interfaceReferenceProblem(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "an interface is required"
	}
	if !server.portIDExists(ctx, server.canonicalInterfaceReference(ctx, value)) {
		return fmt.Sprintf("interface %q is not a port on this device; pick one of the ids from the interface list", value)
	}
	return ""
}

// dhcpInterfaceProblem explains why a DHCP service cannot be rendered right now.
// It is used for the skip warning, not to reject the write: a port can be
// temporarily not-a-LAN while its role is being changed.
func (server *Server) dhcpInterfaceProblem(ctx context.Context, interfaceID string, assignments []vpp.AddressAssignment) string {
	canonical := server.canonicalInterfaceReference(ctx, interfaceID)
	if !isProductInterfaceID(canonical) {
		return fmt.Sprintf("interface %q is not a port on this device", interfaceID)
	}
	linux := server.resolveInterfaceID(ctx, canonical)
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(assignment.LinuxInterface), linux) {
			return "the LAN has no usable address yet"
		}
	}
	return fmt.Sprintf("interface %s is not a configured LAN (role or address missing)", server.displayInterfaceName(ctx, linux))
}

// portIDExists reports whether a product id is one this device actually has.
// Shape alone is not enough: `eth9` looks like a product id but is not a port,
// and accepting it persisted a document no dataplane could ever apply.
func (server *Server) portIDExists(ctx context.Context, id string) bool {
	id = strings.TrimSpace(id)
	if !isProductInterfaceID(id) {
		return false
	}
	for _, identity := range server.interfaceIdentities(ctx) {
		if strings.EqualFold(identity.ID, id) {
			return true
		}
	}
	return false
}
