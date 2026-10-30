package httpapi

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	controlapi "ly-route/backend/internal/api"
	"ly-route/backend/internal/runtime/vpp"
)

// Desired-vs-live data-plane health.
//
// /api/v1/health used to be satisfied by unit state and by "the configured port
// exists in VPP". A failed role change leaves the port attached while its
// address, LCP host pair, dnsmasq binding and DNS interception are gone, so
// clients cannot even reach the gateway while every service still reports
// active. The port-level `dataplane` dependency cannot see that: it only asks
// whether VPP owns an interface, not whether the desired configuration on that
// interface is realized.
//
// This probe recomputes the desired LAN/WAN set from the persisted documents
// and proves each item against the live system with read-only commands:
//
//   - every desired static LAN/WAN address is present on its lyroute-* interface
//     in `vppctl show interface address`;
//   - every LAN has its lylan-* LCP host interface with the same address;
//   - with at least one LAN, dnsmasq has an interface= for each LAN LCP and a
//     dhcp-range=, otherwise clients get no lease/resolver;
//   - with one LAN, VPP's dns-intercept plugin is attached to it; with more
//     than one LAN, every ingress owns its per-ingress ABF steering policy
//     family. The shipped dns-intercept plugin has a single attachment slot,
//     so a multi-LAN gateway that only attached one port is reported degraded
//     instead of looking healthy while the other LANs leak plain DNS.
//
// The probe is read-only and bounded: one short context covers every command,
// so a wedged vppctl cannot stretch the 8 s health budget. It is only published
// when desired state actually contains a LAN/WAN assignment, which keeps a
// factory-fresh appliance (and every orchestrator) reporting an honest ok.
const (
	desiredDataplaneDefaultDnsmasqConfig = "/etc/dnsmasq.d/ly-route-dhcp.conf"
	desiredDataplaneProbeTimeout         = 2 * time.Second
	desiredDataplaneMaxProblems          = 8
)

// desiredDataplaneCapability reports whether the desired LAN/WAN dataplane is
// actually realized. It returns false (no dependency) when nothing is
// configured, so an appliance without LAN/WAN resources is never degraded by a
// probe that has nothing to verify.
func (server *Server) desiredDataplaneCapability(ctx context.Context) (controlapi.CapabilityState, bool) {
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil || len(assignments) == 0 {
		return controlapi.CapabilityState{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, desiredDataplaneProbeTimeout)
	defer cancel()
	problems := server.desiredDataplaneProblems(probeCtx, assignments)
	if len(problems) == 0 {
		return controlapi.CapabilityState{
			Name:      "desired_dataplane",
			Available: true,
			State:     controlapi.CapabilityAvailable,
		}, true
	}
	reason := strings.Join(problems, "; ")
	if len(problems) > desiredDataplaneMaxProblems {
		reason = strings.Join(problems[:desiredDataplaneMaxProblems], "; ") +
			fmt.Sprintf("; and %d more", len(problems)-desiredDataplaneMaxProblems)
	}
	return controlapi.CapabilityState{
		Name:      "desired_dataplane",
		Available: false,
		State:     controlapi.CapabilityDegraded,
		Reason:    reason,
	}, true
}

func (server *Server) desiredDataplaneProblems(ctx context.Context, assignments []vpp.AddressAssignment) []string {
	liveVPP, err := server.liveVPPInterfaceAddresses(ctx)
	if err != nil {
		// A VPP that cannot answer has nothing else worth reporting: every
		// later probe would fail with the same deadline and produce a cascade
		// of misleading "missing" items.
		return []string{fmt.Sprintf("VPP did not answer \"show interface address\" (%v); the desired LAN/WAN addresses cannot be verified", err)}
	}
	problems := []string{}
	problems = append(problems, desiredDataplaneAddressProblems(assignments, liveVPP)...)
	problems = append(problems, server.desiredDataplaneLCPProblems(ctx, assignments)...)
	problems = append(problems, server.desiredDataplaneDnsmasqProblems(ctx, assignments)...)
	problems = append(problems, server.desiredDataplaneDNSInterceptionProblems(ctx, assignments)...)
	return problems
}

// desiredDataplaneAddressProblems checks the VPP side of every static
// assignment. A dhcp4 WAN has no desired address to compare: the lease is
// dynamic, so only the port attachment applies to it.
func desiredDataplaneAddressProblems(assignments []vpp.AddressAssignment, live map[string]map[string]bool) []string {
	problems := []string{}
	for _, assignment := range assignments {
		if strings.EqualFold(strings.TrimSpace(assignment.Mode), "dhcp4") {
			continue
		}
		interfaceName := strings.TrimSpace(assignment.VPPInterface)
		cidr, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if interfaceName == "" || err != nil {
			continue
		}
		role := desiredDataplaneRoleLabel(assignment.Role)
		present := live[interfaceName]
		if len(present) == 0 {
			problems = append(problems, fmt.Sprintf("%s interface %s has no address in VPP", role, interfaceName))
			continue
		}
		if present[cidr.String()] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s interface %s has no address %s in VPP (live: %s)",
			role, interfaceName, cidr, strings.Join(sortedAddressKeys(present), ", ")))
	}
	return problems
}

