package vpp

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

func policyLANTestAssignments() []AddressAssignment {
	return []AddressAssignment{
		// Deliberately ordered like the live plan: lab20 first, then lab10.
		{ID: "eth1", LinuxInterface: "eth1", VPPInterface: "lyroute-ens224", CIDR: "192.168.20.1/24", Role: "lan"},
		{ID: "eth4", LinuxInterface: "eth4", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"},
		{ID: "eth2", LinuxInterface: "eth2", VPPInterface: "lyroute-lab20", CIDR: "192.168.30.1/24", Role: "lan"},
		{ID: "wan1", LinuxInterface: "ens192", VPPInterface: "lyroute-ens192", CIDR: "10.99.0.1/24", Role: "wan"},
	}
}

func policyLANTestPolicy(id, source string, priority int) trafficpolicy.RoutePolicy {
	return trafficpolicy.RoutePolicy{
		ID:       id,
		Priority: priority,
		Action:   "route",
		NextHop:  "10.99.0.99",
		Match: trafficpolicy.Match{
			Sources:      []string{source},
			Destinations: []string{"10.99.0.2/32"},
			Protocols:    []string{"any"},
		},
	}
}

func routePolicyCommandLines(operations []Operation, id string) string {
	for _, operation := range operations {
		if operation.Name == "vpp.route-policy" && operation.Resource == id {
			return strings.Join(operation.VPPCtlCommands, "\n")
		}
	}
	return ""
}

// The incremental route path (BuildRouteWANGroupOperations) is the one a live
// apply uses. It must bind the classifier to the LAN that owns the policy
// source, not to the first LAN in the plan.
func TestRouteWANGroupOperationsBindClassifierToPolicySourceLAN(t *testing.T) {
	policy := policyLANTestPolicy("uiacc-route", "192.168.10.10/32", 960)
	operations, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:       "txn-policy-lan",
		IngressVPPInterface: "lyroute-ens224",
		AddressAssignments:  policyLANTestAssignments(),
		Routes:              []trafficpolicy.RoutePolicy{policy},
		RoutePolicyContext:  []trafficpolicy.RoutePolicy{policy},
	})
	if err != nil {
		t.Fatalf("BuildRouteWANGroupOperations() error = %v", err)
	}
	commands := routePolicyCommandLines(operations, policy.ID)
	if commands == "" {
		t.Fatal("route policy operation is missing")
	}
	wantHeader := "set ly-route pre-nat-route interface lyroute-ens256 lan-prefix 192.168.10.0/24"
	if !strings.Contains(commands, wantHeader) {
		t.Fatalf("classifier is not bound to the policy source LAN %q:\n%s", wantHeader, commands)
	}
	if strings.Contains(commands, "interface lyroute-ens224") {
		t.Fatalf("classifier still references the first LAN:\n%s", commands)
	}
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	wantRule := "set ly-route pre-nat-route add id " + strconv.Itoa(policyID) + " priority " + strconv.Itoa(vppABFPriority(policy.Priority)) + " source 192.168.10.10/32 destination 10.99.0.2/32"
	if !strings.Contains(commands, wantRule) {
		t.Fatalf("classifier rule is missing %q:\n%s", wantRule, commands)
	}
	if !strings.Contains(commands, "table "+strconv.Itoa(tableID)) {
		t.Fatalf("classifier rule does not select table %d:\n%s", tableID, commands)
	}
}

func TestRoutePolicyLANPrefixFollowsSourceLAN(t *testing.T) {
	policy := policyLANTestPolicy("lab20-route", "192.168.20.0/24", 10)
	options := buildRoutePolicyCommandOptions([]trafficpolicy.RoutePolicy{policy}, nil)
	addRoutePolicyLocalDestinations(options, []trafficpolicy.RoutePolicy{policy}, policyLANTestAssignments())
	plan, ok := compileRoutePolicyRadixPlan(policy, nil, options[policy.ID])
	if !ok {
		t.Fatal("radix plan was not compiled")
	}
	if want := netip.MustParsePrefix("192.168.20.0/24"); plan.lanPrefix != want {
		t.Fatalf("radix lanPrefix = %s, want the policy's own LAN %s", plan.lanPrefix, want)
	}
}

