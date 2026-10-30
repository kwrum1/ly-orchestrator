package apply

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

// A LAN-only generation must not schedule the nat44 node: the persisted plan
// has no NAT object, and the evidence validator rejects a NAT44 resource that
// is not in the plan. The LAN NAT role is reconciled by the LAN control-plane
// lifecycle instead. A static pool change still schedules the node.
func TestGatewayDiffDoesNotScheduleNAT44ForLANOnlyGeneration(t *testing.T) {
	if gatewayDiffHasApply("nat44", vpp.GatewayDiff{
		NAT44: vpp.NAT44Plan{LANVPPInterfaces: []string{"lyroute-enp4s0"}},
	}) {
		t.Fatal("LAN-only generation scheduled a NAT44 evidence resource")
	}
	if !gatewayDiffHasApply("nat44", vpp.GatewayDiff{
		NAT44: vpp.NAT44Plan{PoolAddresses: []vpp.NAT44PoolAddress{{Interface: "lyroute-enp5s0", Address: "203.0.113.2"}}},
	}) {
		t.Fatal("WAN pool change did not schedule the nat44 node")
	}
}
