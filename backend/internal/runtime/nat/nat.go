package nat

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type StaticMapping struct {
	ID              string `json:"id"`
	ExternalAddress string `json:"external_address"`
	// ExternalAddressExplicit records that the operator supplied a VIP instead
	// of relying on the WAN lease. A VIP must survive a WAN reconnect; only an
	// address that was omitted (or still equals the current lease) follows the
	// WAN address.
	ExternalAddressExplicit bool   `json:"external_address_explicit,omitempty"`
	InternalAddress         string `json:"internal_address"`
	WANInterface            string `json:"wan_interface,omitempty"`
	WANNextHop              string `json:"wan_next_hop,omitempty"`
	ReturnPathGuard         bool   `json:"return_path_guard,omitempty"`
}

type PortMapping struct {
	ID                      string `json:"id"`
	Protocol                string `json:"protocol"`
	ExternalAddress         string `json:"external_address,omitempty"`
	ExternalAddressExplicit bool   `json:"external_address_explicit,omitempty"`
	ExternalPort            int    `json:"external_port"`
	InternalHost            string `json:"internal_host"`
	InternalPort            int    `json:"internal_port"`
	WANInterface            string `json:"wan_interface,omitempty"`
	WANNextHop              string `json:"wan_next_hop,omitempty"`
	Hairpin                 bool   `json:"hairpin,omitempty"`
	ReturnPathGuard         bool   `json:"return_path_guard,omitempty"`
}

// Behavior selects the NAT44 connection-tracking semantics for the gateway.
// Endpoint-dependent is the safe default used by NAT44-ED. Full-cone uses
// VPP's endpoint-independent NAT44-EI plugin and is a global gateway mode;
// the two VPP plugins are never mixed in one running plan.
type Behavior string

const (
	BehaviorEndpointDependent Behavior = "endpoint_dependent"
	BehaviorFullCone          Behavior = "full_cone"
)

type CompiledConfig struct {
	StaticMappings []StaticMapping `json:"static_mappings,omitempty"`
	PortMappings   []PortMapping   `json:"port_mappings,omitempty"`
	Behavior       Behavior        `json:"behavior,omitempty"`
}

// ResolveBehavior reads accepted API aliases from all NAT-related resources.
// Missing values use the endpoint-dependent default. Explicit conflicting
// values are rejected because VPP cannot safely run NAT44-ED and NAT44-EI on
// the same gateway at the same time.
func ResolveBehavior(itemSets ...[]map[string]any) (Behavior, error) {
	behavior := BehaviorEndpointDependent
	configured := false
	for _, items := range itemSets {
		for _, item := range items {
			if !enabled(item) {
				continue
			}
			candidate, explicit, err := requestedBehavior(item)
			if err != nil {
				return "", err
			}
			if !explicit {
				continue
			}
			if configured && candidate != behavior {
				return "", fmt.Errorf("conflicting NAT behaviors: %q and %q cannot be active together", behavior, candidate)
			}
			behavior, configured = candidate, true
		}
	}
	return behavior, nil
}

func requestedBehavior(item map[string]any) (Behavior, bool, error) {
	for _, key := range []string{"full_cone", "full_cone_nat", "endpoint_independent"} {
		if value, ok := item[key]; ok && truthy(value) {
			return BehaviorFullCone, true, nil
		}
	}
	for _, key := range []string{"nat_behavior", "nat_mode"} {
		value := strings.TrimSpace(strings.ToLower(fmt.Sprint(item[key])))
		if value == "" || value == "<nil>" {
			continue
		}
		normalized := strings.ReplaceAll(strings.ReplaceAll(value, "-", "_"), " ", "_")
		switch normalized {
		case "full_cone", "fullcone", "endpoint_independent", "endpoint_independent_nat", "cone":
			return BehaviorFullCone, true, nil
		case "endpoint_dependent", "endpoint_dependent_nat", "nat44_ed", "ed", "default":
			return BehaviorEndpointDependent, true, nil
		default:
			return "", false, fmt.Errorf("unsupported NAT behavior %q; use endpoint_dependent or full_cone", value)
		}
	}
	return BehaviorEndpointDependent, false, nil
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		value := strings.ToLower(strings.TrimSpace(typed))
		return value == "true" || value == "1" || value == "enabled" || value == "yes"
	default:
		return false
	}
}

