package httpapi

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// This file is the single desired-state validation layer for the cross-resource
// rules that a per-document check cannot enforce:
//
//   - an interface role may not change while another desired object still
//     requires the old role (see interface_role_reference.go);
//   - operator-visible names are unique inside one resource type;
//   - interface/WAN/NAT addresses are syntactically valid and do not overlap
//     an existing subnet of the same direction.
//
// Every check runs before the store write, is read-only and idempotent, and
// returns an actionable error that the HTTP layer renders as
// 422 invalid_desired_resource.

// desiredResourceKind returns the operator-facing noun used in diagnostics.
func desiredResourceKind(resourceType string) string {
	switch resourceType {
	case "interface":
		return "LAN line"
	case "wan_link":
		return "WAN line"
	case "interface_bond":
		return "interface bond"
	case "port_map":
		return "port map"
	case "nat_static":
		return "static NAT mapping"
	case "proxy_egress":
		return "proxy egress"
	case "proxy_node":
		return "proxy node"
	case "proxy_group":
		return "proxy group"
	case "route_policy":
		return "route policy"
	case "wan_group":
		return "WAN group"
	case "dhcp_server":
		return "DHCP service"
	case "dhcp_static_binding":
		return "DHCP static binding"
	case "dns_upstream":
		return "DNS upstream"
	case "dns_policy":
		return "DNS policy"
	case "traffic_control":
		return "traffic control policy"
	default:
		return resourceType
	}
}

// desiredResourceDisplayName is the name an operator sees and types. Interface
// documents derive `name` from the port id, so the friendly label lives in
// display_name.
func desiredResourceDisplayName(resourceType string, item map[string]any) string {
	if resourceType == "interface" {
		if name := strings.TrimSpace(stringField(item, "display_name")); name != "" {
			return name
		}
	}
	return strings.TrimSpace(stringField(item, "name"))
}

// validateDesiredResourceNameUnique rejects a second live object of the same
// resource type that reuses an existing name. Names are scoped by resource
// type: a LAN line and a WAN line may share a label, but two LAN lines may not.
func (server *Server) validateDesiredResourceNameUnique(ctx context.Context, resourceType string, payload map[string]any) error {
	switch resourceType {
	case "interface", "wan_link", "port_map", "nat_static", "proxy_egress", "proxy_node", "proxy_group", "route_policy", "wan_group":
	default:
		return nil
	}
	name := desiredResourceDisplayName(resourceType, payload)
	if name == "" {
		return nil
	}
	if resourceType == "interface" {
		// Only LAN lines have an operator-assigned name. A WAN port document's
		// display_name is derived from the port id.
		role, _ := interfaceDesiredRole(payload)
		if role != "lan" {
			return nil
		}
	}
	items, err := server.desiredItems(ctx, resourceType)
	if err != nil {
		return err
	}
	id := stringField(payload, "id")
	port := ""
	if resourceType == "interface" {
		port = server.canonicalInterfaceReference(ctx, firstNonEmpty(stringField(payload, "interface_id"), id))
	}
	for _, item := range items {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		itemID := stringField(item, "id")
		if id != "" && strings.EqualFold(itemID, id) {
			continue
		}
		if resourceType == "interface" {
			role, _ := interfaceDesiredRole(item)
			if role != "lan" {
				continue
			}
			// Legacy duplicate documents that describe the same physical port
			// are collapsed by this same PATCH; they are not a second LAN line.
			if itemID != "" && (strings.EqualFold(itemID, port) || (port != "" && server.interfaceReferenceMatches(ctx, itemID, port))) {
				continue
			}
		}
		if strings.EqualFold(desiredResourceDisplayName(resourceType, item), name) {
			return fmt.Errorf("%s name %q already exists; choose a different name", desiredResourceKind(resourceType), name)
		}
	}
	return nil
}

