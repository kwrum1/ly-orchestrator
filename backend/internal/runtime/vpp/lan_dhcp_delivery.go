package vpp

import (
	"context"
	"fmt"
	"strings"
)

// LAN DHCP delivery to the host service tap.
//
// A DHCP client that has no lease yet sends every DISCOVER to the IPv4 limited
// broadcast address (0.0.0.0:68 -> 255.255.255.255:67). Dnsmasq answers it on
// the Linux control-plane (LCP) host interface, so the broadcast has to reach
// the tap that backs the LAN port. VPP's linux-cp plugin builds that path out
// of three pieces:
//
//  1. lcp_router.c installs 255.255.255.255/32 as a local (receive) FIB entry in
//     every router table it attaches, so the broadcast is neither forwarded out
//     the default route nor dropped as an unknown prefix. The LAN lifecycle
//     also asks for it explicitly (managementLCPCommands) so a pair that is not
//     yet connected to a router table still receives it.
//  2. lcp_interface.c adds `ip punt redirect rx <phy> via <tap>` for every
//     pair. Once linux-cp enables unknown-UDP punt (step 3) the DISCOVER is
//     punted to the ip4-punt arc and this redirect rewrites it onto the tap
//     with the pair's DVR adjacency, which preserves the broadcast destination
//     MAC. Without the redirect the packet leaves via error-punt and dnsmasq's
//     journal stays empty even though the FIB, the DHCP ACL and the ABF bypass
//     all read back correctly.
//  3. lcp_interface_init() enables unknown-UDP punt (udp_punt_unknown) for both
//     address families, so a UDP packet with no local listener - DHCP :67 on
//     the gateway address or on the limited broadcast - is punted instead of
//     drawing an ICMP port-unreachable. This is process-global and lives for
//     the lifetime of the ldpreload plugin, so it cannot silently disappear
//     between applies.
//
// The per-LAN object that can go missing is step 2: a pair created before the
// plugin glue ran, a pair re-created after an apply (new tap sw_if_index), or a
// redirect removed by another actor. The helpers below read the leg back on
// every LAN apply and repair it when it is absent or still points at a stale
// tap. A healthy system emits only the two readbacks, so re-applies stay
// idempotent.

// lanDHCPDelivery is the resolved per-LAN delivery leg: the VPP port that
// receives the DHCP broadcast and the tap interface that carries it to the
// host service.
type lanDHCPDelivery struct {
	VPPInterface  string
	HostInterface string
	TapInterface  string
}

// managementLCPTap returns the tap interface name of an LCP pair. `show lcp`
// prints one pair per line:
//
//	itf-pair: [1] lyroute-enp4s0 tap4097 lylan-enp4s0 102 type tap
func managementLCPTap(output, vppInterface, hostInterface string) string {
	vppInterface = strings.TrimSpace(vppInterface)
	hostInterface = strings.TrimSpace(hostInterface)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "itf-pair:" {
			continue
		}
		if fields[2] == vppInterface && fields[4] == hostInterface {
			return fields[3]
		}
	}
	return ""
}

// lanPuntRedirectBlock returns the `show ip punt redirect` block for one rx
// interface, for example:
//
//	rx lyroute-enp4s0 via:
//	  path-list:[40] locks:1 flags:no-uRPF, uRPF-list: None
//	   path:[46] pl-index:40 ip4 weight=1 pref=0 dvr:  oper-flags:resolved,
//	      tap4097
//	forwarding
//	 [@3]: ip4-dvr-tap4097-dpo l3
func lanPuntRedirectBlock(output, rxInterface string) (string, bool) {
	rxInterface = strings.TrimSpace(rxInterface)
	if rxInterface == "" {
		return "", false
	}
	lines := strings.Split(output, "\n")
	for index := 0; index < len(lines); index++ {
		fields := strings.Fields(lines[index])
		if len(fields) < 2 || fields[0] != "rx" {
			continue
		}
		start := index
		end := len(lines)
		for next := index + 1; next < len(lines); next++ {
			if nextFields := strings.Fields(lines[next]); len(nextFields) > 0 && nextFields[0] == "rx" {
				end = next
				break
			}
		}
		if fields[1] != rxInterface {
			index = end - 1
			continue
		}
		return strings.Join(lines[start:end], "\n"), true
	}
	return "", false
}

// lanPuntRedirectTargets lists the interface tokens a redirect block forwards
// to, so a redirect that still points at a stale tap is detected instead of
// being mistaken for a healthy one.
func lanPuntRedirectTargets(block string) []string {
	targets := make([]string, 0, 2)
	for _, line := range strings.Split(block, "\n") {
		for _, token := range strings.Fields(line) {
			token = strings.Trim(token, ",;:()[]")
			if !strings.HasPrefix(token, "tap") && !strings.HasPrefix(token, "host-") && !strings.HasPrefix(token, "lylan-") {
				continue
			}
			if strings.Contains(token, "-dpo") || strings.Contains(token, "dvr") {
				continue
			}
			targets = append(targets, token)
		}
	}
	return targets
}