func TestRoutePolicyReadbackChecksPerPolicyLAN(t *testing.T) {
	policy := policyLANTestPolicy("uiacc-route", "192.168.10.10/32", 960)
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	plan := routePolicyRadixPlan{lanPrefix: netip.MustParsePrefix("192.168.10.0/24"), routeTarget: "10.99.0.99", ruleCount: 1}
	rule := "rule id " + strconv.Itoa(policyID) + " priority 255 prefixes 1 table " + strconv.Itoa(tableID) + " fib-index 9 skip-nat 0 bypass 0"
	request := SnapshotRequest{RoutePolicyLANInterfaces: map[string]string{policy.ID: "lyroute-ens256"}}

	wrongPrefix := "enabled 1 interface lyroute-ens256 lan-prefix 192.168.20.0/24 rules 2 radix-nodes 1\n" + rule + "\n"
	if present, err := verifyRoutePolicyRadixReadback(wrongPrefix, request, policy, policyID, tableID, plan); err != nil || present {
		t.Fatalf("readback accepted the wrong lan-prefix: present=%t err=%v", present, err)
	}

	wrongInterface := "enabled 1 interface lyroute-ens224 lan-prefix 192.168.10.0/24 rules 2 radix-nodes 1\n" + rule + "\n"
	if present, err := verifyRoutePolicyRadixReadback(wrongInterface, request, policy, policyID, tableID, plan); err != nil || present {
		t.Fatalf("readback accepted the wrong ingress interface: present=%t err=%v", present, err)
	}

	correct := "enabled 1 interface lyroute-ens256 lan-prefix 192.168.10.0/24 rules 2 radix-nodes 1\n" + rule + "\n"
	if present, err := verifyRoutePolicyRadixReadback(correct, request, policy, policyID, tableID, plan); err != nil || !present {
		t.Fatalf("readback rejected the matching classifier: present=%t err=%v", present, err)
	}
}

func TestCrossLANRoutePoliciesAreRejected(t *testing.T) {
	assignments := policyLANTestAssignments()
	policies := []trafficpolicy.RoutePolicy{
		policyLANTestPolicy("policy-lab10", "192.168.10.10/32", 960),
		policyLANTestPolicy("policy-lab20", "192.168.20.10/32", 961),
	}
	if err := ValidateRoutePolicyLANBindings(policies, assignments); err == nil {
		t.Fatal("two policies on different LANs must be rejected")
	} else if !strings.Contains(err.Error(), "different LANs") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if _, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:      "txn-cross-lan",
		AddressAssignments: assignments,
		Routes:             policies,
		RoutePolicyContext: policies,
	}); err == nil {
		t.Fatal("BuildRouteWANGroupOperations accepted a cross-LAN policy set")
	}

	// The same LAN is fine even when the source prefixes differ.
	sameLAN := []trafficpolicy.RoutePolicy{
		policyLANTestPolicy("policy-lab10-a", "192.168.10.10/32", 960),
		policyLANTestPolicy("policy-lab10-b", "192.168.10.11/32", 961),
	}
	if err := ValidateRoutePolicyLANBindings(sameLAN, assignments); err != nil {
		t.Fatalf("same-LAN policies must be accepted: %v", err)
	}
}

func TestRoutePolicyCrossLANSourceInOnePolicyIsRejected(t *testing.T) {
	assignments := policyLANTestAssignments()
	policy := policyLANTestPolicy("policy-spanning", "192.168.10.10/32", 960)
	policy.Match.Sources = []string{"192.168.10.10/32", "192.168.20.10/32"}
	err := ValidateRoutePolicyLANBindings([]trafficpolicy.RoutePolicy{policy}, assignments)
	if err == nil {
		t.Fatal("a single policy with sources on two LANs must be rejected")
	} else if !strings.Contains(err.Error(), "selects sources on different LANs") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if _, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:      "txn-spanning-lan",
		AddressAssignments: assignments,
		Routes:             []trafficpolicy.RoutePolicy{policy},
		RoutePolicyContext: []trafficpolicy.RoutePolicy{policy},
	}); err == nil {
		t.Fatal("BuildRouteWANGroupOperations accepted a policy spanning two LANs")
	}
}

