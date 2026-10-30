package vpp

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// Restoring the classifier header must run as plain vppctl commands. Naming
// the operation vpp.route-policy.* sends it into the ABF/pre-NAT policy
// lifecycle, which requires a typed policy and fails the rollback.
func TestPreNATClassifierRestoreRunsAsPlainCommands(t *testing.T) {
	state := &PreNATClassifierState{Enabled: true, InterfaceName: "lyroute-ens256", LANPrefix: "192.168.10.0/24", RuleCount: 2}
	channel := &rollbackTestChannel{classifier: state}
	if err := (Adapter{Client: &rollbackTestClient{channel: channel}}).RestorePreNATClassifier(context.Background(), "txn-classifier-restore", state); err != nil {
		t.Fatalf("restore classifier: %v", err)
	}
	var restored bool
	for _, operation := range channel.operations {
		if strings.HasPrefix(operation.Name, "vpp.route-policy") {
			t.Fatalf("classifier restore uses the route-policy lifecycle: %q", operation.Name)
		}
		if strings.Contains(strings.Join(operation.VPPCtlCommands, "\n"), "set ly-route pre-nat-route interface lyroute-ens256 lan-prefix 192.168.10.0/24") {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("classifier header was not restored: %#v", channel.operations)
	}
}

// Rollback cleanup must never tear down an interface the previous generation
// also owned. Doing so removed the live LAN address before RollbackRestore
// could replay it, which is the addressless window this rollback prevents.
func TestInterfaceRollbackCleanupKeepsSurvivingInterface(t *testing.T) {
	prior := []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.1/24"}}}
	plan := InterfaceBondPlan{
		TransactionID: "txn-rollback-window",
		Interfaces:    []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.254/24"}}},
	}

	operations, err := BuildInterfaceRollbackCleanupOperations(plan, prior)
	if err != nil {
		t.Fatalf("rollback cleanup: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("surviving interface was queued for rollback teardown: %#v", operations)
	}

	// An interface this generation created is still removed, because rollback
	// replays the previous generation and never recreates it.
	plan.Interfaces = append(plan.Interfaces, InterfaceState{Name: "lyroute-lab20", AdminState: "up", Addresses: []string{"192.168.20.1/24"}})
	operations, err = BuildInterfaceRollbackCleanupOperations(plan, prior)
	if err != nil {
		t.Fatalf("rollback cleanup: %v", err)
	}
	if len(operations) != 1 || operations[0].Resource != "lyroute-lab20" {
		t.Fatalf("introduced interface was not torn down: %#v", operations)
	}
	if !strings.Contains(strings.Join(operations[0].VPPCtlCommands, "\n"), "set interface state lyroute-lab20 down") {
		t.Fatalf("introduced interface teardown is incomplete: %#v", operations[0].VPPCtlCommands)
	}
}

// When the interface node itself fails, its own rollback must replay the
// previous address before removing the address the failed generation added.
// The old code deleted first (and brought the interface down), producing a
// window with no LAN address at all.
func TestInterfaceFailureRollbackReplaysPriorBeforeDeletingNewAddress(t *testing.T) {
	prior := Snapshot{Interfaces: []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.1/24"}}}}
	current := InterfaceBondPlan{
		TransactionID: "txn-interface-window",
		Interfaces:    []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.254/24"}}},
	}
	channel := &rollbackTestChannel{interfaces: prior.Interfaces}
	err := (Adapter{Client: &rollbackTestClient{channel: channel}}).interfaceBondFailure(context.Background(), channel, current.TransactionID, "readback", errRollbackTestFailure, prior, current)
	if err == nil {
		t.Fatal("expected the lifecycle error")
	}
	lifecycle, ok := err.(*InterfaceBondLifecycleError)
	if !ok {
		t.Fatalf("error type = %T, want *InterfaceBondLifecycleError", err)
	}
	if lifecycle.RollbackResult != RollbackSucceeded || lifecycle.Rollback != nil {
		t.Fatalf("rollback reported %s: %v", lifecycle.RollbackResult, lifecycle.Rollback)
	}

	joined := joinedCommands(channel.operations)
	addPrior := strings.Index(joined, "set interface ip address lyroute-ens256 192.168.10.1/24")
	deleteNew := strings.Index(joined, "ip address del lyroute-ens256 192.168.10.254/24")
	if addPrior < 0 {
		t.Fatalf("rollback did not replay the previous LAN address:\n%s", joined)
	}
	if deleteNew < 0 {
		t.Fatalf("rollback did not remove the address the failed generation added:\n%s", joined)
	}
	if addPrior > deleteNew {
		t.Fatalf("rollback deletes before it builds:\n%s", joined)
	}
	if strings.Contains(joined, "set interface state lyroute-ens256 down") {
		t.Fatalf("rollback brought the surviving LAN interface down:\n%s", joined)
	}
}

// RemoveStaleInterfaceAddresses is the post-restore step: it only ever deletes
// addresses the failed generation introduced, and never touches admin state.
func TestRemoveStaleInterfaceAddressesDeletesOnlyIntroducedCIDR(t *testing.T) {
	channel := &rollbackTestChannel{}
	prior := []InterfaceState{{Name: "lyroute-ens256", Addresses: []string{"192.168.10.1/24"}}}
	attempted := []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", Addresses: []string{"192.168.10.1/24", "192.168.10.254/24"}}}
	if err := (Adapter{Client: &rollbackTestClient{channel: channel}}).RemoveStaleInterfaceAddresses(context.Background(), "txn-stale", prior, attempted); err != nil {
		t.Fatalf("stale address cleanup: %v", err)
	}
	joined := joinedCommands(channel.operations)
	if !strings.Contains(joined, "ip address del lyroute-ens256 192.168.10.254/24") {
		t.Fatalf("introduced address was not removed:\n%s", joined)
	}
	if strings.Contains(joined, "ip address del lyroute-ens256 192.168.10.1/24") {
		t.Fatalf("previous LAN address was removed:\n%s", joined)
	}
	if strings.Contains(joined, "set interface state") {
		t.Fatalf("stale cleanup changed admin state:\n%s", joined)
	}
}

// Rollback of a route policy must resolve per-policy ingress against the
// previous generation's LANs. When the failed apply removed the LAN that owned
// the policy, falling back to its assignments (or to the default LAN) would
// classify the restored policy on the wrong interface.
func TestRouteRollbackUsesPriorAssignmentsForPolicyIngress(t *testing.T) {
	priorPolicy := trafficpolicy.RoutePolicy{
		ID:       "route-prior",
		Priority: 10,
		Action:   "route",
		NextHop:  "10.99.0.2",
		Match:    trafficpolicy.Match{Sources: []string{"192.168.10.10/32"}, Destinations: []string{"10.99.0.2/32"}, Protocols: []string{"any"}, SourcePorts: []string{"any"}, DestPorts: []string{"any"}},
	}
	prior := Snapshot{
		RoutePolicies:    []trafficpolicy.RoutePolicy{priorPolicy},
		PreNATClassifier: &PreNATClassifierState{Enabled: true, InterfaceName: "lyroute-ens256", LANPrefix: "192.168.10.0/24", RuleCount: 2},
	}
	channel := &rollbackTestChannel{classifier: prior.PreNATClassifier, policies: prior.RoutePolicies}
	current := RouteWANGroupPlan{
		TransactionID:       "txn-prior-ingress",
		IngressVPPInterface: "lyroute-lab20",
		// The failed generation dropped the LAN10 assignment.
		AddressAssignments:      []AddressAssignment{{ID: "lan20", LinuxInterface: "ens224", VPPInterface: "lyroute-lab20", CIDR: "192.168.20.1/24", Role: "lan"}},
		PriorAddressAssignments: []AddressAssignment{{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"}},
	}
	err := (Adapter{Client: &rollbackTestClient{channel: channel}}).routeWANGroupFailure(context.Background(), channel, current.TransactionID, "readback", errRollbackTestFailure, prior, current)
	lifecycle, ok := err.(*RouteWANGroupLifecycleError)
	if !ok {
		t.Fatalf("error type = %T, want *RouteWANGroupLifecycleError", err)
	}
	if lifecycle.RollbackResult != RollbackSucceeded {
		t.Fatalf("rollback reported %s: %v", lifecycle.RollbackResult, lifecycle.Rollback)
	}
	joined := joinedCommands(channel.operations)
	if !strings.Contains(joined, "set ly-route pre-nat-route interface lyroute-ens256 lan-prefix 192.168.10.0/24") {
		t.Fatalf("rollback did not bind the classifier to the previous LAN:\n%s", joined)
	}
	if strings.Contains(joined, "lyroute-lab20") {
		t.Fatalf("rollback used the failed generation's LAN:\n%s", joined)
	}
}

func joinedCommands(operations []Operation) string {
	var builder strings.Builder
	for _, operation := range operations {
		for _, command := range operation.VPPCtlCommands {
			builder.WriteString(strings.TrimPrefix(command, "?"))
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}
