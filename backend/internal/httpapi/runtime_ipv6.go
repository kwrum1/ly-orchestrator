package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// ipv6LANPlan is the desired IPv6 view of one LAN that dnsmasq can serve.
// controlInterface is the Linux LCP TAP (lylan-*), never the VPP-owned
// interface, so RA and DHCPv6 broadcast/multicast reach the host daemon.
type ipv6LANPlan struct {
	controlInterface string
	item             map[string]any
	ipv6             map[string]any
	requested        bool
}

// dnsmasqIPv6LANPlans enriches the DHCPv4 plan list with one /64 per IPv6 LAN
// and adds an IPv6-only plan for LANs that have IPv6 enabled but no DHCPv4
// server. Prefix sources, in order of preference, are:
//
//  1. a live PPPoE DHCPv6-PD lease (/run/ly-route/pppoe/<wan>.json);
//  2. a configured delegated prefix on the source WAN;
//  3. a static WAN/LAN IPv6 address or prefix (the "same interface" case).
func (server *Server) dnsmasqIPv6LANPlans(ctx context.Context, plans []serviceRuntime.DnsmasqPlan, assignments []vpp.AddressAssignment) ([]serviceRuntime.DnsmasqPlan, []string, error) {
	lans, err := server.ipv6LANConfigs(ctx, assignments)
	if err != nil {
		return nil, nil, err
	}
	if len(lans) == 0 {
		return plans, nil, nil
	}
	wanPrefixes, dynamicWANPrefixes, err := server.ipv6WANPrefixes(ctx)
	if err != nil {
		return nil, nil, err
	}
	warnings := []string{}
	indexByPrefix := map[string]int{}
	enriched := make(map[string]serviceRuntime.DnsmasqPlan, len(lans))
	for _, lan := range lans {
		raw, source := lanIPv6Prefix(lan, wanPrefixes, dynamicWANPrefixes)
		if raw == "" {
			if lan.requested {
				warnings = append(warnings, fmt.Sprintf("IPv6 LAN %s has no delegated or static prefix yet", lan.controlInterface))
			}
			continue
		}
		normalized, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || !normalized.Addr().Is6() {
			warnings = append(warnings, fmt.Sprintf("IPv6 LAN %s prefix %q is invalid", lan.controlInterface, raw))
			continue
		}
		normalized = normalized.Masked()
		index := indexByPrefix[normalized.String()]
		prefix, err := serviceRuntime.DelegatedLANPrefix(normalized.String(), index)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("IPv6 LAN %s: %v", lan.controlInterface, err))
			continue
		}
		indexByPrefix[normalized.String()] = index + 1
		plan := serviceRuntime.DnsmasqPlan{
			IPv6Prefix:      prefix.String(),
			IPv6Mode:        lanIPv6AssignmentMode(lan.ipv6),
			IPv6NameServers: lanIPv6NameServers(lan.ipv6, prefix),
			RAInterval:      30,
		}
		// The VPP PPPoE dataplane keeps advertising itself as the LAN default
		// router while dnsmasq owns the prefix and DHCPv6. Advertising a
		// second default router on the LCP TAP would black-hole client traffic
		// because the Linux control plane is not the forwarding path. Static
		// IPv6 LANs have no VPP RA owner, so dnsmasq is the router there.
		lifetime := 1800
		if source == "wan-pd" {
			lifetime = 0
		}
		plan.RARouterLifetime = &lifetime
		enriched[lan.controlInterface] = plan
	}
	existing := map[string]bool{}
	for index := range plans {
		existing[plans[index].InterfaceID] = true
		if plan, ok := enriched[plans[index].InterfaceID]; ok {
			plans[index].IPv6Prefix = plan.IPv6Prefix
			plans[index].IPv6Mode = plan.IPv6Mode
			plans[index].IPv6NameServers = plan.IPv6NameServers
			plans[index].RAInterval = plan.RAInterval
			plans[index].RARouterLifetime = plan.RARouterLifetime
		}
	}
	// A LAN may enable IPv6 without configuring a DHCPv4 server. dnsmasq is
	// still the RA/DHCPv6 owner in that case, so add a dedicated plan.
	controls := make([]string, 0, len(enriched))
	for control := range enriched {
		if !existing[control] {
			controls = append(controls, control)
		}
	}
	sort.Strings(controls)
	usedIDs := map[string]bool{}
	for _, plan := range plans {
		usedIDs[plan.ID] = true
	}
	for _, control := range controls {
		id := ""
		for _, lan := range lans {
			if lan.controlInterface != control {
				continue
			}
			id = stringField(lan.item, "id")
			break
		}
		if id == "" || usedIDs[id] {
			id = "ipv6-" + control
		}
		if usedIDs[id] {
			continue
		}
		usedIDs[id] = true
		plan := enriched[control]
		plan.ID = id
		plan.InterfaceID = control
		plans = append(plans, plan)
	}
	return plans, warnings, nil
}