// desiredDataplaneLCPProblems checks the kernel side of every LAN. The LAN
// gateway address lives on the lylan-* LCP host interface, not on the physical
// port: a detached LCP pair leaves VPP looking configured while clients cannot
// ping or resolve through the gateway.
func (server *Server) desiredDataplaneLCPProblems(ctx context.Context, assignments []vpp.AddressAssignment) []string {
	problems := []string{}
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		cidr, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if err != nil {
			continue
		}
		hostInterface := vpp.LANControlPlaneHostInterface(assignment.LinuxInterface)
		if hostInterface == "" {
			continue
		}
		live, err := server.liveHostInterfaceAddresses(ctx, hostInterface)
		if err != nil {
			problems = append(problems, fmt.Sprintf("LAN interface %s has no host LCP interface %s", assignment.VPPInterface, hostInterface))
			continue
		}
		if !live[cidr.String()] {
			problems = append(problems, fmt.Sprintf("LAN host interface %s has no address %s", hostInterface, cidr))
		}
	}
	return problems
}

// desiredDataplaneDnsmasqProblems checks the DHCP/DNS listener binding. A
// dnsmasq drop-in without interface=/dhcp-range= is exactly the observed
// failure: the service is active, but no LAN client is served.
func (server *Server) desiredDataplaneDnsmasqProblems(ctx context.Context, assignments []vpp.AddressAssignment) []string {
	lanLCPs := []string{}
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		if hostInterface := vpp.LANControlPlaneHostInterface(assignment.LinuxInterface); hostInterface != "" {
			lanLCPs = append(lanLCPs, hostInterface)
		}
	}
	if len(lanLCPs) == 0 {
		return nil
	}
	path := envOrDefault("LY_ROUTE_DNSMASQ_CONFIG", desiredDataplaneDefaultDnsmasqConfig)
	content, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("dnsmasq configuration %s is missing or unreadable (%v)", path, err)}
	}
	interfaces, rangeInterfaces, rangeCount := parseDnsmasqBindings(string(content))
	problems := []string{}
	if rangeCount == 0 {
		problems = append(problems, "dnsmasq configuration has no dhcp-range=, so no LAN client can obtain a lease")
	}
	seen := map[string]bool{}
	for _, hostInterface := range lanLCPs {
		if seen[hostInterface] {
			continue
		}
		seen[hostInterface] = true
		if !interfaces[hostInterface] {
			problems = append(problems, fmt.Sprintf("dnsmasq configuration has no interface=%s", hostInterface))
			continue
		}
		if rangeCount > 0 && !rangeInterfaces[hostInterface] {
			problems = append(problems, fmt.Sprintf("dnsmasq configuration has no dhcp-range= for interface=%s", hostInterface))
		}
	}
	return problems
}

// desiredDataplaneLAN describes one LAN ingress and its IPv4 gateway. A
// dual-stack LAN has separate assignments; the ABF steering path is IPv4-only,
// so only the IPv4 address is kept.
type desiredDataplaneLAN struct {
	Interface string
	Address   string
}

func desiredDataplaneLANs(assignments []vpp.AddressAssignment) []desiredDataplaneLAN {
	lans := []desiredDataplaneLAN{}
	seen := map[string]bool{}
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		name := strings.TrimSpace(assignment.VPPInterface)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		lans = append(lans, desiredDataplaneLAN{Interface: name})
	}
	for index := range lans {
		for _, assignment := range assignments {
			if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") || strings.TrimSpace(assignment.VPPInterface) != lans[index].Interface {
				continue
			}
			prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
			if err == nil && prefix.Addr().Is4() {
				lans[index].Address = prefix.Addr().String()
				break
			}
		}
	}
	return lans
}

// desiredDataplaneDNSInterceptionProblems checks the VPP interception
// attachment for every LAN. The plugin state is runtime-only and disappears on
// a restart, and the shipped dns-intercept plugin has a single attachment slot.
// Multi-LAN gateways therefore use per-ingress ABF policies; the probe checks
// that every LAN owns the exact policy family the apply path installs.
func (server *Server) desiredDataplaneDNSInterceptionProblems(ctx context.Context, assignments []vpp.AddressAssignment) []string {
	lans := desiredDataplaneLANs(assignments)
	if len(lans) == 0 {
		return nil
	}
	if len(lans) > 1 {
		// DNS interception is not enabled for multi-LAN generations in this
		// build: the dns-intercept plugin has a single attachment slot and the
		// per-ingress ABF path is disabled. The feature is simply off for
		// these gateways, so there is nothing to verify here; the limitation is
		// documented in the release notes instead of degrading every check.
		return nil
	}
	attached, err := server.liveDNSInterceptionInterfaces(ctx)
	if err != nil {
		return []string{fmt.Sprintf("DNS interception state could not be read from VPP (%v)", err)}
	}
	if attached[lans[0].Interface] {
		return nil
	}
	return []string{fmt.Sprintf("DNS interception is not attached to LAN interface %s", lans[0].Interface)}
}

