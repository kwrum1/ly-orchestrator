package httpapi

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/vpp"
)

// proxySNATSource records why an address was selected. It is kept explicit so
// the runtime plan and the operator-facing warning can distinguish the normal
// interface-address handoff from an explicitly reserved spare address.
type proxySNATSource string

const (
	// proxySNATSourceWANInterface is the preferred handoff: the proxy egress
	// uses the WAN interface's own address. It needs no additional address and
	// therefore cannot collide with a LAN host's ARP ownership.
	proxySNATSourceWANInterface proxySNATSource = "wan_interface"
	// proxySNATSourceReservedPool is used only when the operator explicitly
	// reserved spare addresses for proxy handoffs.
	proxySNATSourceReservedPool proxySNATSource = "reserved_pool"
	// proxySNATSourceLegacyGlobal keeps the legacy global-FIB handoff for
	// dynamic/PPPoE/WAN-group underlays that have no single static subnet.
	proxySNATSourceLegacyGlobal proxySNATSource = "legacy_global_handoff"
)

type proxySNATAllocation struct {
	Address      string
	Source       proxySNATSource
	WANInterface string
	WANPrefix    string
}

// proxySNATError explains why a proxy egress cannot own a usable SNAT outside
// address. The allocator never falls back to a blind subnet hash; an
// unsatisfiable policy is a planning error with an actionable reason.
type proxySNATError struct {
	EgressID     string
	WANInterface string
	WANPrefix    string
	Reason       string
}

func (failure *proxySNATError) Error() string {
	if failure == nil {
		return ""
	}
	reason := strings.TrimSpace(failure.Reason)
	if reason == "" {
		reason = "no explicit SNAT address is available"
	}
	wan := strings.TrimSpace(failure.WANInterface)
	if wan == "" {
		wan = "<unresolved>"
	}
	prefix := strings.TrimSpace(failure.WANPrefix)
	if prefix != "" {
		return fmt.Sprintf("proxy egress %q cannot allocate a SNAT outside address on WAN %q (%s): %s", failure.EgressID, wan, prefix, reason)
	}
	return fmt.Sprintf("proxy egress %q cannot allocate a SNAT outside address on WAN %q: %s", failure.EgressID, wan, reason)
}

type proxySNATRequest struct {
	EgressID string
	// Assignments is the effective VPP address inventory. The matching WAN
	// assignment supplies both the interface address and the usable subnet.
	Assignments []vpp.AddressAssignment
	// UnderlayRoute is the resolved route used by the proxy handoff, for
	// example "10.1.18.1 lyroute-enp4s0".
	UnderlayRoute string
	// ReservedAddresses is the operator's explicit SNAT pool. Empty means the
	// preferred interface-address handoff. Addresses are considered in the
	// operator's order, never hashed.
	ReservedAddresses []string
	// ExcludedAddresses are addresses already claimed by other proxy egresses
	// or by a caller's ARP/ND observation. The allocator itself never probes
	// the network; these inputs must come from planning or an explicit probe.
	ExcludedAddresses []string
	StaticMappings    []nat.StaticMapping
	PortMappings      []nat.PortMapping
}

