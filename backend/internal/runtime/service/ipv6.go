package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var ErrIPv6PrefixTooSmall = errors.New("ipv6 delegated prefix is too small for LAN RA")

// DelegatedLANPrefix returns the index-th /64 inside a delegated prefix.
// Prefixes shorter than /64 are sub-delegated deterministically so multiple
// LANs never share the same on-link prefix. A /64 can only serve index zero.
func DelegatedLANPrefix(delegated string, index int) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(delegated))
	if err != nil || !prefix.Addr().Is6() || prefix.Addr().IsUnspecified() {
		return netip.Prefix{}, fmt.Errorf("ipv6 delegated prefix %q is invalid", delegated)
	}
	prefix = prefix.Masked()
	if prefix.Bits() > 64 {
		return netip.Prefix{}, fmt.Errorf("%w: %s cannot allocate a /64", ErrIPv6PrefixTooSmall, prefix)
	}
	if index < 0 {
		return netip.Prefix{}, fmt.Errorf("ipv6 LAN index must not be negative")
	}
	available := uint64(1) << (64 - prefix.Bits())
	if available == 0 || uint64(index) >= available {
		return netip.Prefix{}, fmt.Errorf("%w: %s has no /64 for LAN index %d", ErrIPv6PrefixTooSmall, prefix, index)
	}
	raw := prefix.Addr().As16()
	high := binary.BigEndian.Uint64(raw[:8])
	binary.BigEndian.PutUint64(raw[:8], high+uint64(index))
	return netip.PrefixFrom(netip.AddrFrom16(raw), 64), nil
}

// ipv6AddressOffset adds a small offset to the low 64 bits of a /64 prefix and
// returns the resulting address.
func ipv6AddressOffset(prefix netip.Prefix, offset uint64) netip.Addr {
	raw := prefix.Masked().Addr().As16()
	low := binary.BigEndian.Uint64(raw[8:])
	binary.BigEndian.PutUint64(raw[8:], low+offset)
	return netip.AddrFrom16(raw)
}

// IPv6GatewayAddress returns the conventional ::1 gateway address of a LAN /64.
func IPv6GatewayAddress(prefix netip.Prefix) string {
	return ipv6AddressOffset(prefix, 1).String()
}

type IPv6RAPlan struct {
	Interface       string `json:"interface"`
	DelegatedPrefix string `json:"delegated_prefix"`
}

func RenderIPv6RA(plan IPv6RAPlan) ([]RenderedArtifact, error) {
	interfaceName := strings.TrimSpace(plan.Interface)
	if interfaceName == "" || interfaceName != plan.Interface || strings.ContainsAny(interfaceName, "\x00\n\r\t ;|&$()<>\\\"") {
		return nil, fmt.Errorf("ipv6 RA interface is invalid")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(plan.DelegatedPrefix))
	if err != nil || !prefix.Addr().Is6() {
		return nil, fmt.Errorf("ipv6 delegated prefix %q is invalid", plan.DelegatedPrefix)
	}
	prefix = prefix.Masked()
	if prefix.Bits() > 64 {
		return nil, fmt.Errorf("%w: %s cannot allocate a /64", ErrIPv6PrefixTooSmall, prefix)
	}
	lanPrefix := netip.PrefixFrom(prefix.Addr(), 64).Masked()
	content := fmt.Sprintf("interface %s\n{\n  AdvSendAdvert on;\n  MinRtrAdvInterval 30;\n  MaxRtrAdvInterval 100;\n  prefix %s\n  {\n    AdvOnLink on;\n    AdvAutonomous on;\n  };\n};\n", interfaceName, lanPrefix)
	return []RenderedArtifact{NewArtifact(IPv6RA, "/etc/radvd.conf", content, "reload")}, nil
}