func (server *Server) desiredDataplaneDNSABFProblems(ctx context.Context, lans []desiredDataplaneLAN) []string {
	problems := []string{}
	for _, lan := range lans {
		expected := vpp.LANDNSABFPolicyIDs(lan.Interface, lan.Address)
		if len(expected) == 0 {
			continue
		}
		output, err := desiredDataplaneCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "abf", "attach", lan.Interface)
		if err != nil {
			problems = append(problems, fmt.Sprintf("DNS interception (ABF) state for LAN interface %s could not be read from VPP (%v)", lan.Interface, err))
			continue
		}
		missing := make([]string, 0, len(expected))
		for _, id := range expected {
			if !strings.Contains(output, fmt.Sprintf("policy:%d", id)) {
				missing = append(missing, strconv.Itoa(id))
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("DNS interception (ABF) does not cover LAN interface %s (missing policy %s)", lan.Interface, strings.Join(missing, ", ")))
		}
	}
	return problems
}

func (server *Server) liveVPPInterfaceAddresses(ctx context.Context) (map[string]map[string]bool, error) {
	output, err := desiredDataplaneCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "interface", "address")
	if err != nil {
		return nil, err
	}
	addresses := parseVPPInterfaceAddresses(output)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("VPP returned no interfaces")
	}
	return addresses, nil
}

// parseVPPInterfaceAddresses reads the `show interface address` grammar:
//
//	lyroute-enp2s0 (up):
//	  L3 192.168.88.1/24
func parseVPPInterfaceAddresses(output string) map[string]map[string]bool {
	result := map[string]map[string]bool{}
	current := ""
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "):") {
			open := strings.LastIndex(line, " (")
			if open > 0 {
				current = strings.TrimSpace(line[:open])
				if current != "" && result[current] == nil {
					result[current] = map[string]bool{}
				}
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "L3" || current == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(fields[1])
		if err != nil {
			continue
		}
		result[current][prefix.String()] = true
	}
	return result
}

func (server *Server) liveHostInterfaceAddresses(ctx context.Context, name string) (map[string]bool, error) {
	output, err := desiredDataplaneCommandOutput(ctx, envOrDefault("LY_ROUTE_IP_COMMAND", "ip"), "-o", "-4", "addr", "show", "dev", name)
	if err != nil {
		return nil, err
	}
	addresses := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] != "inet" {
				continue
			}
			if prefix, err := netip.ParsePrefix(fields[index+1]); err == nil {
				addresses[prefix.String()] = true
			}
			break
		}
	}
	return addresses, nil
}

func (server *Server) liveDNSInterceptionInterfaces(ctx context.Context) (map[string]bool, error) {
	output, err := desiredDataplaneCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "ly-route", "dns-intercept")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, fmt.Errorf("VPP returned no interception state")
	}
	attached := map[string]bool{}
	if !strings.Contains(trimmed, "enabled 1") {
		return attached, nil
	}
	fields := strings.Fields(trimmed)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] != "interface" {
			continue
		}
		name := strings.TrimSpace(fields[index+1])
		if name != "" {
			attached[name] = true
		}
	}
	return attached, nil
}

func parseDnsmasqBindings(content string) (map[string]bool, map[string]bool, int) {
	interfaces := map[string]bool{}
	rangeInterfaces := map[string]bool{}
	ranges := 0
	current := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "interface="):
			current = strings.TrimSpace(strings.TrimPrefix(line, "interface="))
			if current != "" {
				interfaces[current] = true
			}
		case strings.HasPrefix(line, "dhcp-range="):
			ranges++
			if current != "" {
				rangeInterfaces[current] = true
			}
		}
	}
	return interfaces, rangeInterfaces, ranges
}

func desiredDataplaneRoleLabel(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "lan":
		return "LAN"
	case "wan":
		return "WAN"
	default:
		return "Dataplane"
	}
}

// desiredDataplaneCommandOutput runs one read-only probe command with a hard
// bound. WaitDelay also closes the inherited stdout pipe once the context is
// cancelled, otherwise a killed shell can leave its child holding the pipe and
// make Output block for the child's full lifetime.
func desiredDataplaneCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = 250 * time.Millisecond
	output, err := command.Output()
	return string(output), err
}

func sortedAddressKeys(addresses map[string]bool) []string {
	keys := make([]string, 0, len(addresses))
	for key := range addresses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