func BindWANInterfaces(config *CompiledConfig, bindings map[string]string) error {
	if config == nil {
		return fmt.Errorf("NAT config is required")
	}
	for index := range config.StaticMappings {
		if err := bindWANInterface(&config.StaticMappings[index].WANInterface, config.StaticMappings[index].ID, "nat_static", bindings); err != nil {
			return err
		}
	}
	for index := range config.PortMappings {
		if err := bindWANInterface(&config.PortMappings[index].WANInterface, config.PortMappings[index].ID, "port_map", bindings); err != nil {
			return err
		}
	}
	return nil
}

func bindWANInterface(target *string, id, kind string, bindings map[string]string) error {
	wanID := strings.TrimSpace(*target)
	if wanID == "" {
		return nil
	}
	interfaceName, exists := bindings[wanID]
	if !exists || strings.TrimSpace(interfaceName) == "" {
		return fmt.Errorf("%s %q WAN %q has no runtime VPP interface binding", kind, id, wanID)
	}
	if err := optionalToken(interfaceName, kind, id, "runtime WAN interface"); err != nil {
		return err
	}
	*target = strings.TrimSpace(interfaceName)
	return nil
}

func CompileConfig(staticItems, portMapItems []map[string]any) (CompiledConfig, error) {
	return CompileConfigWithWANs(staticItems, portMapItems, nil)
}