// allocateProxySNATAddress selects the outside address for one proxy egress.
//
// Priority:
//  1. the WAN interface's own address (when no explicit pool is configured);
//  2. the first usable address from the operator's explicit reserved pool;
//  3. the legacy global-FIB handoff for dynamic/PPPoE/WAN-group underlays.
//
// The allocator never scans the whole WAN subnet. It also never silently
// changes an explicit policy: conflicts and exhaustion return
// *proxySNATError with the exact reason.
func allocateProxySNATAddress(request proxySNATRequest) (proxySNATAllocation, error) {
	underlay, err := parseProxyUnderlayRoute(request.UnderlayRoute)
	if err != nil {
		return proxySNATAllocation{}, &proxySNATError{
			EgressID:     request.EgressID,
			WANInterface: proxySNATUnderlayInterface(request.UnderlayRoute),
			Reason:       err.Error(),
		}
	}
	if !underlay.Explicit || underlay.Dynamic {
		return legacyProxySNATAllocation(request)
	}
	interfaceName := underlay.Interface

	assignment, found := proxySNATWANAssignment(request.Assignments, interfaceName)
	if !found {
		return legacyProxySNATAllocation(request)
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
	if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() {
		return proxySNATAllocation{}, &proxySNATError{
			EgressID:     request.EgressID,
			WANInterface: firstNonEmptyProxySNAT(assignment.VPPInterface, interfaceName),
			WANPrefix:    strings.TrimSpace(assignment.CIDR),
			Reason:       "the WAN assignment is not a static IPv4 prefix, so neither the interface address nor an explicit SNAT pool can be validated",
		}
	}
	interfaceAddress := prefix.Addr()
	prefix = prefix.Masked()
	allocation := proxySNATAllocation{
		Source:       proxySNATSourceWANInterface,
		WANInterface: firstNonEmptyProxySNAT(assignment.VPPInterface, interfaceName),
		WANPrefix:    prefix.String(),
	}

	reserved := proxySNATReservedAddresses(request, prefix)
	if len(request.ReservedAddresses) == 0 {
		// The WAN interface address is the preferred SNAT address. It is in
		// the reserved map because it belongs to this gateway, so it is
		// deliberately returned here rather than treated as a conflict.
		allocation.Address = interfaceAddress.String()
		return allocation, nil
	}

	if prefix.Bits() > 30 {
		return proxySNATAllocation{}, &proxySNATError{
			EgressID:     request.EgressID,
			WANInterface: allocation.WANInterface,
			WANPrefix:    prefix.String(),
			Reason:       "the WAN prefix has no usable spare host range for an explicit SNAT pool",
		}
	}

	conflicts := make([]string, 0)
	seenPool := make(map[netip.Addr]struct{}, len(request.ReservedAddresses))
	for _, raw := range request.ReservedAddresses {
		candidate, parseErr := netip.ParseAddr(strings.TrimSpace(raw))
		if parseErr != nil || !candidate.Is4() {
			return proxySNATAllocation{}, &proxySNATError{
				EgressID:     request.EgressID,
				WANInterface: allocation.WANInterface,
				WANPrefix:    prefix.String(),
				Reason:       fmt.Sprintf("snat_addresses entry %q is not an IPv4 address", strings.TrimSpace(raw)),
			}
		}
		if _, duplicate := seenPool[candidate]; duplicate {
			return proxySNATAllocation{}, &proxySNATError{
				EgressID:     request.EgressID,
				WANInterface: allocation.WANInterface,
				WANPrefix:    prefix.String(),
				Reason:       fmt.Sprintf("snat_addresses entry %s is listed more than once", candidate),
			}
		}
		seenPool[candidate] = struct{}{}
		if !prefix.Contains(candidate) {
			return proxySNATAllocation{}, &proxySNATError{
				EgressID:     request.EgressID,
				WANInterface: allocation.WANInterface,
				WANPrefix:    prefix.String(),
				Reason:       fmt.Sprintf("snat_addresses entry %s is outside %s", candidate, prefix),
			}
		}
		if candidate == interfaceAddress {
			conflicts = append(conflicts, fmt.Sprintf("%s is the WAN interface address; leave snat_addresses empty to use it", candidate))
			continue
		}
		if _, taken := reserved[candidate]; taken {
			conflicts = append(conflicts, fmt.Sprintf("%s is already owned by the gateway, a static mapping or another proxy egress", candidate))
			continue
		}
		// The pool is an allow-list, not a hint: return the first usable entry
		// exactly as the operator ordered it.
		allocation.Address = candidate.String()
		allocation.Source = proxySNATSourceReservedPool
		return allocation, nil
	}

	reason := "every explicitly reserved SNAT address is unavailable"
	if len(conflicts) > 0 {
		reason += ": " + strings.Join(conflicts, "; ")
	}
	return proxySNATAllocation{}, &proxySNATError{
		EgressID:     request.EgressID,
		WANInterface: allocation.WANInterface,
		WANPrefix:    prefix.String(),
		Reason:       reason,
	}
}

func legacyProxySNATAllocation(request proxySNATRequest) (proxySNATAllocation, error) {
	if len(request.ReservedAddresses) > 0 {
		return proxySNATAllocation{}, &proxySNATError{
			EgressID:     request.EgressID,
			WANInterface: proxySNATUnderlayInterface(request.UnderlayRoute),
			Reason:       "snat_addresses requires a static IPv4 WAN assignment; dynamic, PPPoE and WAN-group underlays keep the legacy global-FIB handoff",
		}
	}
	return proxySNATAllocation{Source: proxySNATSourceLegacyGlobal, WANInterface: proxySNATUnderlayInterface(request.UnderlayRoute)}, nil
}

// proxyUnderlayRouteInfo is a structured view of the underlay string. The
// string is not a stable grammar across call sites, so it is parsed explicitly
// instead of assuming the last token is an interface name. An unrecognized
// static route is an error; only the documented dynamic/PPPoE/WAN-group forms
// may fall back to the legacy global-FIB handoff.
type proxyUnderlayRouteInfo struct {
	Gateway   string
	Interface string
	Table     int
	Dynamic   bool
	Explicit  bool
}

func parseProxyUnderlayRoute(raw string) (proxyUnderlayRouteInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "local") {
		return proxyUnderlayRouteInfo{}, nil
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return proxyUnderlayRouteInfo{}, nil
	}
	if fields[0] == "ip4-lookup-in-table" {
		return proxyUnderlayRouteInfo{Dynamic: true, Explicit: true}, nil
	}
	for _, field := range fields {
		lower := strings.ToLower(strings.TrimSpace(field))
		if strings.HasPrefix(lower, "pppoe") || strings.HasPrefix(lower, "pppox") || strings.HasPrefix(lower, "lyppp-") {
			return proxyUnderlayRouteInfo{Dynamic: true, Explicit: true}, nil
		}
	}

	info := proxyUnderlayRouteInfo{Explicit: true}
	candidates := make([]string, 0, 2)
	setInterface := func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("underlay route %q names an empty interface", raw)
		}
		if info.Interface != "" && info.Interface != value {
			return fmt.Errorf("underlay route %q names multiple interfaces (%s, %s)", raw, info.Interface, value)
		}
		info.Interface = value
		return nil
	}
	setGateway := func(value string) error {
		value = strings.TrimSpace(value)
		if info.Gateway != "" && info.Gateway != value {
			return fmt.Errorf("underlay route %q names multiple gateways (%s, %s)", raw, info.Gateway, value)
		}
		info.Gateway = value
		return nil
	}
	skipValue := func(index *int, keyword string) error {
		if *index+1 >= len(fields) {
			return fmt.Errorf("underlay route %q has %q without a value", raw, keyword)
		}
		*index = *index + 1
		return nil
	}
	for index := 0; index < len(fields); index++ {
		token := strings.TrimSpace(fields[index])
		switch strings.ToLower(token) {
		case "via":
			if index+1 >= len(fields) {
				return info, fmt.Errorf("underlay route %q has via without an interface or gateway", raw)
			}
			next := strings.TrimSpace(fields[index+1])
			index++
			if address, err := netip.ParseAddr(next); err == nil && address.Is4() {
				if err := setGateway(next); err != nil {
					return info, err
				}
				continue
			}
			if err := setInterface(next); err != nil {
				return info, err
			}
		case "dev":
			if index+1 >= len(fields) {
				return info, fmt.Errorf("underlay route %q has dev without an interface", raw)
			}
			index++
			if err := setInterface(fields[index]); err != nil {
				return info, err
			}
		case "table":
			if index+1 >= len(fields) {
				return info, fmt.Errorf("underlay route %q has table without a number", raw)
			}
			index++
			table, err := strconv.Atoi(strings.TrimSpace(fields[index]))
			if err != nil || table < 0 {
				return info, fmt.Errorf("underlay route %q has invalid table %q", raw, fields[index])
			}
			info.Table = table
		case "scope", "metric", "src", "proto", "preference", "onlink":
			if strings.EqualFold(token, "onlink") {
				continue
			}
			if err := skipValue(&index, token); err != nil {
				return info, err
			}
		default:
			if address, err := netip.ParseAddr(token); err == nil && address.Is4() {
				if err := setGateway(token); err != nil {
					return info, err
				}
				continue
			}
			if strings.HasPrefix(token, "-") {
				return info, fmt.Errorf("underlay route %q contains option %q", raw, token)
			}
			candidates = append(candidates, token)
		}
	}
	if info.Interface == "" {
		switch len(candidates) {
		case 0:
			return info, fmt.Errorf("underlay route %q does not name an interface; pass the WAN VPP interface or use a supported dynamic/PPPoE form", raw)
		case 1:
			info.Interface = candidates[0]
		default:
			return info, fmt.Errorf("underlay route %q is ambiguous; unexpected tokens %s", raw, strings.Join(candidates, ", "))
		}
	} else if len(candidates) > 0 {
		return info, fmt.Errorf("underlay route %q has unexpected tokens %s", raw, strings.Join(candidates, ", "))
	}
	if !vppInterfaceNameSafe(info.Interface) {
		return info, fmt.Errorf("underlay route %q has invalid interface %q", raw, info.Interface)
	}
	return info, nil
}

