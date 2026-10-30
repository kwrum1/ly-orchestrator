package vpp

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"ly-route/backend/internal/runtime/nat"
)

// DNSServiceNetworkNATPoolError reports why a DNS service VRF cannot own a
// usable NAT44 outside address. The address is tenant-scoped in VPP: an inside
// interface in VRF 57613 can only translate through a pool entry whose
// fib_index is 57613. The global LAN->WAN pool lives in VRF 0 and cannot serve
// the service handoff.
type DNSServiceNetworkNATPoolError struct {
	UpstreamID   string
	WANInterface string
	WANPrefix    string
	TableID      int
	Reason       string
}

func (failure *DNSServiceNetworkNATPoolError) Error() string {
	if failure == nil {
		return ""
	}
	reason := strings.TrimSpace(failure.Reason)
	if reason == "" {
		reason = "no spare WAN address is available for the service VRF"
	}
	return fmt.Sprintf("DNS service network %q cannot provision a NAT44 outside address in tenant VRF %d for WAN %q (%s): %s",
		failure.UpstreamID, failure.TableID, failure.WANInterface, failure.WANPrefix, reason)
}

// dnsServiceNetworkNATPoolAddresses returns the dedicated NAT44 outside address
// for every DNS service network, in plan order. VPP's nat44_ed_add_address
// rejects an address that is already present in any tenant VRF, so the global
// pool address (the WAN interface address) cannot be reused. A deterministic
// spare address is allocated from the WAN's own subnet instead.
func dnsServiceNetworkNATPoolAddresses(plan Plan) ([]string, error) {
	addresses := make([]string, len(plan.DNSServiceNetworks))
	if len(plan.DNSServiceNetworks) == 0 {
		return addresses, nil
	}

	reserved := map[netip.Addr]struct{}{}
	for _, assignment := range plan.AddressAssignments {
		if prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR)); err == nil && prefix.Addr().Is4() {
			reserved[prefix.Addr()] = struct{}{}
		}
	}
	for _, pool := range nat44PoolAddresses(plan.AddressAssignments) {
		if address, err := netip.ParseAddr(strings.TrimSpace(pool.Address)); err == nil {
			reserved[address] = struct{}{}
		}
	}
	// Explicit mappings and port forwards own their external addresses too.
	for _, mapping := range plan.NAT.StaticMappings {
		if address, err := netip.ParseAddr(strings.TrimSpace(mapping.ExternalAddress)); err == nil {
			reserved[address] = struct{}{}
		}
	}
	for _, mapping := range plan.NAT.PortMappings {
		if address, err := netip.ParseAddr(strings.TrimSpace(mapping.ExternalAddress)); err == nil {
			reserved[address] = struct{}{}
		}
	}

	// Allocate in a stable order so collisions between two service networks do
	// not depend on request or storage iteration order.
	order := make([]int, len(plan.DNSServiceNetworks))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(left, right int) bool {
		leftNetwork := plan.DNSServiceNetworks[order[left]]
		rightNetwork := plan.DNSServiceNetworks[order[right]]
		if leftNetwork.TableID != rightNetwork.TableID {
			return leftNetwork.TableID < rightNetwork.TableID
		}
		return leftNetwork.UpstreamID < rightNetwork.UpstreamID
	})

	for _, index := range order {
		network := plan.DNSServiceNetworks[index]
		address, err := allocateDNSServiceNetworkNATPoolAddress(plan, network, reserved)
		if err != nil {
			return nil, err
		}
		addresses[index] = address
		if parsed, parseErr := netip.ParseAddr(address); parseErr == nil {
			reserved[parsed] = struct{}{}
		}
	}
	return addresses, nil
}

