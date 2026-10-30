package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

func nat44RoleState(inside, outside, outputFeature []string) nat44RuntimeState {
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{},
		Outside:       map[string]bool{},
		OutputFeature: map[string]bool{},
	}
	for _, name := range inside {
		state.Inside[name] = true
	}
	for _, name := range outside {
		state.Outside[name] = true
	}
	for _, name := range outputFeature {
		state.OutputFeature[name] = true
	}
	return state
}

func roleCommands(plan NAT44Plan, state nat44RuntimeState) string {
	return strings.Join(nat44ReconcileCommands(plan, state), "\n")
}

func nat44KnownEmptyRoleState() nat44RuntimeState {
	state := nat44RoleState(nil, nil, nil)
	// A non-empty pool proves the plugin is enabled, so the repeated-clear
	// assertion measures role residue rather than the one-time plugin enable.
	state.Pool = []nat44PoolAddressEntry{{Address: "203.0.113.10"}}
	return state
}

func TestNAT44RoleReconcileRetiresClearedLAN(t *testing.T) {
	state := nat44RoleState([]string{"lyroute-enp4s0"}, nil, nil)
	plan := NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0",
		DeleteInterfaces:    []string{"lyroute-enp4s0"},
	}
	commands := roleCommands(plan, state)
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0 del") {
		t.Fatalf("cleared LAN inside role was not removed:\n%s", commands)
	}
	if strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0\n") {
		t.Fatalf("cleared LAN inside role was re-added:\n%s", commands)
	}
	if repeat := roleCommands(plan, nat44KnownEmptyRoleState()); repeat != "" {
		t.Fatalf("repeated clear is not idempotent: %q", repeat)
	}
}

func TestNAT44RoleReconcileRetiresClearedWAN(t *testing.T) {
	state := nat44RoleState(nil, []string{"lyroute-enp2s0"}, []string{"lyroute-enp2s0"})
	plan := NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0",
		LANVPPInterfaces:    []string{"lyroute-enp4s0"},
		DeleteInterfaces:    []string{"lyroute-enp2s0"},
	}
	commands := roleCommands(plan, state)
	if !strings.Contains(commands, "?set interface nat44 out lyroute-enp2s0 output-feature del") {
		t.Fatalf("cleared WAN outside/output-feature role was not removed:\n%s", commands)
	}
	if strings.Contains(commands, "?set interface nat44 out lyroute-enp2s0 del") {
		t.Fatalf("combined role emitted the unsupported plain del:\n%s", commands)
	}
	if repeat := roleCommands(plan, nat44KnownEmptyRoleState()); repeat != "" {
		t.Fatalf("repeated WAN clear is not idempotent: %q", repeat)
	}
}

func TestNAT44RoleReconcileClearsOneLANWithoutTouchingOther(t *testing.T) {
	state := nat44RoleState([]string{"lyroute-enp3s0", "lyroute-enp4s0"}, nil, nil)
	plan := NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp3s0",
		LANVPPInterfaces:    []string{"lyroute-enp3s0"},
		WANIngressInterface: "lyroute-enp2s0",
		DeleteInterfaces:    []string{"lyroute-enp4s0"},
	}
	commands := roleCommands(plan, state)
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0 del") {
		t.Fatalf("cleared LAN role was not removed:\n%s", commands)
	}
	if strings.Contains(commands, "lyroute-enp3s0") {
		t.Fatalf("untouched LAN was mutated:\n%s", commands)
	}
}

func TestNAT44RoleReconcileTransitionsLANToWAN(t *testing.T) {
	state := nat44RoleState([]string{"lyroute-enp2s0"}, nil, nil)
	plan := NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0",
		LANVPPInterfaces:    []string{"lyroute-enp4s0"},
		WANIngressInterface: "lyroute-enp2s0",
	}
	commands := roleCommands(plan, state)
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp2s0 del") {
		t.Fatalf("stale inside role was not removed on LAN->WAN transition:\n%s", commands)
	}
	if !strings.Contains(commands, "?set interface nat44 out lyroute-enp2s0") {
		t.Fatalf("WAN outside role was not added:\n%s", commands)
	}
}

func TestNAT44RoleReconcileLastLANClearDoesNotReAddIngressFallback(t *testing.T) {
	state := nat44RoleState([]string{"lyroute-enp4s0"}, nil, nil)
	plan := NAT44Plan{
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp4s0", // gateway diff keeps the prior LAN for route cleanup
		DeleteInterfaces:    []string{"lyroute-enp4s0"},
	}
	commands := roleCommands(plan, state)
	if !strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0 del") {
		t.Fatalf("last LAN role was not removed:\n%s", commands)
	}
	if strings.Contains(commands, "?set interface nat44 in lyroute-enp4s0\n") {
		t.Fatalf("last LAN role was re-added from the ingress fallback:\n%s", commands)
	}

	initialize := strings.Join(nat44InitializeForPlan(plan), "\n")
	if strings.Contains(initialize, "?set interface nat44 in lyroute-enp4s0\n") {
		t.Fatalf("unknown-state fallback re-added the cleared LAN:\n%s", initialize)
	}
}

func TestStaleNAT44RoleInterfacesIsPerPort(t *testing.T) {
	prior := Plan{AddressAssignments: []AddressAssignment{
		{LinuxInterface: "enp3s0", VPPInterface: "lyroute-enp3s0", Role: "lan"},
		{LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", Role: "lan"},
	}}
	desired := Plan{AddressAssignments: []AddressAssignment{
		{LinuxInterface: "enp3s0", VPPInterface: "lyroute-enp3s0", Role: "lan"},
	}}
	stale := staleNAT44RoleInterfaces(prior, desired)
	if len(stale) != 1 || stale[0] != "lyroute-enp4s0" {
		t.Fatalf("stale interfaces = %#v, want only lyroute-enp4s0", stale)
	}
	if again := staleNAT44RoleInterfaces(desired, desired); len(again) != 0 {
		t.Fatalf("idempotent diff produced %#v", again)
	}
}
