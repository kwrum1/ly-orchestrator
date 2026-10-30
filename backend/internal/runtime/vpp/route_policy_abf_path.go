package vpp

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// proxyIngressPath is the ABF path that steers a route policy into one proxy
// egress: the service ingress host address plus the VPP TAP that owns the
// matching service /30.
type proxyIngressPath struct {
	NextHop netip.Addr
	Network netip.Prefix
	TAP     string
}

// parseProxyIngressPath recognises the `<host-address> lypxin<id>` ABF path
// form used for proxy service handoffs. A WAN path, a lookup DPO
// (`ip4-lookup-in-table`) or `local` is not a proxy ingress path; those are
// left to their own lifecycles.
func parseProxyIngressPath(via string) (proxyIngressPath, bool) {
	fields := strings.Fields(strings.TrimSpace(via))
	if len(fields) != 2 {
		return proxyIngressPath{}, false
	}
	tap := strings.TrimSpace(fields[1])
	if !strings.HasPrefix(strings.ToLower(tap), proxyServiceIngressPrefix) {
		return proxyIngressPath{}, false
	}
	nextHop, err := netip.ParseAddr(strings.TrimSpace(fields[0]))
	if err != nil || !nextHop.Is4() {
		return proxyIngressPath{}, false
	}
	// ServiceNetworkForEgressID places the VPP peer at .1 and the Linux peer
	// at .2 of one /30; the ABF next hop is always the Linux peer.
	vppPeer := nextHop.Prev()
	if !vppPeer.IsValid() {
		return proxyIngressPath{}, false
	}
	return proxyIngressPath{
		NextHop: nextHop,
		Network: netip.PrefixFrom(vppPeer, 30).Masked(),
		TAP:     tap,
	}, true
}

// validateRoutePolicyABFVia refuses a plan that would install an ABF path
// which cannot reach the proxy service.
//
// `local`/`receive` punts every matched flow into the host stack (VPP prints
// it as `dpo-receive`), and a proxy policy whose path does not name its own
// service TAP installs a path that drops the traffic while every "the policy
// exists" readback still passes. Both shapes must fail the apply instead.
//
// Delete/rollback operations are exempt: removing a live `local` path is the
// repair, not the defect.
func validateRoutePolicyABFVia(spec routePolicyVPPCTLSpec) error {
	if !spec.apply {
		return nil
	}
	via := strings.TrimSpace(spec.via)
	if via == "" {
		return snapshotDecodeError("route policy %q has no ABF next hop; refusing to install an unbound path", spec.policy.ID)
	}
	for _, field := range strings.Fields(via) {
		token := strings.ToLower(strings.Trim(field, ","))
		if token == "local" || token == "receive" {
			return snapshotDecodeError("route policy %q would install ABF path %q; a local/receive DPO cannot reach the proxy service, refusing to drop the matched traffic", spec.policy.ID, via)
		}
	}
	if !routePolicyUsesProxyServiceInterface(spec.policy) {
		return nil
	}
	path, ok := parseProxyIngressPath(via)
	if !ok {
		return snapshotDecodeError("route policy %q targets proxy service interface %q but its ABF path %q is not a service path", spec.policy.ID, spec.policy.Path.VPPInterface, via)
	}
	if !strings.EqualFold(path.TAP, strings.TrimSpace(spec.policy.Path.VPPInterface)) {
		return snapshotDecodeError("route policy %q ABF path %q does not target the planned service TAP %q", spec.policy.ID, via, spec.policy.Path.VPPInterface)
	}
	return nil
}

// parseVPPInterfaceAddresses maps an interface name to the prefixes printed by
// `show interface address`. The inventory command prints one header line per
// interface (`lypxin1 (up):`) followed by indented `L3 <prefix>` lines.
func parseVPPInterfaceAddresses(output string) map[string][]netip.Prefix {
	addresses := map[string][]netip.Prefix{}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			name := strings.TrimSuffix(strings.Fields(trimmed)[0], ":")
			if strings.Contains(trimmed, "unknown input") || strings.Contains(trimmed, "unknown interface") {
				current = ""
				continue
			}
			current = name
			continue
		}
		if current == "" {
			continue
		}
		for _, field := range strings.Fields(trimmed) {
			value := strings.TrimRight(field, ",")
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				continue
			}
			addresses[current] = append(addresses[current], prefix.Masked())
		}
	}
	return addresses
}

func interfaceHasNetwork(prefixes []netip.Prefix, network netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Masked() == network {
			return true
		}
	}
	return false
}

// proxyServiceIngressAddresses reads the live service TAP inventory. The
// address readback is what proves a TAP belongs to *this* egress: the name is
// derived from the egress id, but a half-created or foreign TAP can carry the
// name without the /30 the ABF next hop lives in.
func (channel vppctlChannel) proxyServiceIngressAddresses(ctx context.Context) (map[string][]netip.Prefix, error) {
	output, err := channel.runReadOnlyCommand(ctx, "show", "interface", "address")
	if err != nil {
		return nil, fmt.Errorf("read live VPP interface addresses: %w", err)
	}
	return parseVPPInterfaceAddresses(output), nil
}

// liveProxyServiceIngress returns the live ingress TAP that owns the service
// network of one ABF path. Zero candidates mean the egress service network is
// not ready; more than one is ambiguous and fails closed.
func liveProxyServiceIngress(live map[string]bool, addresses map[string][]netip.Prefix, network netip.Prefix) []string {
	candidates := make([]string, 0, 2)
	for name := range live {
		if !strings.HasPrefix(strings.ToLower(name), proxyServiceIngressPrefix) {
			continue
		}
		if interfaceHasNetwork(addresses[name], network) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	return candidates
}