func TestRouteWANGroupSnapshotRequestCarriesPolicyLANBinding(t *testing.T) {
	policy := policyLANTestPolicy("uiacc-route", "192.168.10.10/32", 960)
	request := routeWANGroupSnapshotRequestForPlan(RouteWANGroupPlan{
		TransactionID:      "txn-policy-lan-snapshot",
		AddressAssignments: policyLANTestAssignments(),
		Routes:             []trafficpolicy.RoutePolicy{policy},
		RoutePolicyContext: []trafficpolicy.RoutePolicy{policy},
	})
	if got, want := request.RoutePolicyLANInterfaces[policy.ID], "lyroute-ens256"; got != want {
		t.Fatalf("readback ingress for %q = %q, want %q (map=%v)", policy.ID, got, want, request.RoutePolicyLANInterfaces)
	}
	if len(request.LANAddressAssignments) != len(policyLANTestAssignments()) {
		t.Fatalf("readback LAN assignments = %#v, want the full LAN inventory", request.LANAddressAssignments)
	}
}

func TestDecodeRoutesUsesPolicySourceLANForReadback(t *testing.T) {
	policy := policyLANTestPolicy("uiacc-route", "192.168.10.10/32", 960)
	policy.NextHop = "10.99.0.4"
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	// LANVPPInterface/LocalDestinations describe the old first-LAN assumption:
	// the plan was classified on lyroute-ens224/192.168.20.0/24. The policy is
	// actually on LAN10, so only the per-policy binding may satisfy readback.
	request := SnapshotRequest{
		LANVPPInterface:          "lyroute-ens224",
		LocalDestinations:        []string{"192.168.20.0/24", "192.168.30.0/24"},
		RoutePolicies:            []string{policy.ID},
		Candidates:               SnapshotCandidates{RoutePolicies: []trafficpolicy.RoutePolicy{policy}},
		LANAddressAssignments:    policyLANTestAssignments(),
		RoutePolicyLANInterfaces: map[string]string{policy.ID: "lyroute-ens256"},
	}
	fib := fmt.Sprintf("ipv4-VRF:%d, fib_index:9, flow hash:[src dst sport dport proto]\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: ipv4 via 0.0.0.0 10.99.0.4\n", tableID)
	readback, err := decodeVPPCTLRoutes(request, []VPPCTLCommandResult{
		{Command: "show acl-plugin acl", Stdout: ""},
		{Command: "show ly-route pre-nat-route", Stdout: fmt.Sprintf("enabled 1 interface lyroute-ens256 lan-prefix 192.168.10.0/24 rules 2 radix-nodes 1\nrule id %d priority 255 prefixes 1 table %d fib-index 9 skip-nat 0 bypass 0\n", policyID, tableID)},
		{Command: fmt.Sprintf("show abf policy %d", policyID), Stdout: fmt.Sprintf("Invalid policy ID:%d\n", policyID)},
		{Command: fmt.Sprintf("show ip fib table %d", tableID), Stdout: fib},
	})
	if err != nil {
		t.Fatalf("readback rejected the classifier bound to the policy source LAN: %v", err)
	}
	if len(readback.Policies) != 1 || readback.Policies[0].ID != policy.ID {
		t.Fatalf("policies = %#v, want %s", readback.Policies, policy.ID)
	}

	// The old first-LAN classifier is not accepted: the error must name the
	// expected lan-prefix and interface so an operator can act on it.
	_, err = decodeVPPCTLRoutes(request, []VPPCTLCommandResult{
		{Command: "show ly-route pre-nat-route", Stdout: fmt.Sprintf("enabled 1 interface lyroute-ens224 lan-prefix 192.168.20.0/24 rules 2 radix-nodes 1\nrule id %d priority 255 prefixes 1 table %d fib-index 9 skip-nat 0 bypass 0\n", policyID, tableID)},
	})
	if err == nil {
		t.Fatal("readback accepted a classifier on the wrong LAN")
	}
	for _, want := range []string{`expected lan-prefix 192.168.10.0/24`, `on "lyroute-ens256"`, `observed "enabled 1 interface lyroute-ens224`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("readback error %q does not contain %q", err, want)
		}
	}
}