// validateDesiredNameUniqueAgainst is used by save paths that build their
// payload from a request DTO instead of a map.
func (server *Server) validateDesiredNameUniqueAgainst(ctx context.Context, resourceType, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	items, err := server.desiredItems(ctx, resourceType)
	if err != nil {
		return err
	}
	for _, item := range items {
		if truthy(item["deleted"]) || !desiredEnabled(item) {
			continue
		}
		if id != "" && strings.EqualFold(stringField(item, "id"), id) {
			continue
		}
		if strings.EqualFold(desiredResourceDisplayName(resourceType, item), name) {
			return fmt.Errorf("%s name %q already exists; choose a different name", desiredResourceKind(resourceType), name)
		}
	}
	return nil
}

// validateDesiredAddressPayload rejects malformed or unusable addresses before
// they are persisted. Interface and WAN addresses also get a subnet-overlap
// check against existing documents of the same direction.
func (server *Server) validateDesiredAddressPayload(ctx context.Context, resourceType string, payload map[string]any) error {
	switch resourceType {
	case "interface", "wan_link":
		prefixes, err := desiredLocalPrefixes(resourceType, payload)
		if err != nil {
			return err
		}
		if len(prefixes) == 0 {
			return nil
		}
		role := "wan"
		if resourceType == "interface" {
			role, _ = interfaceDesiredRole(payload)
		}
		if role != "lan" && role != "wan" {
			return nil
		}
		return server.validateDesiredSubnetOverlap(ctx, resourceType, payload, role, prefixes)
	case "nat_static":
		for _, field := range []string{"external_address", "internal_address", "wan_next_hop"} {
			if err := validateDesiredHostField("static NAT "+field, stringField(payload, field)); err != nil {
				return err
			}
		}
	case "port_map":
		for _, field := range []string{"external_address", "internal_host", "wan_next_hop"} {
			if err := validateDesiredHostField("port map "+field, stringField(payload, field)); err != nil {
				return err
			}
		}
	}
	return nil
}

// desiredLocalPrefixes extracts every address that a document configures on a
// local interface. Bare addresses are treated as host prefixes.
func desiredLocalPrefixes(resourceType string, payload map[string]any) ([]netip.Prefix, error) {
	type field struct {
		label string
		raw   string
	}
	fields := make([]field, 0, 8)
	keys := []string{"cidr", "ip_cidr", "address", "ip"}
	if resourceType == "wan_link" {
		keys = []string{"cidr", "ip_cidr", "address"}
	}
	for _, key := range keys {
		if raw := strings.TrimSpace(stringField(payload, key)); raw != "" {
			fields = append(fields, field{label: key, raw: raw})
		}
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		nested, ok := payload[family].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"address", "cidr"} {
			if raw := strings.TrimSpace(stringField(nested, key)); raw != "" {
				fields = append(fields, field{label: family + "." + key, raw: raw})
			}
		}
	}
	result := make([]netip.Prefix, 0, len(fields))
	seen := map[string]struct{}{}
	for _, entry := range fields {
		prefix, err := parseDesiredPrefix(entry.label, entry.raw)
		if err != nil {
			return nil, err
		}
		if err := validateDesiredPrefixUsable(entry.label, prefix); err != nil {
			return nil, err
		}
		key := prefix.Masked().String()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, prefix)
	}
	return result, nil
}

// parseDesiredPrefix validates an address or CIDR and reports the exact
// problem: invalid address, invalid prefix length, or an unusable subnet.
func parseDesiredPrefix(label, raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Prefix{}, fmt.Errorf("%s address is empty", label)
	}
	host, bitsText, hasPrefix := strings.Cut(raw, "/")
	address, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s %q is not a valid IPv4/IPv6 address", label, raw)
	}
	if !hasPrefix {
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	bits, err := strconv.Atoi(strings.TrimSpace(bitsText))
	if err != nil || bits < 0 || bits > address.BitLen() {
		return netip.Prefix{}, fmt.Errorf("%s %q has an invalid prefix length; %s allows 0-%d", label, raw, addressFamilyName(address), address.BitLen())
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s %q is not a valid IPv4/IPv6 network", label, raw)
	}
	return prefix, nil
}