func CompileConfigWithWANs(staticItems, portMapItems, wanItems []map[string]any) (CompiledConfig, error) {
	wanAddresses := compileWANAddresses(wanItems)
	wanPrefixes := compileWANPrefixes(wanItems)
	wanAliases, wanDynamic := compileWANAliases(wanItems)
	staticMappings := make([]StaticMapping, 0, len(staticItems))
	for _, item := range staticItems {
		if !enabled(item) {
			continue
		}
		mapping, err := compileStaticMapping(item)
		if err != nil {
			return CompiledConfig{}, err
		}
		if mapping.WANInterface != "" {
			// The operator may name either the WAN line id or the physical port
			// it owns (eth3). When WAN state is supplied, both must resolve to
			// the WAN line before the lease and next-hop lookups run; an
			// unresolvable reference is a config error, not a mapping that
			// silently never reaches VPP. A nil WAN list is the legacy
			// no-cross-resource caller and keeps the reference untouched.
			if wanItems != nil {
				canonical, known := resolveWANReference(mapping.WANInterface, wanAliases)
				if !known {
					return CompiledConfig{}, fmt.Errorf("nat_static %q wan_interface %q is not a WAN line on this device", mapping.ID, mapping.WANInterface)
				}
				mapping.WANInterface = canonical
			}
			// A static mapping created through the router UI may either follow
			// a dynamic WAN lease or own an independent VIP. Only an omitted
			// external address, or one that still equals the live lease, may be
			// refreshed. Overwriting an explicit VIP (the J1900
			// 10.1.18.35 -> 192.168.89.2 case) silently mapped the gateway's
			// own WAN address 1:1 to a LAN host.
			liveAddress := strings.TrimSpace(wanAddresses[mapping.WANInterface])
			if liveAddress != "" {
				if !mapping.ExternalAddressExplicit || mapping.ExternalAddress == liveAddress {
					mapping.ExternalAddress = liveAddress
				} else if prefix, ok := wanPrefixes[mapping.WANInterface]; ok {
					address, parseErr := netip.ParseAddr(mapping.ExternalAddress)
					if parseErr != nil || !address.Is4() {
						return CompiledConfig{}, fmt.Errorf("nat_static %q external_address %q is not a valid IPv4 address", mapping.ID, mapping.ExternalAddress)
					}
					if prefix.Bits() <= 30 && !prefix.Contains(address) {
						return CompiledConfig{}, fmt.Errorf("nat_static %q external_address %s is not inside WAN %q subnet %s", mapping.ID, mapping.ExternalAddress, mapping.WANInterface, prefix.String())
					}
				}
			}
		}
		if strings.TrimSpace(mapping.ExternalAddress) == "" {
			return CompiledConfig{}, fmt.Errorf("nat_static %q requires external_address or an acquired WAN address", mapping.ID)
		}
		staticMappings = append(staticMappings, mapping)
	}
	if err := validateStaticMappingUniqueness(staticMappings); err != nil {
		return CompiledConfig{}, err
	}

	portMappings := make([]PortMapping, 0, len(portMapItems))
	validatedPortMappings := make([]PortMapping, 0, len(portMapItems))
	for _, item := range portMapItems {
		if !enabled(item) {
			continue
		}
		// Resolve the mapping's WAN reference before compilation so a physical
		// port id (eth3) and a WAN line id name the same lease. The lookup uses
		// the canonical WAN line id, which also carries the next hop.
		item = clonePortMapItem(item)
		canonicalWAN := ""
		if reference := stringValue(item, "wan_interface", "wan_link", "interface_id", "egress"); reference != "" && wanItems != nil {
			resolved, known := resolveWANReference(reference, wanAliases)
			if !known {
				return CompiledConfig{}, fmt.Errorf("port_map %q wan interface %q is not a WAN line on this device", stringValue(item, "id", "name"), reference)
			}
			canonicalWAN = resolved
			item["wan_interface"] = canonicalWAN
			delete(item, "wan_link")
		}
		mapping, err := compilePortMapping(item, wanAddresses)
		if err != nil {
			return CompiledConfig{}, err
		}
		validatedPortMappings = append(validatedPortMappings, mapping)
		if mapping.ExternalAddress == "" {
			// A port map bound to a dynamic WAN is valid before the WAN has a
			// lease. Do not let it block PPPoE/DHCP startup; it is installed on
			// the next reconcile after the WAN address is available. Any other
			// missing address (a static WAN or an unknown reference) is a hard
			// error: dropping the mapping produced a mapping that reported
			// desired_not_applied forever while VPP never saw it.
			if wanDynamic[canonicalWAN] || (wanItems == nil && mapping.WANInterface != "") {
				continue
			}
			if canonicalWAN != "" {
				return CompiledConfig{}, fmt.Errorf("port_map %q WAN %q has no acquired IPv4 address", mapping.ID, canonicalWAN)
			}
			return CompiledConfig{}, fmt.Errorf("port_map %q requires external_address or an acquired WAN address", mapping.ID)
		}
		portMappings = append(portMappings, mapping)
	}
	if err := validatePortMappingUniqueness(validatedPortMappings); err != nil {
		return CompiledConfig{}, err
	}

	behavior, err := ResolveBehavior(staticItems, portMapItems, wanItems)
	if err != nil {
		return CompiledConfig{}, err
	}
	return CompiledConfig{StaticMappings: staticMappings, PortMappings: portMappings, Behavior: behavior}, nil
}

// validatePortMappingUniqueness enforces the VPP NAT44 static mapping
// contract. NAT44-EI and the gateway's NAT44-ED adapter both identify a
// mapping by protocol plus internal endpoint, so changing only the public
// port cannot create a second mapping for the same LAN service. Rejecting it
// during compilation keeps a bad desired state from reaching runtime apply.
func validatePortMappingUniqueness(mappings []PortMapping) error {
	seen := make(map[string]PortMapping, len(mappings))
	for _, mapping := range mappings {
		key := strings.Join([]string{mapping.Protocol, mapping.InternalHost, strconv.Itoa(mapping.InternalPort)}, "/")
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("port_map %q conflicts with %q: %s %s:%d is already mapped; VPP supports one mapping per internal endpoint", mapping.ID, previous.ID, mapping.Protocol, mapping.InternalHost, mapping.InternalPort)
		}
		seen[key] = mapping
	}
	return nil
}