// lanDHCPPuntRedirectPresent reports whether the rx interface redirects to the
// expected tap.
func lanDHCPPuntRedirectPresent(output, rxInterface, tapInterface string) bool {
	tapInterface = strings.TrimSpace(tapInterface)
	if tapInterface == "" {
		return false
	}
	block, ok := lanPuntRedirectBlock(output, rxInterface)
	if !ok {
		return false
	}
	for _, target := range lanPuntRedirectTargets(block) {
		if target == tapInterface {
			return true
		}
	}
	return false
}

// lanHostXConnectFeature is the ip4-unicast input feature linux-cp enables on
// the *host* side of a pair. It runs the reply direction: every packet the
// Linux stack sends on the tap (dnsmasq's OFFER/ACK) is rewritten onto the phy.
//
//	lcp_interface.c:208-230  lip_phy_adjs = adj_mcast_add_or_lock(phy)
//	lcp_node.c:380-450        linux-cp-xc-ip4: dst MAC cast -> phy mcast adjacency
//
// The broadcast reply therefore leaves the phy with the phy MAC as source and
// ff:ff:ff:ff:ff:ff as destination, which is exactly what the client sees.
// Without the feature the reply falls through to the FIB, 255.255.255.255/32 is
// a local receive entry, and the OFFER is dropped after dnsmasq already logged
// it - a one-way DHCP failure that the forward-direction checks cannot see.
const lanHostXConnectFeature = "linux-cp-xc-ip4"

// lanHostXConnectPresent reports whether `show interface features <tap>`
// lists linux-cp-xc-ip4 in the ip4-unicast arc.
func lanHostXConnectPresent(output, feature string) bool {
	feature = strings.TrimSpace(feature)
	if feature == "" {
		return false
	}
	arc := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			arc = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if arc != "ip4-unicast" {
			continue
		}
		for _, token := range strings.Fields(trimmed) {
			if token == feature {
				return true
			}
		}
	}
	return false
}

// lanHostFeaturesCommand is the per-tap readback of the reply leg.
func lanHostFeaturesCommand(tapInterface string) string {
	return "show interface features " + strings.TrimSpace(tapInterface)
}

// Why the leg is repaired by re-creating the LCP pair and not by
// `ip punt redirect add rx <phy> via <tap>`:
//
// linux-cp installs the redirect with FIB_ROUTE_PATH_DVR
// (lcp_interface.c:196,296-307). The `ip punt redirect ... via <tap>` CLI goes
// through unformat_fib_route_path, which has no `dvr` keyword, so it installs an
// *attached* path and VPP resolves it to an ipv4-glean adjacency. A glean
// adjacency ARPs for 255.255.255.255, which never resolves, so the repair would
// look installed in `show ip punt redirect` while still dropping every
// DISCOVER. Measured on the vm-lab (26.06):
//
//	path:[35] ip4 weight=1 pref=0 attached: oper-flags:resolved, tap4096
//	[@5]: ipv4-glean: [src:0.0.0.0/0] tap4096 ...      -> no OFFER
//
// Re-creating the pair is the only CLI path that reproduces the DVR redirect.
// It also re-creates the host tap, so `lcp lcp-sync on` must follow to push the
// LAN address back onto it.
//
// lanDHCPPairRecreateCommands rebuilds an LCP pair whose delivery leg is
// missing or points at a stale tap.
func lanDHCPPairRecreateCommands(vppInterface, hostInterface string) []string {
	return []string{
		"lcp delete " + strings.TrimSpace(vppInterface),
		fmt.Sprintf("lcp create %s host-if %s", strings.TrimSpace(vppInterface), strings.TrimSpace(hostInterface)),
		// A re-created tap starts without addresses; lcp-sync pushes the VPP
		// interface address (the LAN gateway) back onto the host side so
		// dnsmasq can serve the DHCP range again.
		"lcp lcp-sync on",
	}
}