func addressFamilyName(address netip.Addr) string {
	if address.Is4() {
		return "IPv4"
	}
	return "IPv6"
}

// validateDesiredPrefixUsable rejects default routes and other addresses that
// can never be a local interface subnet.
func validateDesiredPrefixUsable(label string, prefix netip.Prefix) error {
	address := prefix.Addr()
	if prefix.Bits() == 0 || address.IsUnspecified() {
		return fmt.Errorf("%s %s is not a usable subnet: 0.0.0.0/0 and ::/0 cannot be assigned to an interface", label, prefix)
	}
	if address.IsMulticast() {
		return fmt.Errorf("%s %s is not a usable subnet: multicast space cannot be assigned to an interface", label, prefix)
	}
	return nil
}

func validateDesiredHostField(label, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	prefix, err := parseDesiredPrefix(label, raw)
	if err != nil {
		return err
	}
	if strings.Contains(raw, "/") {
		return validateDesiredPrefixUsable(label, prefix)
	}
	address := prefix.Addr()
	if address.IsUnspecified() {
		return fmt.Errorf("%s %q is not a usable address", label, raw)
	}
	if address.IsMulticast() {
		return fmt.Errorf("%s %q must not be a multicast address", label, raw)
	}
	return nil
}

// validateDesiredSubnetOverlap rejects a new LAN/WAN subnet that overlaps an
// existing subnet in the same direction. Cross-direction overlap is left alone
// because the product intentionally supports a WAN address inside a local
// prefix (for example a lab WAN and LAN on one /24).
func (server *Server) validateDesiredSubnetOverlap(ctx context.Context, resourceType string, payload map[string]any, role string, candidates []netip.Prefix) error {
	id := stringField(payload, "id")
	port := ""
	if resourceType == "interface" {
		port = server.canonicalInterfaceReference(ctx, firstNonEmpty(stringField(payload, "interface_id"), id))
	}
	type existingPrefix struct {
		resourceType string
		label        string
		prefix       netip.Prefix
	}
	existing := make([]existingPrefix, 0, 8)
	addItem := func(itemType string, item map[string]any) {
		itemID := stringField(item, "id")
		if id != "" && strings.EqualFold(itemID, id) {
			return
		}
		// The document for the same physical port is this write's own data.
		if resourceType == "interface" && port != "" && itemID != "" && server.interfaceReferenceMatches(ctx, itemID, port) {
			return
		}
		prefixes, err := desiredLocalPrefixes(itemType, item)
		if err != nil {
			return
		}
		label := firstNonEmpty(desiredResourceDisplayName(itemType, item), itemID)
		for _, prefix := range prefixes {
			existing = append(existing, existingPrefix{resourceType: itemType, label: label, prefix: prefix})
		}
	}
	scan := func(itemType string, roleFilter string) error {
		items, err := server.desiredItems(ctx, itemType)
		if err != nil {
			return err
		}
		for _, item := range items {
			if truthy(item["deleted"]) || !desiredEnabled(item) {
				continue
			}
			if itemType == "interface" {
				itemRole, _ := interfaceDesiredRole(item)
				if !strings.EqualFold(itemRole, roleFilter) {
					continue
				}
			}
			addItem(itemType, item)
		}
		return nil
	}
	if role == "lan" {
		if err := scan("interface", "lan"); err != nil {
			return err
		}
	}
	if role == "wan" {
		if err := scan("wan_link", ""); err != nil {
			return err
		}
		if err := scan("interface", "wan"); err != nil {
			return err
		}
	}
	for _, candidate := range candidates {
		for _, have := range existing {
			if candidate.Overlaps(have.prefix) {
				return fmt.Errorf("%s subnet %s overlaps existing %s %s %s; choose an unused subnet",
					strings.ToUpper(role), candidate.Masked(), desiredResourceKind(have.resourceType), have.label, have.prefix.Masked())
			}
		}
	}
	return nil
}