func validateStaticMappingUniqueness(mappings []StaticMapping) error {
	byExternal := make(map[string]StaticMapping, len(mappings))
	byInternal := make(map[string]StaticMapping, len(mappings))
	for _, mapping := range mappings {
		if previous, exists := byExternal[mapping.ExternalAddress]; exists && previous.InternalAddress != mapping.InternalAddress {
			return fmt.Errorf("nat_static %q conflicts with %q: external address %s is already mapped to %s", mapping.ID, previous.ID, mapping.ExternalAddress, previous.InternalAddress)
		}
		if previous, exists := byInternal[mapping.InternalAddress]; exists && previous.ExternalAddress != mapping.ExternalAddress {
			return fmt.Errorf("nat_static %q conflicts with %q: internal host %s is already mapped to %s", mapping.ID, previous.ID, mapping.InternalAddress, previous.ExternalAddress)
		}
		byExternal[mapping.ExternalAddress] = mapping
		byInternal[mapping.InternalAddress] = mapping
	}
	return nil
}

func compileStaticMapping(item map[string]any) (StaticMapping, error) {
	id := stringValue(item, "id", "name")
	externalAddress := stringValue(item, "external_address", "public_ip", "wan_ip", "external_ip")
	mapping := StaticMapping{
		ID:                      id,
		ExternalAddress:         externalAddress,
		ExternalAddressExplicit: externalAddress != "",
		InternalAddress:         stringValue(item, "internal_address", "internal_host", "private_ip", "inside_address"),
		WANInterface:            stringValue(item, "wan_interface", "wan_link", "interface_id", "egress"),
		ReturnPathGuard:         true,
	}
	if err := requireID(id, "nat_static"); err != nil {
		return StaticMapping{}, err
	}
	if mapping.ExternalAddress == "" && mapping.WANInterface == "" {
		return StaticMapping{}, fmt.Errorf("nat_static %q requires external_address or wan_interface", id)
	}
	if err := optionalToken(mapping.ExternalAddress, "nat_static", id, "external_address"); err != nil {
		return StaticMapping{}, err
	}
	if err := requireToken(mapping.InternalAddress, "nat_static", id, "internal_address"); err != nil {
		return StaticMapping{}, err
	}
	if err := optionalToken(mapping.WANInterface, "nat_static", id, "wan_interface"); err != nil {
		return StaticMapping{}, err
	}
	if strings.Contains(mapping.ExternalAddress, ":") || strings.Contains(mapping.InternalAddress, ":") {
		return StaticMapping{}, fmt.Errorf("nat_static %q only supports IPv4 NAT44", id)
	}
	return mapping, nil
}