// ensureLANDHCPDelivery reads both directions of the LAN delivery leg back and
// repairs them when they are missing:
//
//   - phy -> host: `ip punt redirect rx <phy> via <tap>` (DVR) carries the
//     DISCOVER to dnsmasq;
//   - host -> phy: the tap's `linux-cp-xc-ip4` feature carries the OFFER/ACK
//     back to the LAN.
//
// It emits no mutation on a healthy system. Both objects are owned by the LCP
// pair, so the repair re-creates that pair and `lcp delete` rolls the leg back
// with the pair when the LAN is disabled.
func (channel vppctlChannel) ensureLANDHCPDelivery(ctx context.Context, operation Operation, management ManagementLCP) ([]VPPCTLCommandResult, error) {
	if !management.Enabled || strings.TrimSpace(management.VPPInterface) == "" {
		return nil, nil
	}
	results, err := channel.runServiceChainCommands(ctx, operation, "show lcp")
	if err != nil {
		return nil, err
	}
	delivery := lanDHCPDelivery{
		VPPInterface:  strings.TrimSpace(management.VPPInterface),
		HostInterface: strings.TrimSpace(management.HostInterface),
		TapInterface:  managementLCPTap(resultStdout(results, "show lcp"), management.VPPInterface, management.HostInterface),
	}
	if delivery.TapInterface == "" {
		return results, snapshotDecodeError("LAN DHCP delivery: LCP pair %s/%s has no tap interface in readback", delivery.VPPInterface, delivery.HostInterface)
	}
	readback, readErr := channel.runServiceChainCommands(ctx, operation,
		"show ip punt redirect",
		lanHostFeaturesCommand(delivery.TapInterface),
	)
	if readErr != nil {
		return results, readErr
	}
	results = append(results, readback...)
	problems := lanDHCPDeliveryGaps(delivery, readback)
	if len(problems) == 0 {
		return results, nil
	}
	commands := append(lanDHCPPairRecreateCommands(delivery.VPPInterface, delivery.HostInterface),
		"show lcp",
		"show ip punt redirect",
	)
	applied, applyErr := channel.runServiceChainCommands(ctx, operation, commands...)
	if applyErr != nil {
		return results, applyErr
	}
	results = append(results, applied...)
	recreatedTap := managementLCPTap(resultStdoutLast(applied, "show lcp"), delivery.VPPInterface, delivery.HostInterface)
	if recreatedTap == "" {
		return results, snapshotDecodeError("LAN DHCP delivery: LCP pair %s/%s is absent after re-creation", delivery.VPPInterface, delivery.HostInterface)
	}
	verified, verifyErr := channel.runServiceChainCommands(ctx, operation, lanHostFeaturesCommand(recreatedTap))
	if verifyErr != nil {
		return results, verifyErr
	}
	results = append(results, verified...)
	remaining := lanDHCPDeliveryGaps(lanDHCPDelivery{
		VPPInterface:  delivery.VPPInterface,
		HostInterface: delivery.HostInterface,
		TapInterface:  recreatedTap,
	}, append(append([]VPPCTLCommandResult(nil), applied...), verified...))
	if len(remaining) > 0 {
		return results, snapshotDecodeError("LAN DHCP delivery: %s is absent from readback after re-creating the LCP pair", strings.Join(remaining, " and "))
	}
	return results, nil
}

// lanDHCPDeliveryGaps names the missing halves of the delivery leg, in the order
// an operator should fix them.
func lanDHCPDeliveryGaps(delivery lanDHCPDelivery, results []VPPCTLCommandResult) []string {
	gaps := make([]string, 0, 2)
	if !lanDHCPPuntRedirectPresent(resultStdoutLast(results, "show ip punt redirect"), delivery.VPPInterface, delivery.TapInterface) {
		gaps = append(gaps, fmt.Sprintf("inbound punt redirect rx %s via %s", delivery.VPPInterface, delivery.TapInterface))
	}
	if !lanHostXConnectPresent(resultStdoutLast(results, lanHostFeaturesCommand(delivery.TapInterface)), lanHostXConnectFeature) {
		gaps = append(gaps, fmt.Sprintf("outbound %s feature on %s", lanHostXConnectFeature, delivery.TapInterface))
	}
	return gaps
}

// verifyLANIsNotNAT44Outside fails the apply when a LAN ingress still carries
// the NAT44 outside role. nat44-ed-out2in runs before the LCP punt and drops an
// untranslated DHCP broadcast in nat44-ed-out2in-slowpath ("no translation"),
// which is indistinguishable from a missing delivery leg in dnsmasq's journal
// but is visible in a packet trace. The role is normally removed by the NAT44
// reconcile step earlier in this lifecycle; this is the readback that turns a
// silent residue (a swallowed `set interface nat44 out ... del`) into a loud
// apply failure.
func (channel vppctlChannel) verifyLANIsNotNAT44Outside(ctx context.Context, operation Operation, lanInterface string) ([]VPPCTLCommandResult, error) {
	lanInterface = strings.TrimSpace(lanInterface)
	if lanInterface == "" {
		return nil, nil
	}
	results, err := channel.runServiceChainCommands(ctx, operation, "?show nat44 interfaces", "?show nat44 ei interfaces")
	if err != nil {
		return nil, err
	}
	state := nat44RuntimeState{Inside: map[string]bool{}, Outside: map[string]bool{}, OutputFeature: map[string]bool{}}
	parseNAT44InterfaceRoles(resultStdout(results, "show nat44 interfaces"), &state)
	parseNAT44InterfaceRoles(resultStdout(results, "show nat44 ei interfaces"), &state)
	if state.Outside[lanInterface] || state.OutputFeature[lanInterface] {
		return results, snapshotDecodeError("LAN interface %s carries the NAT44 outside role; DHCP broadcasts are dropped in nat44-ed-out2in-slowpath (no translation) before the LCP punt (run `set interface nat44 out %s del`)", lanInterface, lanInterface)
	}
	return results, nil
}