func allocateDNSServiceNetworkNATPoolAddress(plan Plan, network DNSServiceNetwork, reserved map[netip.Addr]struct{}) (string, error) {
	underlay := dnsServiceNetworkUnderlayInterface(network)
	assignment, found := dnsServiceNetworkWANAssignment(plan.AddressAssignments, underlay)
	if !found {
		if existing := strings.TrimSpace(network.NATPoolAddress); existing != "" {
			return existing, nil
		}
		return "", &DNSServiceNetworkNATPoolError{
			UpstreamID:   network.UpstreamID,
			WANInterface: underlay,
			TableID:      network.TableID,
			Reason:       "the WAN has no static IPv4 assignment to derive a service-VRF pool address from",
		}
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
	if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() {
		return "", &DNSServiceNetworkNATPoolError{
			UpstreamID:   network.UpstreamID,
			WANInterface: firstNonEmpty(strings.TrimSpace(assignment.VPPInterface), underlay),
			WANPrefix:    strings.TrimSpace(assignment.CIDR),
			TableID:      network.TableID,
			Reason:       "the WAN assignment is not a static IPv4 prefix",
		}
	}
	prefix = prefix.Masked()
	bits := prefix.Bits()
	if bits > 30 || bits < 8 {
		return "", &DNSServiceNetworkNATPoolError{
			UpstreamID:   network.UpstreamID,
			WANInterface: firstNonEmpty(strings.TrimSpace(assignment.VPPInterface), underlay),
			WANPrefix:    prefix.String(),
			TableID:      network.TableID,
			Reason:       "the WAN prefix has no usable spare host range for a service-VRF pool address",
		}
	}

	first := prefix.Addr()
	last := lastIPv4Address(prefix)
	usable := uint32(1) << uint(32-bits)
	usable -= 2 // network and broadcast
	start := dnsServicePoolOffset(network, prefix)

	firstBytes := first.As4()
	firstValue := binary.BigEndian.Uint32(firstBytes[:])
	gateway, _ := netip.ParseAddr(dnsServiceNetworkGateway(network.UnderlayRoute))
	for probe := uint32(0); probe < usable; probe++ {
		offset := (start + probe) % usable
		var candidateBytes [4]byte
		binary.BigEndian.PutUint32(candidateBytes[:], firstValue+1+offset)
		candidate := netip.AddrFrom4(candidateBytes)
		if candidate.Compare(last) >= 0 {
			continue
		}
		if _, used := reserved[candidate]; used {
			continue
		}
		if gateway.IsValid() && candidate == gateway {
			continue
		}
		return candidate.String(), nil
	}
	return "", &DNSServiceNetworkNATPoolError{
		UpstreamID:   network.UpstreamID,
		WANInterface: firstNonEmpty(strings.TrimSpace(assignment.VPPInterface), underlay),
		WANPrefix:    prefix.String(),
		TableID:      network.TableID,
		Reason:       "every spare host address in the WAN prefix is already reserved",
	}
}

func dnsServiceNetworkWANAssignment(assignments []AddressAssignment, underlay string) (AddressAssignment, bool) {
	underlay = strings.TrimSpace(underlay)
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			continue
		}
		if strings.TrimSpace(assignment.VPPInterface) == underlay {
			return assignment, true
		}
	}
	// A PPPoE or WAN-group underlay can carry a different interface name than
	// the address assignment. If there is exactly one static WAN, that is the
	// only possible outside address source.
	var candidates []AddressAssignment
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			continue
		}
		if prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR)); err == nil && prefix.Addr().Is4() && !prefix.Addr().IsUnspecified() {
			candidates = append(candidates, assignment)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return AddressAssignment{}, false
}

func dnsServicePoolOffset(network DNSServiceNetwork, prefix netip.Prefix) uint32 {
	bits := prefix.Bits()
	usable := uint32(1) << uint(32-bits)
	if usable <= 2 {
		return 0
	}
	usable -= 2
	digest := sha256.Sum256([]byte(network.UpstreamID + "\x00" + network.WANEgressID + "\x00" + strconv.Itoa(network.TableID) + "\x00" + prefix.String()))
	return binary.BigEndian.Uint32(digest[:4]) % usable
}

func lastIPv4Address(prefix netip.Prefix) netip.Addr {
	address := prefix.Masked().Addr().As4()
	bits := prefix.Bits()
	value := binary.BigEndian.Uint32(address[:])
	if bits < 32 {
		value |= (uint32(1) << uint(32-bits)) - 1
	}
	var last [4]byte
	binary.BigEndian.PutUint32(last[:], value)
	return netip.AddrFrom4(last)
}

