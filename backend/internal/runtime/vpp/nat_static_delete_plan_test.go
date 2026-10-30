package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

func TestDeletedStaticVIPIsRemovedWithTheExplicitAddress(t *testing.T) {
	mapping := nat.StaticMapping{ID: "host", ExternalAddress: "10.1.18.35", InternalAddress: "192.168.89.2", ExternalAddressExplicit: true}
	prior := Plan{NAT: nat.CompiledConfig{StaticMappings: []nat.StaticMapping{mapping}}}
	desired := Plan{}
	diff, err := ReconcileGatewayPlan(GatewayReconciliationInput{
		TransactionID:       "txn-static-delete",
		Prior:               prior,
		Desired:             desired,
		Live:                Snapshot{NAT: nat.CompiledConfig{StaticMappings: []nat.StaticMapping{mapping}}},
		RepairVerifiedDrift: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.NAT44.DeleteStaticMappings) != 1 || diff.NAT44.DeleteStaticMappings[0] != "host" {
		t.Fatalf("deleted static mappings = %#v", diff.NAT44.DeleteStaticMappings)
	}
	diff.NAT44.ReadbackStaticMappings = prior.NAT.StaticMappings
	operations, err := BuildNAT44Operations(diff.NAT44)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("delete operations = %#v", operations)
	}
	commands := strings.Join(operations[0].VPPCtlCommands, "\n")
	if !strings.Contains(commands, "nat44 add static mapping local 192.168.89.2 external 10.1.18.35 del") {
		t.Fatalf("static delete did not address the explicit VIP:\n%s", commands)
	}
	if strings.Contains(commands, "external 10.1.18.24") {
		t.Fatalf("static delete used the gateway WAN address instead of the VIP:\n%s", commands)
	}
}