func TestRoutePolicySourceOutsideEveryLANIsRejected(t *testing.T) {
	assignments := policyLANTestAssignments()
	policy := policyLANTestPolicy("policy-external", "203.0.113.10/32", 960)
	err := ValidateRoutePolicyLANBindings([]trafficpolicy.RoutePolicy{policy}, assignments)
	if err == nil {
		t.Fatal("a policy whose source is outside every LAN must be rejected")
	} else if !strings.Contains(err.Error(), "outside every configured LAN") || !strings.Contains(err.Error(), "203.0.113.10/32") {
		t.Fatalf("validation error is not actionable: %v", err)
	}
	if _, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:      "txn-outside-lan",
		AddressAssignments: assignments,
		Routes:             []trafficpolicy.RoutePolicy{policy},
		RoutePolicyContext: []trafficpolicy.RoutePolicy{policy},
	}); err == nil {
		t.Fatal("BuildRouteWANGroupOperations accepted a source outside every LAN")
	}

	// An explicit any-source policy is valid: it stays on the default LAN and
	// must not be mistaken for a cross-LAN or outside-LAN policy.
	anySource := policyLANTestPolicy("policy-any", "0.0.0.0/0", 1000)
	if err := ValidateRoutePolicyLANBindings([]trafficpolicy.RoutePolicy{anySource}, assignments); err != nil {
		t.Fatalf("any-source policy must remain valid: %v", err)
	}

	// A prefix that spans two LANs is still cross-LAN even though its first
	// address looks like it belongs to only one of them.
	spanning := policyLANTestPolicy("policy-spanning-prefix", "192.168.0.0/16", 1001)
	if err := ValidateRoutePolicyLANBindings([]trafficpolicy.RoutePolicy{spanning}, assignments); err == nil {
		t.Fatal("a source prefix overlapping two LANs must be rejected")
	}
}

func TestRouteWANGroupSnapshotRequestMapsUnchangedContextPolicy(t *testing.T) {
	changed := policyLANTestPolicy("policy-changed", "192.168.20.10/32", 960)
	unchanged := policyLANTestPolicy("policy-unchanged", "192.168.20.11/32", 961)
	request := routeWANGroupSnapshotRequestForPlan(RouteWANGroupPlan{
		TransactionID:      "txn-context-lan",
		AddressAssignments: policyLANTestAssignments(),
		Routes:             []trafficpolicy.RoutePolicy{changed},
		RoutePolicyContext: []trafficpolicy.RoutePolicy{changed, unchanged},
	})
	for _, policy := range []trafficpolicy.RoutePolicy{changed, unchanged} {
		if got, want := request.RoutePolicyLANInterfaces[policy.ID], "lyroute-ens224"; got != want {
			t.Fatalf("readback ingress for context policy %q = %q, want %q (map=%v)", policy.ID, got, want, request.RoutePolicyLANInterfaces)
		}
	}
}

func TestRoutePolicyDeleteUsesPolicySourceLAN(t *testing.T) {
	policy := policyLANTestPolicy("uiacc-route", "192.168.10.10/32", 960)
	operations, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:      "txn-delete-lan",
		AddressAssignments: policyLANTestAssignments(),
		Routes:             []trafficpolicy.RoutePolicy{policy},
		RoutePolicyContext: []trafficpolicy.RoutePolicy{policy},
		DeleteRoutes:       []string{policy.ID},
		DeleteRouteState:   []trafficpolicy.RoutePolicy{policy},
	})
	if err != nil {
		t.Fatalf("BuildRouteWANGroupOperations() error = %v", err)
	}
	for _, operation := range operations {
		commands := strings.Join(operation.VPPCtlCommands, "\n")
		if !strings.Contains(commands, "abf attach ip4 del policy") {
			continue
		}
		if !strings.Contains(commands, "lyroute-ens256") {
			t.Fatalf("delete detached the policy from the wrong LAN:\n%s", commands)
		}
		if strings.Contains(commands, "lyroute-ens224") {
			t.Fatalf("delete still references the default LAN:\n%s", commands)
		}
		return
	}
	t.Fatal("route policy delete operation is missing")
}

