package vpp

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// rollbackTestChannel serves typed readbacks and records every operation so a
// test can assert both the command text and the restore order.
type rollbackTestChannel struct {
	interfaces []InterfaceState
	classifier *PreNATClassifierState
	policies   []trafficpolicy.RoutePolicy
	operations []Operation
	failOn     string
}

func (channel *rollbackTestChannel) Do(_ context.Context, operation Operation) (Reply, error) {
	channel.operations = append(channel.operations, operation)
	if channel.failOn != "" && strings.Contains(operation.Name, channel.failOn) {
		return Reply{}, errRollbackTestFailure
	}
	results := make([]VPPCTLCommandResult, 0, len(operation.VPPCtlCommands))
	for _, command := range operation.VPPCtlCommands {
		command = strings.TrimPrefix(command, "?")
		result := VPPCTLCommandResult{Command: command}
		if strings.HasPrefix(command, "show ly-route pre-nat-route") && channel.classifier != nil {
			if channel.classifier.Enabled {
				result.Stdout = "enabled 1 interface " + channel.classifier.InterfaceName + " lan-prefix " + channel.classifier.LANPrefix + " rules 2 radix-nodes 25\n"
			} else {
				result.Stdout = "enabled 0 interface  lan-prefix  rules 0 radix-nodes 0\n"
			}
		}
		if strings.HasPrefix(command, "show interface address") && len(channel.interfaces) > 0 {
			var lines []string
			for _, state := range channel.interfaces {
				lines = append(lines, state.Name+" ("+state.AdminState+"):")
				for _, address := range state.Addresses {
					lines = append(lines, "  L3 "+address)
				}
			}
			result.Stdout = strings.Join(lines, "\n") + "\n"
		}
		results = append(results, result)
	}
	switch operation.Name {
	case "vpp.interface.snapshot":
		return Reply{Payload: InterfaceReadback{Interfaces: channel.interfaces}}, nil
	case "vpp.route-policy.snapshot":
		return Reply{Payload: RoutePolicyReadback{Policies: channel.policies, Classifier: channel.classifier}}, nil
	default:
		return Reply{Payload: VPPCTLReplyPayload{CommandResults: results}}, nil
	}
}

func (channel *rollbackTestChannel) Close() error { return nil }

type rollbackTestClient struct{ channel *rollbackTestChannel }

func (client *rollbackTestClient) OpenChannel(context.Context) (Channel, error) {
	return client.channel, nil
}

var errRollbackTestFailure = rollbackTestError("injected apply failure")

type rollbackTestError string

func (err rollbackTestError) Error() string { return string(err) }