func (server *Server) ipv6LANConfigs(ctx context.Context, assignments []vpp.AddressAssignment) ([]ipv6LANPlan, error) {
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return nil, err
	}
	lans := []ipv6LANPlan{}
	for _, item := range items {
		ipv6, _ := item["ipv6"].(map[string]any)
		if len(ipv6) == 0 {
			continue
		}
		mode := strings.ToLower(strings.TrimSpace(stringField(ipv6, "mode")))
		if mode == "disabled" || mode == "none" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringField(item, "gateway_role"), stringField(item, "role"))))
		if role == "wan" || (role != "" && role != "lan" && role != "internal") {
			continue
		}
		interfaceID := server.resolveInterfaceID(ctx, firstNonEmpty(stringField(item, "interface_id"), stringField(item, "id")))
		if interfaceID == "" {
			continue
		}
		assignment, assigned := ipv6LANAssignment(assignments, item, interfaceID)
		if !assigned && hasLANAssignment(assignments) {
			// A desired interface that has no LAN assignment is not part of
			// the forwarding dataplane yet and must not receive an RA/DHCPv6
			// listener. WAN-only assignments do not suppress an otherwise
			// valid LAN plan.
			continue
		}
		controlInterface := ""
		if assigned {
			controlInterface = vpp.LANControlPlaneHostInterface(assignment.LinuxInterface)
		} else {
			controlInterface = vpp.LANControlPlaneHostInterface(interfaceID)
		}
		if controlInterface == "" {
			continue
		}
		requested := mode == "delegated_prefix" || mode == "slaac" || mode == "dhcpv6_pd" || truthy(ipv6["prefix_delegation"])
		lans = append(lans, ipv6LANPlan{
			controlInterface: controlInterface,
			item:             item,
			ipv6:             ipv6,
			requested:        requested,
		})
	}
	sort.Slice(lans, func(left, right int) bool {
		return lans[left].controlInterface < lans[right].controlInterface
	})
	return lans, nil
}

func hasLANAssignment(assignments []vpp.AddressAssignment) bool {
	for _, assignment := range assignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			return true
		}
	}
	return false
}

func ipv6LANAssignment(assignments []vpp.AddressAssignment, item map[string]any, interfaceID string) (vpp.AddressAssignment, bool) {
	itemID := strings.TrimSpace(stringField(item, "id"))
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		if assignment.ID != "" && itemID != "" && assignment.ID == itemID {
			return assignment, true
		}
		if assignment.LinuxInterface != "" && assignment.LinuxInterface == interfaceID {
			return assignment, true
		}
		if assignment.VPPInterface != "" && (assignment.VPPInterface == interfaceID || assignment.VPPInterface == "lyroute-"+interfaceID) {
			return assignment, true
		}
	}
	return vpp.AddressAssignment{}, false
}