func dnsServiceNetworkGateway(underlayRoute string) string {
	for _, field := range strings.Fields(underlayRoute) {
		if address, err := netip.ParseAddr(field); err == nil && address.Is4() {
			return address.String()
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// nat44PoolShowCommand returns the CLI inventory for the active NAT44 flavor.
func nat44PoolShowCommand(behavior nat.Behavior) string {
	if behavior == nat.BehaviorFullCone {
		return "show nat44 ei addresses"
	}
	return "show nat44 addresses"
}

func nat44PoolAddCommand(behavior nat.Behavior, address string, tableID int) string {
	if behavior == nat.BehaviorFullCone {
		return fmt.Sprintf("?nat44 ei add address %s tenant-vrf %d", address, tableID)
	}
	return fmt.Sprintf("?nat44 add address %s tenant-vrf %d", address, tableID)
}

func nat44PoolDeleteCommand(behavior nat.Behavior, address string, tableID int) string {
	if behavior == nat.BehaviorFullCone {
		return fmt.Sprintf("?nat44 ei add address %s tenant-vrf %d del", address, tableID)
	}
	return fmt.Sprintf("?nat44 add address %s tenant-vrf %d del", address, tableID)
}

type nat44PoolAddressEntry struct {
	Address     string
	VRF         int
	VRFSet      bool
	Independent bool
}

// parseNAT44PoolAddresses parses `show nat44 addresses` / `show nat44 ei
// addresses`. The twice-nat section is ignored: it is a separate translation
// namespace and must not satisfy a service-VRF proof.
func parseNAT44PoolAddresses(output string) []nat44PoolAddressEntry {
	entries := []nat44PoolAddressEntry{}
	var current *nat44PoolAddressEntry
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "twice-nat pool addresses") {
			break
		}
		if strings.Contains(strings.ToLower(line), "twice-nat") {
			continue
		}
		if address, err := netip.ParseAddr(line); err == nil && address.Is4() {
			entries = append(entries, nat44PoolAddressEntry{Address: address.String()})
			current = &entries[len(entries)-1]
			continue
		}
		if current == nil {
			continue
		}
		if index := strings.Index(strings.ToLower(line), "tenant vrf:"); index >= 0 {
			value := strings.TrimSpace(line[index+len("tenant vrf:"):])
			if strings.EqualFold(value, "independent") {
				current.Independent = true
				current.VRFSet = true
				continue
			}
			if vrf, err := strconv.Atoi(value); err == nil {
				current.VRF = vrf
				current.VRFSet = true
			}
		}
	}
	return entries
}

func nat44PoolHasAddressForVRF(output, address string, tableID int) bool {
	address = strings.TrimSpace(address)
	for _, entry := range parseNAT44PoolAddresses(output) {
		if entry.Address == address && entry.VRFSet && !entry.Independent && entry.VRF == tableID {
			return true
		}
	}
	return false
}

func nat44PoolAddressesForVRF(output string, tableID int) []string {
	var addresses []string
	for _, entry := range parseNAT44PoolAddresses(output) {
		if entry.VRFSet && !entry.Independent && entry.VRF == tableID {
			addresses = append(addresses, entry.Address)
		}
	}
	return addresses
}

// nat44PoolHasPlainAddress accepts inventories that do not print tenant-VRF
// metadata (notably the deterministic VPP test harness). Production readback
// still proves the tenant form through nat44PoolHasAddressForVRF first.
func nat44PoolHasPlainAddress(output, address string) bool {
	address = strings.TrimSpace(address)
	if address == "" {
		return false
	}
	for _, entry := range parseNAT44PoolAddresses(output) {
		if entry.Address == address && !entry.VRFSet {
			return true
		}
	}
	return false
}

// nat44PoolOutputAny returns the first successful pool inventory in the
// operation readback. A DNS operation carries exactly one of the ED/EI shows,
// so verification must not demand the flavor that is not running.
func nat44PoolOutputAny(results []VPPCTLCommandResult) (string, bool) {
	var fallback string
	for _, command := range []string{"show nat44 addresses", "show nat44 ei addresses"} {
		for _, result := range results {
			if result.Command != command || result.Retval != 0 || strings.TrimSpace(result.Stdout) == "" {
				continue
			}
			if len(parseNAT44PoolAddresses(result.Stdout)) > 0 {
				return result.Stdout, true
			}
			if fallback == "" {
				fallback = result.Stdout
			}
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}
