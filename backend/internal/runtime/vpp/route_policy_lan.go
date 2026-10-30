package vpp

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

// routePolicyLANBinding is the LAN that owns one route policy's source prefix.
//
// The pre-NAT route plugin keeps a single classifier header: one ingress
// interface, one lan-prefix, one feature attachment. A policy therefore has to
// be installed on the LAN its source lives on, and two policies that name
// different LANs cannot both be classified at the same time. Planning and
// readback must agree on this binding or the classifier lands on the wrong
// interface and the apply fails its own readback.
type routePolicyLANBinding struct {
	LinuxInterface string
	VPPInterface   string
	Prefix         netip.Prefix
}

// routePolicyLANBindings lists the IPv4 LAN assignments in a deterministic
// order. Assignments without a resolved VPP interface or CIDR are ignored:
// they cannot own a classifier.
func routePolicyLANBindings(assignments []AddressAssignment) []routePolicyLANBinding {
	bindings := make([]routePolicyLANBinding, 0, len(assignments))
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		vppInterface := strings.TrimSpace(assignment.VPPInterface)
		prefix, err := netip.ParsePrefix(strings.TrimSpace(assignment.CIDR))
		if vppInterface == "" || err != nil || !prefix.Addr().Is4() {
			continue
		}
		bindings = append(bindings, routePolicyLANBinding{
			LinuxInterface: strings.TrimSpace(assignment.LinuxInterface),
			VPPInterface:   vppInterface,
			Prefix:         prefix.Masked(),
		})
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		return bindings[i].Prefix.String() < bindings[j].Prefix.String()
	})
	return bindings
}

// routePolicySourceLANBindings returns every distinct LAN that contains an
// IPv4 source selector of a policy. A deny policy never installs a classifier.
// An any-source policy (or a source outside every configured LAN) returns nil
// and keeps the legacy first-LAN behavior.
//
// Returning all matches (not just the first) lets validation reject a single
// policy whose sources span two LANs: the one classifier header can only carry
// one lan-prefix, so half of that policy would otherwise be silently dead.
func routePolicySourceLANBindings(policy trafficpolicy.RoutePolicy, bindings []routePolicyLANBinding) []routePolicyLANBinding {
	switch strings.ToLower(strings.TrimSpace(policy.Action)) {
	case "deny":
		return nil
	}
	matched := make([]routePolicyLANBinding, 0, 1)
	seen := make(map[string]struct{}, 1)
	for _, source := range policy.Match.Sources {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(source))
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		if prefix.Bits() == 0 {
			// 0.0.0.0/0 is an any-source selector. It does not name a LAN:
			// the classifier may stay on the default LAN and still match the
			// policy, so it must not make the policy look cross-LAN.
			continue
		}
		for _, binding := range bindings {
			if !binding.Prefix.Overlaps(prefix) {
				continue
			}
			if _, found := seen[binding.VPPInterface]; !found {
				seen[binding.VPPInterface] = struct{}{}
				matched = append(matched, binding)
			}
		}
	}
	return matched
}

// routePolicySourceLANBinding finds the LAN that contains the first IPv4
// source selector of a policy. Callers that must reject a policy spanning
// multiple LANs use routePolicySourceLANBindings instead.
func routePolicySourceLANBinding(policy trafficpolicy.RoutePolicy, bindings []routePolicyLANBinding) (routePolicyLANBinding, bool) {
	matched := routePolicySourceLANBindings(policy, bindings)
	if len(matched) == 0 {
		return routePolicyLANBinding{}, false
	}
	return matched[0], true
}

// routePolicySourceOutsideLAN reports the first explicit IPv4 source that
// cannot be reached by any LAN classifier. The classifier is attached to a LAN
// ingress; a policy whose only source is on an unrelated network would be
// installed but never match, so the API rejects it with an actionable error.
// Wildcard sources are the deliberate any-source case and are not reported.
func routePolicySourceOutsideLAN(policy trafficpolicy.RoutePolicy, bindings []routePolicyLANBinding) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(policy.Action)) {
	case "deny":
		return "", false
	}
	for _, source := range policy.Match.Sources {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(source))
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 {
			continue
		}
		inside := false
		for _, binding := range bindings {
			if binding.Prefix.Overlaps(prefix) {
				inside = true
				break
			}
		}
		if !inside {
			return strings.TrimSpace(source), true
		}
	}
	return "", false
}