func (server *Server) ipv6WANPrefixes(ctx context.Context) (map[string]string, map[string]bool, error) {
	items, err := server.desiredItems(ctx, "wan_link")
	if err != nil {
		return nil, nil, err
	}
	prefixes := map[string]string{}
	dynamic := map[string]bool{}
	for _, item := range items {
		id := strings.TrimSpace(stringField(item, "id"))
		if id == "" {
			continue
		}
		if prefix := livePPPoEDelegatedPrefix(id); prefix != "" {
			prefixes[id] = prefix
			dynamic[id] = true
			continue
		}
		ipv6, _ := item["ipv6"].(map[string]any)
		if raw := firstNonEmpty(
			stringField(ipv6, "delegated_prefix"),
			stringField(ipv6, "prefix"),
			stringField(item, "delegated_prefix"),
		); raw != "" {
			if prefix := normalizeIPv6Prefix(raw); prefix != "" {
				prefixes[id] = prefix
				dynamic[id] = true
			}
			continue
		}
		raw := firstNonEmpty(
			stringField(ipv6, "address"),
			stringField(ipv6, "cidr"),
			stringField(item, "ipv6_prefix"),
			stringField(item, "address"),
			stringField(item, "cidr"),
		)
		if prefix := normalizeIPv6Prefix(raw); prefix != "" {
			prefixes[id] = prefix
		}
	}
	return prefixes, dynamic, nil
}

func lanIPv6Prefix(lan ipv6LANPlan, wanPrefixes map[string]string, dynamicWANPrefixes map[string]bool) (string, string) {
	if prefix := normalizeIPv6Prefix(firstNonEmpty(stringField(lan.ipv6, "prefix"), stringField(lan.ipv6, "delegated_prefix"))); prefix != "" {
		return prefix, "lan-static"
	}
	address := firstNonEmpty(
		stringField(lan.ipv6, "address"),
		stringField(lan.ipv6, "cidr"),
		stringField(lan.item, "ipv6_prefix"),
	)
	if prefix := normalizeIPv6Prefix(address); prefix != "" {
		return prefix, "lan-static"
	}
	if !lan.requested {
		return "", ""
	}
	wanID := strings.TrimSpace(stringField(lan.ipv6, "source_wan_id"))
	if wanID == "" {
		return "", ""
	}
	if prefix := wanPrefixes[wanID]; prefix != "" {
		if dynamicWANPrefixes[wanID] {
			return prefix, "wan-pd"
		}
		return prefix, "wan-static"
	}
	return "", ""
}

func lanIPv6AssignmentMode(ipv6 map[string]any) string {
	for _, key := range []string{"dhcpv6", "address_mode", "assignment"} {
		if strings.EqualFold(strings.TrimSpace(stringField(ipv6, key)), "stateful") {
			return "stateful"
		}
	}
	return "slaac"
}

func lanIPv6NameServers(ipv6 map[string]any, prefix netip.Prefix) []string {
	configured := stringSliceField(ipv6, "dns_servers")
	servers := make([]string, 0, len(configured))
	for _, raw := range configured {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err == nil && address.Is6() {
			servers = append(servers, address.String())
		}
	}
	if len(servers) == 0 {
		servers = append(servers, serviceRuntime.IPv6GatewayAddress(prefix))
	}
	return servers
}

// normalizeIPv6Prefix accepts either a prefix or a host address and returns a
// masked IPv6 prefix. A bare host address is interpreted as a /64, which is
// what a static same-interface WAN/LAN assignment means in practice.
func normalizeIPv6Prefix(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		if prefix.Addr().Is6() && !prefix.Addr().IsUnspecified() {
			return prefix.Masked().String()
		}
		return ""
	}
	if address, err := netip.ParseAddr(raw); err == nil && address.Is6() && !address.IsUnspecified() {
		return netip.PrefixFrom(address, 64).Masked().String()
	}
	return ""
}

func livePPPoEDelegatedPrefix(peerID string) string {
	peerID = strings.TrimSpace(peerID)
	if !validPPPoEStatusID(peerID) {
		return ""
	}
	statusDir := strings.TrimSpace(os.Getenv("LY_ROUTE_PPPOE_STATUS_DIR"))
	if statusDir == "" {
		statusDir = "/run/ly-route/pppoe"
	}
	content, err := os.ReadFile(filepath.Join(statusDir, peerID+".json"))
	if err != nil {
		return ""
	}
	var status struct {
		State   string `json:"state"`
		Session struct {
			DelegatedPrefix string `json:"delegated_prefix"`
		} `json:"session"`
	}
	if json.Unmarshal(content, &status) != nil || status.State != "connected" {
		return ""
	}
	return normalizeIPv6Prefix(status.Session.DelegatedPrefix)
}
