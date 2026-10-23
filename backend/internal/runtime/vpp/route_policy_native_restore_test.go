package vpp

import (
	"fmt"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

func TestDecodeRoutePolicyAcceptsNativeClassifierDuringRestore(t *testing.T) {
	policy := trafficpolicy.RoutePolicy{
		ID:       "native-restore",
		Priority: 100,
		Action:   "route",
		Match: trafficpolicy.Match{
			Sources:      []string{"192.0.2.10/32"},
			Destinations: []string{"0.0.0.0/0"},
		},
	}
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	request := SnapshotRequest{
		RoutePolicies:     []string{policy.ID},
		LocalDestinations: []string{"192.168.50.0/24"},
		Candidates:        SnapshotCandidates{RoutePolicies: []trafficpolicy.RoutePolicy{policy}},
	}
	results := []VPPCTLCommandResult{
		{Command: "show acl-plugin acl", Stdout: ""},
		{Command: "show ly-route pre-nat-route", Stdout: fmt.Sprintf("enabled 1 interface lyroute-ens34 lan-prefix 192.168.50.0/24 rules 1 radix-nodes 1\nrule id %d priority 100 prefixes 1 table %d fib-index 12 skip-nat 0 bypass 0\n", policyID, tableID)},
		{Command: fmt.Sprintf("show abf policy %d", policyID), Stdout: fmt.Sprintf("Invalid policy ID:%d\n", policyID)},
		{Command: fmt.Sprintf("show ip fib table %d", tableID), Stdout: fmt.Sprintf("ipv4-VRF:%d, fib_index:12\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: ipv4 via 0.0.0.0 pppoe_session1\n", tableID)},
	}
	readback, err := decodeVPPCTLRoutes(request, results)
	if err != nil {
		t.Fatalf("native classifier restore readback: %v", err)
	}
	if len(readback.Policies) != 1 || readback.Policies[0].ID != policy.ID {
		t.Fatalf("policies = %#v, want %s", readback.Policies, policy.ID)
	}
}

func TestDecodeRoutePolicyFallsBackWhenPreNATReadbackIsUnsupported(t *testing.T) {
	results := []VPPCTLCommandResult{{Command: "show ly-route pre-nat-route", Stdout: "unsupported command", Retval: 64}}
	if !routePolicyPreNATReadbackUnavailable(results) {
		t.Fatal("unsupported pre-NAT command must be treated as unavailable")
	}
	if routePolicyPreNATReadbackUnavailable([]VPPCTLCommandResult{{Command: "show ly-route pre-nat-route", Stdout: ""}}) {
		t.Fatal("empty pre-NAT inventory must not be treated as unsupported")
	}
}
