package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

// Without a WAN the LAN cannot be translated; detaching it prevents the stale
// nat44 outside role from dropping DHCP broadcasts before ABF/LCP.
func TestNAT44InitializeFallbackIsAdditive(t *testing.T) {
	commands := strings.Join(nat44InitializeForPlan(NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0",
		LANVPPInterfaces:    []string{"lyroute-enp4s0"},
	}), "\n")
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0") {
		t.Fatalf("fallback did not add the LAN inside role:\n%s", commands)
	}
	for _, forbidden := range []string{"output-feature del", " in lyroute-enp4s0 del", " out lyroute-enp4s0 del"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("unknown-state fallback emitted a delete %q:\n%s", forbidden, commands)
		}
	}
}

func TestNAT44InitializeBindsLANInsideWithWAN(t *testing.T) {
	commands := strings.Join(nat44InitializeForPlan(NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0",
		LANVPPInterfaces:    []string{"lyroute-enp4s0"},
		PoolAddresses:       []NAT44PoolAddress{{Interface: "lyroute-enp5s0", Address: "203.0.113.2"}},
	}), "\n")
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0") {
		t.Fatalf("LAN inside role is missing:\n%s", commands)
	}
	if !strings.Contains(commands, "?set interface nat44 out lyroute-enp5s0") {
		t.Fatalf("WAN outside role is missing:\n%s", commands)
	}
	for _, forbidden := range []string{"output-feature del", " in lyroute-enp4s0 del", " out lyroute-enp4s0 del"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("fallback emitted a delete %q:\n%s", forbidden, commands)
		}
	}
}