func TestParseFIBResultAcceptsNestedLoadBalance(t *testing.T) {
	tableID := 50910
	output := fmt.Sprintf("ipv4-VRF:%d, fib_index:2, flow hash:[src dst sport dport proto ]\n0.0.0.0/0\n  unicast-ip4-chain\n  [@0]: dpo-load-balance: [proto:ip4 index:53 buckets:1 uRPF:79 to:[0:0]]\n    [0] [@14]: dpo-load-balance: [proto:ip4 index:59 buckets:1 uRPF:77 to:[6:504]]\n          [0] [@5]: ipv4 via 10.99.0.4 lyroute-ens224: mtu:9000 next:11 flags:[features ] 0050563a40220050563a15210800\n", tableID)
	paths, err := parseFIBResult([]VPPCTLCommandResult{{Command: fmt.Sprintf("show ip fib table %d", tableID), Stdout: output}}, tableID)
	if err != nil {
		t.Fatalf("parseFIBResult() rejected VPP nested load-balance output: %v", err)
	}
	if len(paths) != 1 || !strings.Contains(paths[0].via, "10.99.0.4") {
		t.Fatalf("paths = %#v, want the leaf via 10.99.0.4", paths)
	}
}

func staticNATTestMappings() []nat.StaticMapping {
	return []nat.StaticMapping{{
		ID:              "nat-static-kurumi-vip",
		InternalAddress: "192.168.88.2",
		ExternalAddress: "10.1.18.246",
	}}
}

func proxySteeringTestPolicy(id, source, egress string) trafficpolicy.RoutePolicy {
	return trafficpolicy.RoutePolicy{
		ID:       id,
		Priority: 960,
		Action:   "route",
		Egress:   egress,
		Match: trafficpolicy.Match{
			Sources:      []string{source},
			Destinations: []string{"34.160.111.145/32"},
			Protocols:    []string{"any"},
		},
	}
}

// The reproduced field bug: a 1:1 static NAT mapping rewrites the source of
// 192.168.88.2 to the WAN VIP before the proxy steering takes effect, so the
// proxy silently never applies. The save must be rejected with an error that
// names the host and the mapping.
func TestProxyPolicySourceOfStaticNATHostIsRejected(t *testing.T) {
	policy := proxySteeringTestPolicy("proxy-kurumi", "192.168.88.2/32", "proxy-egress-kurumi")
	err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, staticNATTestMappings(), []string{"proxy-egress-kurumi"})
	if err == nil {
		t.Fatal("a proxy policy whose source is the static NAT host must be rejected")
	}
	for _, want := range []string{
		`route policy "proxy-kurumi"`,
		`source "192.168.88.2/32"`,
		`targets a host with 1:1 static NAT (192.168.88.2 <-> 10.1.18.246)`,
		"the proxy would silently not apply",
		"remove the static NAT mapping or use a different host",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error %q does not contain %q", err, want)
		}
	}
}

// A larger source prefix that still selects the NAT host is the same conflict:
// the classifier matches that host and the NAT rewrite still wins.
func TestProxyPolicySourcePrefixCoveringStaticNATHostIsRejected(t *testing.T) {
	for _, source := range []string{"192.168.88.0/24", "192.168.0.0/16", "192.168.88.0/23"} {
		policy := proxySteeringTestPolicy("proxy-covered", source, "proxy-egress-kurumi")
		err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, staticNATTestMappings(), []string{"proxy-egress-kurumi"})
		if err == nil {
			t.Fatalf("proxy policy source %s covers the static NAT host and must be rejected", source)
		}
		if !strings.Contains(err.Error(), "targets a host with 1:1 static NAT (192.168.88.2 <-> 10.1.18.246)") {
			t.Fatalf("validation error for %s is not actionable: %v", source, err)
		}
	}
}

