package vpp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

func natPoolWANAssignments(address string) []AddressAssignment {
	return []AddressAssignment{
		{ID: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24", Role: "lan"},
		{ID: "wan", LinuxInterface: "enp0s5", VPPInterface: "lyroute-enp0s5", CIDR: address + "/24", Role: "wan"},
	}
}

func TestNAT44InitializeDeletesSupersededPoolAddressBeforeAddingNewWAN(t *testing.T) {
	plan := NAT44Plan{
		TransactionID:       "txn-wan-switch",
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp0s4",
		WANIngressInterface: "lyroute-enp0s5",
		PoolAddresses:       []NAT44PoolAddress{{Interface: "lyroute-enp0s5", Address: "100.78.0.2"}},
		DeletePoolAddresses: []string{"100.77.0.2"},
	}

	commands := nat44InitializeForPlan(plan)
	text := strings.Join(commands, "\n")
	for _, required := range []string{
		"?nat44 add address 100.77.0.2 tenant-vrf 0 del",
		"?nat44 add address 100.77.0.2 del",
		"?nat44 add interface address lyroute-enp0s5 del",
		"?nat44 add address 100.78.0.2 tenant-vrf 0",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("pool switch is missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, "\n?nat44 add address 100.77.0.2 tenant-vrf 0\n") {
		t.Fatalf("superseded pool address is still added:\n%s", text)
	}
	deleteAt := strings.Index(text, "?nat44 add address 100.77.0.2 tenant-vrf 0 del")
	addAt := strings.Index(text, "?nat44 add address 100.78.0.2 tenant-vrf 0")
	if deleteAt < 0 || addAt < 0 || deleteAt > addAt {
		t.Fatalf("stale pool delete must precede the new address:\n%s", text)
	}
}

func TestNAT44InitializeIsIdempotentForUnchangedWANPool(t *testing.T) {
	plan := NAT44Plan{
		TransactionID:       "txn-wan-stable",
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp0s4",
		PoolAddresses: []NAT44PoolAddress{
			{Interface: "lyroute-enp0s3", Address: "100.77.0.2"},
			{Interface: "lyroute-enp0s5", Address: "100.78.0.2"},
		},
	}
	first := nat44InitializeForPlan(plan)
	second := nat44InitializeForPlan(plan)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated NAT44 initialization differs:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if strings.Count(strings.Join(first, "\n"), "?nat44 add address 100.77.0.2 tenant-vrf 0") != 1 ||
		strings.Count(strings.Join(first, "\n"), "?nat44 add address 100.78.0.2 tenant-vrf 0") != 1 {
		t.Fatalf("NAT44 initialization duplicates a pool address:\n%s", strings.Join(first, "\n"))
	}
}

func TestApplyNAT44InitializesPoolWithoutPortMappings(t *testing.T) {
	client := &nat44LifecycleClient{replies: map[string]Reply{
		"vpp.nat44-ed.snapshot": {Payload: NAT44Readback{Behavior: nat.BehaviorEndpointDependent}},
	}}
	_, err := (Adapter{Client: client}).ApplyNAT44(context.Background(), NAT44Plan{
		TransactionID:       "txn-pool-only",
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp0s4",
		PoolAddresses: []NAT44PoolAddress{
			{Interface: "lyroute-enp0s3", Address: "100.77.0.2"},
			{Interface: "lyroute-enp0s5", Address: "100.78.0.2"},
		},
	}, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	initialize := ""
	for _, operation := range client.operations {
		if operation.Name == "vpp.nat44.initialize" {
			initialize = strings.Join(operation.VPPCtlCommands, "\n")
			break
		}
	}
	if initialize == "" {
		t.Fatal("pool-only NAT44 plan did not initialize the translation pool")
	}
	for _, required := range []string{
		"?nat44 add address 100.77.0.2 tenant-vrf 0",
		"?nat44 add address 100.78.0.2 tenant-vrf 0",
		"set interface nat44 out lyroute-enp0s3 output-feature",
		"set interface nat44 out lyroute-enp0s5 output-feature",
	} {
		if !strings.Contains(initialize, required) {
			t.Fatalf("pool-only initialization is missing %q:\n%s", required, initialize)
		}
	}
}

func TestRestoreNAT44PoolRollsBackIntroducedAddressAndRestoresPrior(t *testing.T) {
	client := &nat44LifecycleClient{}
	channel, err := client.OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := NAT44Plan{
		TransactionID: "txn-pool-rollback",
		PoolAddresses: []NAT44PoolAddress{
			{Interface: "lyroute-enp0s5", Address: "100.78.0.2"},
		},
		PreviousPoolAddresses: []NAT44PoolAddress{
			{Interface: "lyroute-enp0s3", Address: "100.77.0.2"},
		},
	}
	if err := restoreNAT44Pool(context.Background(), channel, plan.TransactionID, plan); err != nil {
		t.Fatal(err)
	}
	if len(client.operations) != 1 {
		t.Fatalf("rollback operations = %#v, want one pool rollback operation", client.operations)
	}
	text := strings.Join(client.operations[0].VPPCtlCommands, "\n")
	for _, required := range []string{
		"?nat44 add address 100.78.0.2 tenant-vrf 0 del",
		"?nat44 add address 100.78.0.2 del",
		"?nat44 add address 100.77.0.2 tenant-vrf 0",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("pool rollback is missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, "?nat44 add address 100.77.0.2 tenant-vrf 0 del") {
		t.Fatalf("pool rollback deleted the prior address:\n%s", text)
	}
}

func TestGatewayDiffCarriesStaticWANPoolForFailoverGenerations(t *testing.T) {
	natPolicy := trafficpolicy.Config{RoutePolicies: []trafficpolicy.RoutePolicy{{
		ID: "wan-group-default", Action: "nat", Egress: "acc-b-wan-group",
	}}}
	prior := Plan{
		AddressAssignments: natPoolWANAssignments("100.77.0.2"),
		Policy:             natPolicy,
	}
	desired := Plan{
		AddressAssignments: natPoolWANAssignments("100.78.0.2"),
		Policy:             natPolicy,
	}

	diff, err := ReconcileGatewayPlan(GatewayReconciliationInput{
		TransactionID:       "txn-wan-pool-diff",
		Prior:               prior,
		Desired:             desired,
		RepairVerifiedDrift: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(diff.NAT44.PoolAddresses, []NAT44PoolAddress{{Interface: "lyroute-enp0s5", Address: "100.78.0.2"}}) {
		t.Fatalf("desired pool = %#v, want the new WAN address", diff.NAT44.PoolAddresses)
	}
	if !reflect.DeepEqual(diff.NAT44.DeletePoolAddresses, []string{"100.77.0.2"}) {
		t.Fatalf("stale pool = %#v, want the old WAN address", diff.NAT44.DeletePoolAddresses)
	}
	if !reflect.DeepEqual(diff.NAT44.PreviousPoolAddresses, []NAT44PoolAddress{{Interface: "lyroute-enp0s5", Address: "100.77.0.2"}}) {
		t.Fatalf("previous pool = %#v, want the prior WAN address", diff.NAT44.PreviousPoolAddresses)
	}
}