func proxySNATUnderlayInterface(underlayRoute string) string {
	if parsed, err := parseProxyUnderlayRoute(underlayRoute); err == nil && parsed.Interface != "" {
		return parsed.Interface
	}
	fields := strings.Fields(strings.TrimSpace(underlayRoute))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// proxySNATWANAssignment finds the static WAN assignment that owns the
// underlay interface. A single static WAN is accepted as a fallback because a
// logical underlay name can differ from the VPP interface name.
func proxySNATWANAssignment(assignments []vpp.AddressAssignment, interfaceName string) (vpp.AddressAssignment, bool) {
	interfaceName = strings.TrimSpace(interfaceName)
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			continue
		}
		if strings.TrimSpace(assignment.VPPInterface) == interfaceName {
			return assignment, true
		}
	}
	var candidates []vpp.AddressAssignment
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if err == nil && prefix.Addr().Is4() && !prefix.Addr().IsUnspecified() {
			candidates = append(candidates, assignment)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return vpp.AddressAssignment{}, false
}

// proxySNATReservedAddresses is the complete ownership set. It intentionally
// includes every local interface address, the network/broadcast endpoints,
// the underlay gateway, every static/port mapping and addresses claimed by
// other proxy egresses. ARP probing, when available, only adds to this set; it
// never replaces planning.
func proxySNATReservedAddresses(request proxySNATRequest, prefix netip.Prefix) map[netip.Addr]struct{} {
	reserved := map[netip.Addr]struct{}{}
	if prefix.IsValid() {
		reserved[prefix.Addr()] = struct{}{}
		if broadcast := prefixBroadcast(prefix); broadcast.IsValid() {
			reserved[broadcast] = struct{}{}
		}
	}
	for _, assignment := range request.Assignments {
		if parsed, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR)); err == nil && parsed.Addr().Is4() {
			reserved[parsed.Addr()] = struct{}{}
		}
	}
	for _, address := range strings.Fields(request.UnderlayRoute) {
		if parsed, err := netip.ParseAddr(address); err == nil && parsed.Is4() {
			reserved[parsed] = struct{}{}
		}
	}
	for _, mapping := range request.StaticMappings {
		if parsed, err := netip.ParseAddr(strings.TrimSpace(mapping.ExternalAddress)); err == nil && parsed.Is4() {
			reserved[parsed] = struct{}{}
		}
	}
	for _, mapping := range request.PortMappings {
		if parsed, err := netip.ParseAddr(strings.TrimSpace(mapping.ExternalAddress)); err == nil && parsed.Is4() {
			reserved[parsed] = struct{}{}
		}
	}
	for _, raw := range request.ExcludedAddresses {
		if parsed, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil && parsed.Is4() {
			reserved[parsed] = struct{}{}
		}
	}
	return reserved
}