func compilePortMapping(item map[string]any, wanAddresses map[string]string) (PortMapping, error) {
	id := stringValue(item, "id", "name")
	internalHost := stringValue(item, "internal_host", "internal_address", "private_ip", "dst_ip")
	internalPort, hasInternalPort := intValue(item, "internal_port", "private_port", "dst_port")
	if internalHost == "" || !hasInternalPort {
		host, port, ok := splitHostPort(stringValue(item, "internal_target"))
		if internalHost == "" {
			internalHost = host
		}
		if !hasInternalPort && ok {
			internalPort = port
			hasInternalPort = true
		}
	}
	externalPort, hasExternalPort := intValue(item, "external_port", "public_port", "src_port")
	mapping := PortMapping{
		ID:              id,
		Protocol:        strings.ToLower(stringValue(item, "protocol")),
		ExternalAddress: stringValue(item, "external_address", "public_ip", "wan_ip", "external_ip"),
		ExternalPort:    externalPort,
		InternalHost:    internalHost,
		InternalPort:    internalPort,
		WANInterface:    stringValue(item, "wan_interface", "wan_link", "interface_id", "egress"),
		Hairpin:         boolValue(item, "hairpin", "internal_loopback", "loopback"),
		ReturnPathGuard: true,
	}
	if mapping.Protocol == "" {
		mapping.Protocol = "tcp"
	}
	// An explicit external address is an independent NAT pool/VIP, not the
	// transient WAN lease. Hairpin requires this address to be distinct from the
	// PPPoE interface address; overwriting it with the live lease re-creates the
	// unsupported "hairpin to the WAN's own address" case.
	// Hairpin requires an independent VIP/NAT-pool address. An explicitly
	// supplied external address on a hairpin mapping is that VIP and must not be
	// overwritten by the transient WAN lease. Non-hairpin mappings retain the
	// legacy behavior: an external address that looks like a WAN lease is
	// refreshed on reconnect.
	mapping.ExternalAddressExplicit = strings.TrimSpace(mapping.ExternalAddress) != "" && mapping.Hairpin
	if mapping.WANInterface != "" && !mapping.ExternalAddressExplicit {
		if liveAddress := strings.TrimSpace(wanAddresses[mapping.WANInterface]); liveAddress != "" {
			mapping.ExternalAddress = liveAddress
		}
	}
	if err := requireID(id, "port_map"); err != nil {
		return PortMapping{}, err
	}
	if mapping.ExternalAddress == "" && mapping.WANInterface == "" {
		return PortMapping{}, fmt.Errorf("port_map %q requires an acquired WAN address or external_address", id)
	}
	if err := optionalToken(mapping.ExternalAddress, "port_map", id, "external_address"); err != nil {
		return PortMapping{}, err
	}
	if err := optionalToken(mapping.WANInterface, "port_map", id, "wan_interface"); err != nil {
		return PortMapping{}, err
	}
	if err := requireToken(mapping.InternalHost, "port_map", id, "internal_host"); err != nil {
		return PortMapping{}, err
	}
	if strings.Contains(mapping.ExternalAddress, ":") || strings.Contains(mapping.InternalHost, ":") {
		return PortMapping{}, fmt.Errorf("port_map %q only supports IPv4 NAT44", id)
	}
	if err := requireToken(mapping.Protocol, "port_map", id, "protocol"); err != nil {
		return PortMapping{}, err
	}
	if mapping.Protocol != "tcp" && mapping.Protocol != "udp" {
		return PortMapping{}, fmt.Errorf("port_map %q protocol must be tcp or udp", id)
	}
	if !hasExternalPort || externalPort <= 0 || externalPort > 65535 {
		return PortMapping{}, fmt.Errorf("port_map %q requires external_port in 1..65535", id)
	}
	if !hasInternalPort || internalPort <= 0 || internalPort > 65535 {
		return PortMapping{}, fmt.Errorf("port_map %q requires internal_port in 1..65535", id)
	}
	if mapping.Hairpin {
		return mapping, fmt.Errorf("port_map %q hairpin is not supported by the VPP 26.06 NAT44 path; use a routed external VIP without hairpin or configure the source network to reach the internal service directly", id)
	}
	return mapping, nil
}

// compileWANAliases indexes every spelling of a WAN line so a port map or
// static NAT mapping may name the WAN line id, the physical port id it owns, or
// either of the display/system names. Without this a mapping created with
// interface_id=eth3 compiled to an empty external address and was silently
// dropped from the plan.
func compileWANAliases(items []map[string]any) (map[string]string, map[string]bool) {
	aliases := make(map[string]string, len(items)*5)
	dynamic := make(map[string]bool, len(items))
	for _, item := range items {
		id := wanItemID(item)
		if id == "" {
			continue
		}
		dynamic[id] = wanIsDynamic(item)
		for _, key := range []string{"id", "interface_id", "name", "system_name", "display_name"} {
			alias := stringValue(item, key)
			if alias == "" {
				continue
			}
			if _, exists := aliases[alias]; !exists {
				aliases[alias] = id
			}
		}
	}
	return aliases, dynamic
}

func wanItemID(item map[string]any) string {
	return stringValue(item, "id", "interface_id", "name")
}

