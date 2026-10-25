package vpp

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// A WAN-group route has no single WANPath. It must still be installed through
// the native pre-NAT classifier: the legacy ABF runs after NAT has rewritten
// the LAN source, so its source match can never succeed and the group table
// would never carry LAN traffic.
func TestWANGroupRouteInstallsNativePreNATClassifier(t *testing.T) {
	group := trafficpolicy.WANGroup{
		ID:            "wan-group",
		Mode:          trafficpolicy.WANGroupPrimaryBackup,
		Members:       []string{"wan-a", "wan-b"},
		PrimaryMember: "wan-a",
		BackupMember:  "wan-b",
		Weights:       map[string]int{"wan-a": 1, "wan-b": 1},
		Paths: map[string]trafficpolicy.WANPath{
			"wan-a": {VPPInterface: "lyroute-enp0s3", NextHop: "100.77.0.1"},
			"wan-b": {VPPInterface: "lyroute-enp0s5", NextHop: "100.78.0.1"},
		},
	}
	route := trafficpolicy.RoutePolicy{
		ID:       "wan-group-default",
		Priority: 150,
		Action:   "nat",
		Egress:   "wan-group",
		Match: trafficpolicy.Match{
			Sources:      []string{"192.168.77.0/24"},
			Destinations: []string{"0.0.0.0/0"},
			Protocols:    []string{"any"},
		},
	}
	if !routePolicySupportsNativePreNAT(route) {
		t.Fatal("WAN-group route must be eligible for the native pre-NAT classifier")
	}

	operations, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:       "txn-wan-group",
		IngressVPPInterface: "lyroute-enp0s4",
		Routes:              []trafficpolicy.RoutePolicy{route},
		WANGroups:           []trafficpolicy.WANGroup{group},
	})
	if err != nil {
		t.Fatal(err)
	}
	var routeOperation Operation
	var groupOperation Operation
	for _, operation := range operations {
		switch {
		case operation.Name == "vpp.route-policy" && operation.Resource == route.ID:
			routeOperation = operation
		case operation.Name == "vpp.pbr.next-hop-group" && operation.Resource == group.ID:
			groupOperation = operation
		}
	}
	if routeOperation.Name == "" {
		t.Fatal("route operation is missing")
	}
	if groupOperation.Name == "" {
		t.Fatal("WAN-group operation is missing")
	}
	if indexOfOperation(operations, groupOperation.Name, group.ID) > indexOfOperation(operations, routeOperation.Name, route.ID) {
		t.Fatal("WAN-group table must be installed before the route that looks it up")
	}

	policyID := stableID("route-abf:"+route.ID, 10000, 8999)
	tableID := stableID("route-table:"+route.ID, 50000, 49999)
	groupTableID := wanGroupTableID(group.ID)
	joined := strings.Join(routeOperation.VPPCtlCommands, "\n")
	chain := fmt.Sprintf("ip route add table %d 0.0.0.0/0 via ip4-lookup-in-table %d", tableID, groupTableID)
	if !strings.Contains(joined, chain) {
		t.Fatalf("route private FIB does not chain into the WAN-group table %q:\n%s", chain, joined)
	}

	spec, err := parseRoutePolicyVPPCTLSpec(routeOperation)
	if err != nil {
		t.Fatal(err)
	}
	commands, applied, err := buildPreNATRoutePolicyCommands(spec, netip.MustParsePrefix("192.168.77.2/24"))
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("native pre-NAT classifier was not generated for the WAN-group route")
	}
	wantRule := fmt.Sprintf("set ly-route pre-nat-route add id %d priority 150 source 192.168.77.0/24 destination 0.0.0.0/0 protocol any sport 0-65535 dport 0-65535 table %d",
		policyID, tableID)
	if !strings.Contains(strings.Join(commands, "\n"), wantRule) {
		t.Fatalf("pre-NAT classifier does not select the route FIB %q:\n%s", wantRule, strings.Join(commands, "\n"))
	}
}

func TestNativePreNATRequiresEgress(t *testing.T) {
	base := trafficpolicy.RoutePolicy{
		ID:       "no-egress",
		Priority: 100,
		Action:   "nat",
		Match: trafficpolicy.Match{
			Sources:      []string{"192.168.77.0/24"},
			Destinations: []string{"0.0.0.0/0"},
		},
	}
	if routePolicySupportsNativePreNAT(base) {
		t.Fatal("a path-less policy with no egress must not claim the native classifier")
	}
	denied := base
	denied.Action = "deny"
	denied.Egress = "wan-group"
	if routePolicySupportsNativePreNAT(denied) {
		t.Fatal("a deny policy must not own a pre-NAT classifier")
	}
}

func indexOfOperation(operations []Operation, name, resource string) int {
	for index, operation := range operations {
		if operation.Name == name && operation.Resource == resource {
			return index
		}
	}
	return -1
}