func firstNonEmptyProxySNAT(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// normalizeProxySNATAddresses trims an API-authored pool while preserving its
// order. Duplicates are left in place so ValidateEgress can reject them with a
// precise error instead of silently changing the operator's policy.
func normalizeProxySNATAddresses(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		normalized = append(normalized, value)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func validateProxySNATAddressList(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		address, err := netip.ParseAddr(value)
		if err != nil || !address.Is4() {
			return fmt.Errorf("snat_addresses entry %q is not an IPv4 address", value)
		}
		if _, duplicate := seen[address.String()]; duplicate {
			return fmt.Errorf("snat_addresses entry %q is duplicated", address.String())
		}
		seen[address.String()] = struct{}{}
	}
	return nil
}

// validateProxyEgressDefaultSNAT rejects a save that would rely on the WAN
// interface address as the proxy SNAT outside address.
//
// VPP keeps a WAN interface address in VRF 0. The proxy handoff captures into
// a tenant VRF, and NAT44 pools are tenant-scoped: adding the same address a
// second time fails with "NAT address already in use". The dataplane script
// therefore fails the whole apply, leaving the operator with a generation that
// cannot converge. Rejecting the save keeps the previous generation intact and
// turns an opaque apply failure into an actionable field error.
//
// Dynamic/PPPoE/WAN-group underlays keep the documented legacy global-FIB
// handoff and remain allowed without snat_addresses.
func (server *Server) validateProxyEgressDefaultSNAT(ctx context.Context, payload map[string]any) error {
	if truthy(payload["deleted"]) || !desiredEnabled(payload) {
		return nil
	}
	// An unbound draft is not compiled into a proxy generation. Blocking it
	// would make the UI's "create the line first, bind a node later" flow
	// impossible without inventing an address the operator has not chosen.
	if strings.TrimSpace(firstNonEmpty(
		stringField(payload, "node_id"),
		stringField(payload, "subscription_id"),
		stringField(payload, "group_id"),
	)) == "" {
		return nil
	}
	if len(normalizeProxySNATAddresses(stringSliceField(payload, "snat_addresses"))) > 0 {
		return nil
	}
	underlayID := strings.TrimSpace(firstNonEmpty(stringField(payload, "underlay_wan_id"), stringField(payload, "underlay_wan")))
	if underlayID == "" {
		return nil
	}
	// A WAN group has no single static prefix and uses the legacy global-FIB
	// handoff, exactly like a dynamic/PPPoE line.
	if _, exists, err := server.desiredItem(ctx, "wan_group", underlayID); err == nil && exists {
		return nil
	}
	item, exists, err := server.desiredItem(ctx, "wan_link", underlayID)
	if err != nil || !exists || truthy(item["deleted"]) || !desiredEnabled(item) {
		return nil
	}
	// wanStaticCIDR is empty for dhcp/pppoe/proxy WAN lines.
	cidr := wanStaticCIDR(item)
	if cidr == "" {
		return nil
	}
	linuxInterface := server.resolveInterfaceID(ctx, firstNonEmpty(stringField(item, "interface_id"), stringField(item, "id"), stringField(item, "name")))
	if linuxInterface == "" {
		return nil
	}
	underlayRoute := vppInterfaceName(linuxInterface)
	if gateway := strings.TrimSpace(stringField(item, "gateway")); gateway != "" {
		underlayRoute = gateway + " " + underlayRoute
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		return err
	}
	allocation, err := allocateProxySNATAddress(proxySNATRequest{
		EgressID:      stringField(payload, "id"),
		Assignments:   assignments,
		UnderlayRoute: underlayRoute,
	})
	if err != nil {
		// Only the confirmed VPP tenant-VRF constraint is promoted to a save
		// error. Other allocator problems keep their existing apply-time
		// reporting so this change stays narrowly scoped.
		return nil
	}
	if allocation.Source != proxySNATSourceWANInterface {
		return nil
	}
	return &desiredValidationError{
		Code:    "proxy_snat_address_required",
		Message: proxyDefaultSNATMessage(stringField(payload, "id"), allocation),
	}
}

func proxyDefaultSNATMessage(egressID string, allocation proxySNATAllocation) string {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(allocation.WANPrefix))
	example := "<spare IPv4 address on " + allocation.WANPrefix + ">"
	if err == nil && prefix.IsValid() && prefix.Addr().Is4() {
		candidate := prefixBroadcast(prefix).Prev()
		if candidate.String() == allocation.Address {
			candidate = candidate.Prev()
		}
		if prefix.Contains(candidate) && candidate != prefix.Addr() {
			example = candidate.String()
		}
	}
	return fmt.Sprintf(
		"proxy egress %q uses static WAN %s (%s); VPP keeps the WAN interface address %s in VRF 0 and refuses to add it to the proxy tenant VRF (NAT address already in use). Configure snat_addresses with a spare address on that WAN, for example: \"snat_addresses\": [%q]. Choose an address that is not leased by DHCP and is not used by static NAT, port mappings or another proxy egress.",
		egressID, allocation.WANInterface, allocation.WANPrefix, allocation.Address, example,
	)
}

func prefixBroadcast(prefix netip.Prefix) netip.Addr {
	address := prefix.Addr().As4()
	bits := prefix.Bits()
	value := binary.BigEndian.Uint32(address[:])
	value |= (uint32(1) << uint(32-bits)) - 1
	raw := [4]byte{}
	binary.BigEndian.PutUint32(raw[:], value)
	return netip.AddrFrom4(raw)
}