// Any-source and unrelated-source policies are not conflicts: only a policy
// that actually matches the static NAT host loses its traffic to the rewrite.
func TestProxyPolicySourcesWithoutStaticNATConflictAreAllowed(t *testing.T) {
	cases := map[string]trafficpolicy.RoutePolicy{
		"implicit any": {ID: "proxy-any", Action: "route", Egress: "proxy-egress-kurumi", Match: trafficpolicy.Match{Destinations: []string{"0.0.0.0/0"}}},
		"explicit any": proxySteeringTestPolicy("proxy-any-explicit", "0.0.0.0/0", "proxy-egress-kurumi"),
		"unrelated":    proxySteeringTestPolicy("proxy-other", "192.168.88.3/32", "proxy-egress-kurumi"),
		"other subnet": proxySteeringTestPolicy("proxy-lab10", "192.168.10.0/24", "proxy-egress-kurumi"),
		"deny":         {ID: "deny-kurumi-host", Action: "deny", Egress: "proxy-egress-kurumi", Match: trafficpolicy.Match{Sources: []string{"192.168.88.2/32"}}},
	}
	for name, policy := range cases {
		if err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, staticNATTestMappings(), []string{"proxy-egress-kurumi"}); err != nil {
			t.Fatalf("%s policy must stay valid: %v", name, err)
		}
	}
}

// A policy routed out a physical WAN keeps its 1:1 static NAT: that pairing is
// exactly what the static mapping is for.
func TestWANPolicyForStaticNATHostIsAllowed(t *testing.T) {
	policy := proxySteeringTestPolicy("wan-route", "192.168.88.2/32", "wan-pppoe-1")
	if err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, staticNATTestMappings(), []string{"proxy-egress-kurumi"}); err != nil {
		t.Fatalf("a WAN policy must not be rejected by the proxy/static-NAT rule: %v", err)
	}

	// The same policy on a WAN group is also a WAN target.
	group := proxySteeringTestPolicy("wan-group-route", "192.168.88.2/32", "wan-group-primary-backup")
	if err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{group}, staticNATTestMappings(), []string{"proxy-egress-kurumi"}); err != nil {
		t.Fatalf("a WAN-group policy must not be rejected by the proxy/static-NAT rule: %v", err)
	}
}

// Removing the static NAT mapping removes the conflict: the identical policy
// saves and the proxy works again.
func TestProxyPolicyIsAllowedAfterStaticNATRemoval(t *testing.T) {
	policy := proxySteeringTestPolicy("proxy-kurumi", "192.168.88.2/32", "proxy-egress-kurumi")
	if err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, nil, []string{"proxy-egress-kurumi"}); err != nil {
		t.Fatalf("the policy must be accepted once the static mapping is gone: %v", err)
	}
	if err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, []nat.StaticMapping{}, []string{"proxy-egress-kurumi"}); err != nil {
		t.Fatalf("the policy must be accepted once the static mapping is gone: %v", err)
	}
}

// The compiled plan path detects proxy steering from the resolved ingress TAP
// as well, so desired state written by an older build cannot skip the check.
func TestPlanBoundProxyPolicyIsRejectedByStaticNAT(t *testing.T) {
	policy := proxySteeringTestPolicy("proxy-bound", "192.168.88.2/32", "")
	policy.Path = &trafficpolicy.WANPath{VPPInterface: "lypxin7f3a", NextHop: "169.254.77.1"}
	err := ValidateRoutePolicyStaticNAT([]trafficpolicy.RoutePolicy{policy}, staticNATTestMappings(), nil)
	if err == nil {
		t.Fatal("a policy bound to the proxy ingress TAP must be rejected")
	}
	if !strings.Contains(err.Error(), "1:1 static NAT") {
		t.Fatalf("validation error is not actionable: %v", err)
	}
}