// routePolicyIngressByID resolves the classifier ingress for every policy that
// names a source inside one of the LANs.
func routePolicyIngressByID(policies []trafficpolicy.RoutePolicy, assignments []AddressAssignment) map[string]string {
	bindings := routePolicyLANBindings(assignments)
	if len(bindings) == 0 {
		return nil
	}
	ingresses := make(map[string]string, len(policies))
	for _, policy := range policies {
		binding, ok := routePolicySourceLANBinding(policy, bindings)
		if !ok {
			continue
		}
		if id := strings.TrimSpace(policy.ID); id != "" {
			ingresses[id] = binding.VPPInterface
		}
	}
	return ingresses
}

// prioritizeRoutePolicyLANPrefix moves the owning LAN prefix to the front of
// a policy's local-destination list. Both the classifier lan-prefix and the
// route readback take the first entry as the policy's LAN, so this keeps the
// installed header and the readback expectation identical.
func prioritizeRoutePolicyLANPrefix(options map[string]routePolicyCommandOptions, policy trafficpolicy.RoutePolicy, bindings []routePolicyLANBinding) {
	id := strings.TrimSpace(policy.ID)
	if id == "" || len(bindings) == 0 {
		return
	}
	binding, ok := routePolicySourceLANBinding(policy, bindings)
	if !ok {
		return
	}
	option := options[id]
	prefix := binding.Prefix.String()
	reordered := make([]string, 0, len(option.localDestinations)+1)
	reordered = append(reordered, prefix)
	for _, candidate := range option.localDestinations {
		if candidate == prefix {
			continue
		}
		reordered = append(reordered, candidate)
	}
	option.localDestinations = reordered
	options[id] = option
}

// ValidateRoutePolicyLANBindings rejects a desired route-policy set that needs
// more than one pre-NAT classifier ingress. The plugin deliberately owns one
// header; installing the second LAN would silently move the feature off the
// first LAN and the first policy would stop matching. Rejecting the set keeps
// the restriction explicit instead of committing a half-classified policy.
//
// A single policy whose source list spans two LANs is rejected for the same
// reason: its one classifier rule can only be installed under one lan-prefix.
func ValidateRoutePolicyLANBindings(policies []trafficpolicy.RoutePolicy, assignments []AddressAssignment) error {
	bindings := routePolicyLANBindings(assignments)
	if len(bindings) == 0 {
		// No LAN inventory to bind against (legacy/migration plan). The
		// operation keeps its placeholder and falls back to the first LAN.
		return nil
	}
	type owner struct {
		policyID string
		prefix   string
		vpp      string
	}
	lanPrefixes := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		lanPrefixes = append(lanPrefixes, binding.Prefix.String())
	}
	var first *owner
	for _, policy := range policies {
		if outside, found := routePolicySourceOutsideLAN(policy, bindings); found {
			return fmt.Errorf(
				"route policy %q source %q is outside every configured LAN (%s): the VPP pre-NAT classifier is attached to a LAN interface and cannot see that traffic; assign the source subnet to a LAN or change the policy source",
				strings.TrimSpace(policy.ID), outside, strings.Join(lanPrefixes, ", "))
		}
		matched := routePolicySourceLANBindings(policy, bindings)
		if len(matched) > 1 {
			return fmt.Errorf(
				"route policy %q selects sources on different LANs (%s on %q and %s on %q): the VPP pre-NAT classifier supports one lan-prefix per appliance, so the policy cannot be classified on both; split it into one policy per LAN or narrow its sources to a single LAN",
				strings.TrimSpace(policy.ID), matched[0].Prefix.String(), matched[0].VPPInterface, matched[1].Prefix.String(), matched[1].VPPInterface)
		}
		if len(matched) == 0 {
			continue
		}
		binding := matched[0]
		if first == nil {
			first = &owner{policyID: strings.TrimSpace(policy.ID), prefix: binding.Prefix.String(), vpp: binding.VPPInterface}
			continue
		}
		if first.vpp == binding.VPPInterface {
			continue
		}
		return fmt.Errorf(
			"route policies %q (%s on %q) and %q (%s on %q) belong to different LANs: the VPP pre-NAT classifier supports one ingress interface per appliance, so both policies cannot be classified at the same time; move the policies onto one LAN or delete one of them",
			first.policyID, first.prefix, first.vpp, strings.TrimSpace(policy.ID), binding.Prefix.String(), binding.VPPInterface)
	}
	return nil
}

