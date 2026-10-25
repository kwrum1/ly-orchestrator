package apply

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestGatewayDiffAppliesNAT44ForStaticWANPoolChanges(t *testing.T) {
	diff := vpp.GatewayDiff{NAT44: vpp.NAT44Plan{PoolAddresses: []vpp.NAT44PoolAddress{{
		Interface: "lyroute-enp0s5", Address: "100.78.0.2",
	}}}}
	if !gatewayDiffApplies("nat44", diff) {
		t.Fatal("a static WAN pool change must re-run NAT44 initialization")
	}
	if gatewayDiffApplies("port-maps", diff) {
		t.Fatal("a pool-only change must not be mistaken for a port-map change")
	}

	diff = vpp.GatewayDiff{NAT44: vpp.NAT44Plan{DeletePoolAddresses: []string{"100.77.0.2"}}}
	if !gatewayDiffApplies("nat44", diff) {
		t.Fatal("a superseded WAN pool address must trigger NAT44 cleanup")
	}
}
