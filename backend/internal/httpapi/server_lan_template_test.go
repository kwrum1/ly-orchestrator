package httpapi

import (
	"os"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestSetApplyLANVPPEnvironmentBindsAndRestores(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_INTERFACE", "before")
	restore := setApplyLANVPPEnvironment(vpp.Plan{AddressAssignments: []vpp.AddressAssignment{
		{Role: "wan", LinuxInterface: "eth1"},
		{Role: "lan", LinuxInterface: "eth2", VPPInterface: "lyroute-eth2"},
	}})
	if got := os.Getenv("LY_ROUTE_LAN_INTERFACE"); got != "eth2" {
		t.Fatalf("during apply LY_ROUTE_LAN_INTERFACE=%q, want eth2", got)
	}
	restore()
	if got := os.Getenv("LY_ROUTE_LAN_INTERFACE"); got != "before" {
		t.Fatalf("after apply LY_ROUTE_LAN_INTERFACE=%q, want before", got)
	}
}

func TestSetApplyLANVPPEnvironmentFallsBackToRenderedLAN(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_INTERFACE", "before")
	restore := setApplyLANVPPEnvironment(vpp.Plan{Interfaces: []vpp.InterfaceState{
		{Name: "lyroute-eth2", Addresses: []string{"192.168.88.1/24"}},
	}})
	if got := os.Getenv("LY_ROUTE_LAN_INTERFACE"); got != "eth2" {
		t.Fatalf("during apply LY_ROUTE_LAN_INTERFACE=%q, want eth2", got)
	}
	restore()
}