// wanIsDynamic reports whether a WAN line acquires its address at runtime. A
// dynamic line legitimately has no address before its first lease; a static
// line without an address is a configuration error.
func wanIsDynamic(item map[string]any) bool {
	for _, key := range []string{"type", "wan_type", "connection_type"} {
		switch strings.ToLower(stringValue(item, key)) {
		case "pppoe", "dhcp", "dhcpv4", "auto":
			return true
		}
	}
	if ipv4, ok := item["ipv4"].(map[string]any); ok {
		switch strings.ToLower(stringValue(ipv4, "mode")) {
		case "pppoe", "dhcp":
			return true
		}
	}
	return false
}

func resolveWANReference(reference string, aliases map[string]string) (string, bool) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", false
	}
	canonical, known := aliases[reference]
	return canonical, known
}

func clonePortMapItem(item map[string]any) map[string]any {
	clone := make(map[string]any, len(item))
	for key, value := range item {
		clone[key] = value
	}
	return clone
}

func compileWANAddresses(items []map[string]any) map[string]string {
	addresses := map[string]string{}
	for _, item := range items {
		id := stringValue(item, "id", "interface_id", "name")
		address := normalizeIPv4Address(stringValue(item, "external_address", "current_address", "address", "ip", "ip_cidr", "cidr"))
		if id != "" && address != "" {
			addresses[id] = address
		}
	}
	return addresses
}

// compileWANPrefixes keeps each WAN's live IPv4 subnet so an explicit VIP can
// be validated against the WAN it claims to belong to. A missing prefix simply
// skips that check (legacy callers and dynamic WANs do not carry one).
func compileWANPrefixes(items []map[string]any) map[string]netip.Prefix {
	prefixes := map[string]netip.Prefix{}
	for _, item := range items {
		id := stringValue(item, "id", "interface_id", "name")
		if id == "" {
			continue
		}
		raw := strings.TrimSpace(stringValue(item, "current_cidr", "ip_cidr", "cidr", "external_address", "current_address", "address", "ip"))
		if raw == "" || strings.Contains(raw, ":") {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			address, addrErr := netip.ParseAddr(raw)
			if addrErr != nil || !address.Is4() {
				continue
			}
			prefix = netip.PrefixFrom(address, 32)
		}
		if prefix.Addr().Is4() {
			prefixes[id] = prefix.Masked()
		}
	}
	return prefixes
}

func normalizeIPv4Address(value string) string {
	address := strings.TrimSpace(value)
	if address == "" || strings.Contains(address, ":") {
		return ""
	}
	if index := strings.Index(address, "/"); index > 0 {
		address = address[:index]
	}
	return address
}

func enabled(item map[string]any) bool {
	value, ok := item["enabled"]
	if !ok {
		return true
	}
	boolValue, ok := value.(bool)
	return !ok || boolValue
}

func boolValue(item map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
		}
	}
	return false
}

func stringValue(item map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func intValue(item map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case int:
			return typed, true
		case int64:
			return int(typed), true
		case float64:
			if typed == float64(int(typed)) {
				return int(typed), true
			}
		case jsonNumber:
			parsed, err := strconv.Atoi(string(typed))
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.Atoi(strings.TrimSpace(typed))
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

type jsonNumber string

func splitHostPort(value string) (string, int, bool) {
	index := strings.LastIndex(value, ":")
	if index <= 0 || index == len(value)-1 {
		return "", 0, false
	}
	port, err := strconv.Atoi(value[index+1:])
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(value[:index]), port, true
}

func requireID(id, resourceType string) error {
	if id == "" {
		return fmt.Errorf("%s requires id", resourceType)
	}
	return requireCleanToken(id, resourceType, id, "id")
}

func requireToken(value, resourceType, id, field string) error {
	if value == "" {
		return fmt.Errorf("%s %q requires %s", resourceType, id, field)
	}
	return requireCleanToken(value, resourceType, id, field)
}

func optionalToken(value, resourceType, id, field string) error {
	if value == "" {
		return nil
	}
	return requireCleanToken(value, resourceType, id, field)
}

func requireCleanToken(value, resourceType, id, field string) error {
	if strings.ContainsAny(value, " \t\n\r|&;$`()<>\\\"") {
		return fmt.Errorf("%s %q %s contains unsupported command characters", resourceType, id, field)
	}
	return nil
}