// The rollback snapshot must carry the previous generation's interface
// addresses and the pre-NAT classifier header, not just the persisted plan's
// object names.
func TestGatewayDiffSnapshotCapturesPriorLANAddressAndClassifier(t *testing.T) {
	channel := &rollbackTestChannel{
		interfaces: []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.1/24"}}},
		classifier: &PreNATClassifierState{Enabled: true, InterfaceName: "lyroute-ens256", LANPrefix: "192.168.10.0/24", RuleCount: 2},
	}
	client := &rollbackTestClient{channel: channel}
	prior := Plan{
		AddressAssignments: []AddressAssignment{{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"}},
		Interfaces:         []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.10.1/24"}}},
	}
	desired := prior
	desired.Policy.RoutePolicies = []trafficpolicy.RoutePolicy{{ID: "route-new", Action: "route", NextHop: "10.99.0.2"}}

	request := GatewayDiffSnapshotRequest("txn-rollback-snapshot", prior, desired)
	if len(request.Interfaces) != 1 || request.Interfaces[0] != "lyroute-ens256" {
		t.Fatalf("snapshot request interfaces = %#v, want the prior LAN", request.Interfaces)
	}
	if !containsSnapshotCapability(request.Capabilities, SnapshotCapabilityInterfaces) {
		t.Fatalf("snapshot request lacks interface capability: %#v", request.Capabilities)
	}
	if !containsSnapshotCapability(request.Capabilities, SnapshotCapabilityRoutePolicies) {
		t.Fatalf("snapshot request lacks route-policy capability (classifier capture): %#v", request.Capabilities)
	}

	snapshot, err := (Adapter{Client: client}).Snapshot(context.Background(), request)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snapshot.Interfaces) != 1 || !strings.Contains(strings.Join(snapshot.Interfaces[0].Addresses, ","), "192.168.10.1/24") {
		t.Fatalf("rollback snapshot lost the LAN address: %#v", snapshot.Interfaces)
	}
	if snapshot.PreNATClassifier == nil || snapshot.PreNATClassifier.InterfaceName != "lyroute-ens256" || snapshot.PreNATClassifier.LANPrefix != "192.168.10.0/24" {
		t.Fatalf("rollback snapshot lost the classifier header: %#v", snapshot.PreNATClassifier)
	}
}

// A thin prior generation that did not enumerate interfaces must still capture
// the desired LAN state instead of producing an empty interface snapshot.
func TestGatewayDiffSnapshotFallsBackToDesiredInterfacesForThinPrior(t *testing.T) {
	desired := Plan{
		Interfaces:         []InterfaceState{{Name: "lyroute-ens256", AdminState: "up", Addresses: []string{"192.168.10.1/24"}}},
		AddressAssignments: []AddressAssignment{{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"}},
	}
	request := GatewayDiffSnapshotRequest("txn-thin-prior", Plan{}, desired)
	if len(request.Interfaces) != 1 || request.Interfaces[0] != "lyroute-ens256" {
		t.Fatalf("request interfaces = %#v, want the desired LAN fallback", request.Interfaces)
	}
}

// The empty-prior rollback used to fail its own readback with "no interface
// state returned", which reported a successful restore as a rollback failure.
func TestInterfaceRollbackWithEmptyPriorDoesNotFailReadback(t *testing.T) {
	channel := &rollbackTestChannel{}
	client := &rollbackTestClient{channel: channel}
	result, err := (Adapter{Client: client}).ApplyInterfaceBond(context.Background(), InterfaceBondPlan{TransactionID: "txn-empty"}, Snapshot{})
	if err != nil {
		t.Fatalf("empty interface rollback failed: %v", err)
	}
	if len(result.Readback.Interfaces) != 0 || len(channel.operations) != 0 {
		t.Fatalf("empty rollback mutated or read back state: ops=%#v readback=%#v", channel.operations, result.Readback)
	}
}

// Route rollback must bind the restored classifier to the LAN that owned each
// prior policy, and must restore the previous global classifier header.
func TestRouteRollbackUsesPerPolicyIngressAndRestoresClassifier(t *testing.T) {
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
	current := RouteWANGroupPlan{
		TransactionID: "txn-rollback-ingress",
		// lab20 sorts first: the old default-LAN rewrite would classify on it.
		AddressAssignments: []AddressAssignment{
			{ID: "lan20", LinuxInterface: "ens224", VPPInterface: "lyroute-lab20", CIDR: "192.168.20.1/24", Role: "lan"},
			{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"},
		},
		Routes: []trafficpolicy.RoutePolicy{{ID: "route-new", Action: "route", NextHop: "10.99.0.99", Match: trafficpolicy.Match{Sources: []string{"192.168.10.0/24"}}}},
	}
	channel := &rollbackTestChannel{classifier: prior.PreNATClassifier, policies: prior.RoutePolicies}
	err := (Adapter{Client: &rollbackTestClient{channel: channel}}).routeWANGroupFailure(context.Background(), channel, current.TransactionID, "readback", errRollbackTestFailure, prior, current)
	if err == nil {
		t.Fatal("expected the lifecycle error to be returned")
	}
	lifecycle, ok := err.(*RouteWANGroupLifecycleError)
	if !ok {
		t.Fatalf("error type = %T, want *RouteWANGroupLifecycleError", err)
	}
	if lifecycle.RollbackResult != RollbackSucceeded {
		t.Fatalf("empty-prior rollback reported %s: %v", lifecycle.RollbackResult, lifecycle.Rollback)
	}
	joined := ""
	for _, operation := range channel.operations {
		joined += strings.Join(operation.VPPCtlCommands, "\n") + "\n"
	}
	if !strings.Contains(joined, "set ly-route pre-nat-route interface lyroute-ens256 lan-prefix 192.168.10.0/24") {
		t.Fatalf("rollback did not restore the previous classifier header:\n%s", joined)
	}
	if !strings.Contains(joined, "source 192.168.10.10/32") {
		t.Fatalf("rollback did not replay the prior policy rule:\n%s", joined)
	}
	if strings.Contains(joined, "lyroute-lab20 lan-prefix") {
		t.Fatalf("rollback bound the classifier to the default LAN:\n%s", joined)
	}
}

// Rollback cleanup must not tear down an LCP pair that existed in the prior
// generation: deleting it removes the live LAN address before it can be
// replayed.
func TestRollbackCleanupSkipsUnchangedLANLCP(t *testing.T) {
	prior := provenPlan(Plan{
		RequestID: "txn-cleanup",
		AddressAssignments: []AddressAssignment{
			{ID: "lan10", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24", Role: "lan"},
		},
	}, "ens256")
	desired := prior
	cleanup, err := SupplementalIntroducedCleanupOperations(prior, desired, SupplementalInterfaces)
	if err != nil {
		t.Fatalf("introduced cleanup: %v", err)
	}
	for _, operation := range cleanup {
		if strings.Contains(operation.Name, "lan-control-lcp") {
			t.Fatalf("unchanged LAN LCP was queued for rollback teardown: %#v", operation)
		}
	}
}