// routePolicySteersIntoProxy reports whether a policy's action hands its
// traffic to the proxy service instead of a physical WAN. The egress ID names
// the proxy egress document; the resolved path is the proxy ingress TAP the
// VPP classifier steers into. A caller may supply either, so the same check
// works on raw compiled policies and on path-bound plan policies.
func routePolicySteersIntoProxy(policy trafficpolicy.RoutePolicy, proxyEgressIDs map[string]struct{}) bool {
	switch strings.ToLower(strings.TrimSpace(policy.Action)) {
	case "deny":
		return false
	}
	if _, ok := proxyEgressIDs[strings.TrimSpace(policy.Egress)]; ok {
		return true
	}
	if policy.Path == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(policy.Path.VPPInterface)), "lypxin")
}

// staticNATInternalHost parses the inside address of a 1:1 static mapping.
func staticNATInternalHost(mapping nat.StaticMapping) (netip.Addr, bool) {
	value := strings.TrimSpace(mapping.InternalAddress)
	if host, err := netip.ParseAddr(value); err == nil {
		if !host.Is4() {
			return netip.Addr{}, false
		}
		return host, true
	}
	if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Addr().Is4() {
		return prefix.Masked().Addr(), true
	}
	return netip.Addr{}, false
}

// routePolicyStaticNATHost returns the first static NAT mapping whose internal
// host is selected by one of the policy's explicit IPv4 sources. A wildcard
// source is the deliberate any-source case and never names the NAT host.
func routePolicyStaticNATHost(policy trafficpolicy.RoutePolicy, mappings []nat.StaticMapping) (nat.StaticMapping, string, bool) {
	for _, source := range policy.Match.Sources {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(source))
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 {
			continue
		}
		for _, mapping := range mappings {
			host, ok := staticNATInternalHost(mapping)
			if !ok || !prefix.Contains(host) {
				continue
			}
			return mapping, strings.TrimSpace(source), true
		}
	}
	return nat.StaticMapping{}, "", false
}

// ValidateRoutePolicyStaticNAT rejects a proxy-steering route policy whose
// source selects a host that also owns a 1:1 static NAT mapping.
//
// NAT44 rewrites that host's source to the external VIP in the in2out path.
// The rewrite happens after the pre-NAT classifier has already chosen the
// proxy table, so the packet leaves the physical WAN with the VIP source and
// the proxy silently never sees it: the operator configures proxy steering,
// observes the WAN address and cannot tell why the egress IP is wrong. The two
// features each work on their own, but not for the same host, so the save is
// rejected instead of committing a policy that cannot do what it says.
//
// Only proxy egress targets are checked: a policy that routes to a physical
// WAN keeps its static mapping, which is exactly what 1:1 NAT is for. An
// any-source policy is not a conflict either: it does not name the NAT host,
// and rejecting it would break the common "everything through the proxy"
// setup.
func ValidateRoutePolicyStaticNAT(policies []trafficpolicy.RoutePolicy, mappings []nat.StaticMapping, proxyEgressIDs []string) error {
	if len(mappings) == 0 {
		return nil
	}
	proxy := make(map[string]struct{}, len(proxyEgressIDs))
	for _, id := range proxyEgressIDs {
		if id = strings.TrimSpace(id); id != "" {
			proxy[id] = struct{}{}
		}
	}
	for _, policy := range policies {
		if !routePolicySteersIntoProxy(policy, proxy) {
			continue
		}
		mapping, source, found := routePolicyStaticNATHost(policy, mappings)
		if !found {
			continue
		}
		external := strings.TrimSpace(mapping.ExternalAddress)
		if external == "" {
			external = "the WAN address"
		}
		return fmt.Errorf(
			"route policy %q source %q targets a host with 1:1 static NAT (%s <-> %s); the static mapping rewrites the source before proxy steering, so the proxy would silently not apply; remove the static NAT mapping or use a different host",
			strings.TrimSpace(policy.ID), source, strings.TrimSpace(mapping.InternalAddress), external)
	}
	return nil
}
