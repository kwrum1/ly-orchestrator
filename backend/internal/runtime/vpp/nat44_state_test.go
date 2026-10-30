package vpp

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

func nat44TargetPlan() NAT44Plan {
	return NAT44Plan{
		TransactionID:       "txn-target",
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-lan0",
		LANVPPInterfaces:    []string{"lyroute-lan0"},
		WANIngressInterface: "lyroute-wan0",
		PoolAddresses:       []NAT44PoolAddress{{Interface: "lyroute-wan0", Address: "10.1.18.24"}},
	}
}

func nat44TargetState() nat44RuntimeState {
	return nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-lan0": true},
		Outside:       map[string]bool{"lyroute-wan0": true},
		OutputFeature: map[string]bool{},
		Pool:          []nat44PoolAddressEntry{{Address: "10.1.18.24", VRF: 0, VRFSet: true}},
	}
}

func TestNAT44ReconcileCommandsEmptyWhenAlreadyAtTarget(t *testing.T) {
	commands := nat44ReconcileCommands(nat44TargetPlan(), nat44TargetState())
	if len(commands) != 0 {
		t.Fatalf("already-converged NAT44 emitted mutations: %#v", commands)
	}
}

func TestNAT44ReconcileCommandsOnlyChangesRealDifferences(t *testing.T) {
	state := nat44RuntimeState{Known: true, Inside: map[string]bool{}, Outside: map[string]bool{}, OutputFeature: map[string]bool{}}
	commands := nat44ReconcileCommands(nat44TargetPlan(), state)
	text := strings.Join(commands, "\n")
	for _, required := range []string{
		"?set interface nat44 in lyroute-lan0",
		"?set interface nat44 out lyroute-wan0",
		"?nat44 add address 10.1.18.24 tenant-vrf 0",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("NAT44 reconcile is missing %q:\n%s", required, text)
		}
	}
	// A WAN that is not attached yet must not receive a delete. The previous
	// implementation emitted delete-before-add unconditionally, which is what
	// produced `set interface nat44: del ... failed` on every apply.
	if strings.Contains(text, "?set interface nat44 out lyroute-wan0 del") {
		t.Fatalf("absent WAN received a delete:\n%s", text)
	}
	if strings.Count(text, "?nat44 add address 10.1.18.24 tenant-vrf 0") != 1 {
		t.Fatalf("pool address was emitted more than once:\n%s", text)
	}
	if strings.Contains(text, "?nat44 add interface address lyroute-wan0 del") {
		t.Fatalf("converged pool address still emitted a legacy delete:\n%s", text)
	}
}

func TestNAT44RoleReconcileOnlyRealDifferences(t *testing.T) {
	// No WAN: lan0 is inside, lan1 is stale outside+output, lan2 has no role.
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-lan0": true},
		Outside:       map[string]bool{"lyroute-lan1": true},
		OutputFeature: map[string]bool{"lyroute-lan1": true},
	}
	commands := nat44RoleReconcileCommands(
		[]string{"lyroute-lan0", "lyroute-lan1", "lyroute-lan2"},
		nil,
		nat.BehaviorEndpointDependent,
		state,
	)
	text := strings.Join(commands, "\n")
	for _, want := range []string{
		"?set interface nat44 in lyroute-lan0 del",
		"?set interface nat44 out lyroute-lan1 output-feature del",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("role reconcile missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		"lyroute-lan2",
		"?set interface nat44 in lyroute-lan1 del",
		"?set interface nat44 out lyroute-lan1 del",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("role reconcile emitted a non-difference %q:\n%s", forbidden, text)
		}
	}
	for _, command := range commands {
		if strings.Count(text, command) != 1 {
			t.Fatalf("duplicate role command %q:\n%s", command, text)
		}
	}
}

func TestNAT44RoleReconcileRemovesPlainOutsideRoleDirectly(t *testing.T) {
	state := nat44RuntimeState{
		Known:   true,
		Inside:  map[string]bool{},
		Outside: map[string]bool{"lyroute-enp4s0": true},
	}
	commands := nat44RoleReconcileCommands(
		[]string{"lyroute-enp4s0"}, nil, nat.BehaviorEndpointDependent, state,
	)
	text := strings.Join(commands, "\n")
	if !strings.Contains(text, "?set interface nat44 out lyroute-enp4s0 del") {
		t.Fatalf("plain outside role was not removed directly:\n%s", text)
	}
	if strings.Contains(text, "output-feature del") {
		t.Fatalf("plain outside role used the output-feature toggle:\n%s", text)
	}
}

func TestNAT44RoleReconcileNoDelWhenAbsent(t *testing.T) {
	state := nat44RuntimeState{Known: true, Inside: map[string]bool{}, Outside: map[string]bool{}, OutputFeature: map[string]bool{}}
	if commands := nat44RoleReconcileCommands(
		[]string{"lyroute-enp4s0", "lyroute-enp5s0"}, nil, nat.BehaviorEndpointDependent, state,
	); len(commands) != 0 {
		t.Fatalf("empty role state emitted mutations: %#v", commands)
	}
}

func TestNAT44RoleReconcileNormalizesLegacyWANOutputFeature(t *testing.T) {
	// VPP 26.06 prints a legacy outside+output-feature role as
	// `output-feature in out`. Normalize it to the plain outside role without
	// deleting the printed `in` token (that delete fails).
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-enp4s0": true, "lyroute-enp3s0": true},
		Outside:       map[string]bool{"lyroute-enp3s0": true},
		OutputFeature: map[string]bool{"lyroute-enp3s0": true},
	}
	commands := nat44RoleReconcileCommands(
		[]string{"lyroute-enp4s0"},
		[]string{"lyroute-enp3s0"},
		nat.BehaviorEndpointDependent,
		state,
	)
	text := strings.Join(commands, "\n")
	if !strings.Contains(text, "?set interface nat44 out lyroute-enp3s0 output-feature del") ||
		!strings.Contains(text, "?set interface nat44 out lyroute-enp3s0") {
		t.Fatalf("legacy WAN role was not normalized:\n%s", text)
	}
	for _, forbidden := range []string{" in lyroute-enp3s0 del", "lyroute-enp4s0"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("normalization emitted a non-difference %q:\n%s", forbidden, text)
		}
	}
}

func TestNAT44RoleReconcileLeavesPlainOutsideRole(t *testing.T) {
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-enp4s0": true},
		Outside:       map[string]bool{"lyroute-enp3s0": true},
		OutputFeature: map[string]bool{},
	}
	if commands := nat44RoleReconcileCommands(
		[]string{"lyroute-enp4s0"},
		[]string{"lyroute-enp3s0"},
		nat.BehaviorEndpointDependent,
		state,
	); len(commands) != 0 {
		t.Fatalf("plain LAN/WAN roles emitted mutations: %#v", commands)
	}
}

func TestNAT44RoleReconcileKeepsOtherLANAndWANRoles(t *testing.T) {
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-lan0": true},
		Outside:       map[string]bool{"lyroute-wan0": true},
		OutputFeature: map[string]bool{},
	}
	commands := nat44RoleReconcileCommands(
		[]string{"lyroute-lan0", "lyroute-lan1"},
		[]string{"lyroute-wan0", "lyroute-wan1"},
		nat.BehaviorEndpointDependent,
		state,
	)
	text := strings.Join(commands, "\n")
	if !strings.Contains(text, "?set interface nat44 in lyroute-lan1") {
		t.Fatalf("new LAN was not bound inside:\n%s", text)
	}
	if !strings.Contains(text, "?set interface nat44 out lyroute-wan1") {
		t.Fatalf("new WAN was not bound outside:\n%s", text)
	}
	if strings.Contains(text, "lyroute-lan0") || strings.Contains(text, "lyroute-wan0") {
		t.Fatalf("converged LAN/WAN roles were changed:\n%s", text)
	}
}

func TestNAT44RoleReconcileInstallsProxyOutputFeatureOnPlainWAN(t *testing.T) {
	// The proxy egress requires `output-feature in out`. VPP rejects the
	// combined add while the plain outside role is still attached, so the
	// plain role must be removed first, in one apply, before any readback.
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-enp2s0": true},
		Outside:       map[string]bool{"lyroute-enp4s0": true},
		OutputFeature: map[string]bool{},
	}
	commands := nat44RoleReconcileCommandsForPlan(
		[]string{"lyroute-enp2s0"},
		[]string{"lyroute-enp4s0"},
		nil,
		[]string{"lyroute-enp4s0"},
		nat.BehaviorEndpointDependent,
		state,
	)
	text := strings.Join(commands, "\n")
	delIndex := strings.Index(text, "?set interface nat44 out lyroute-enp4s0 del")
	addIndex := strings.Index(text, "?set interface nat44 out lyroute-enp4s0 output-feature")
	if delIndex < 0 || addIndex < 0 || delIndex > addIndex {
		t.Fatalf("proxy underlay role was not replaced before the output-feature add:\n%s", text)
	}
	if strings.Contains(text, "lyroute-enp2s0") {
		t.Fatalf("converged LAN role was changed:\n%s", text)
	}
}

func TestNAT44RoleReconcileKeepsInstalledProxyOutputFeature(t *testing.T) {
	// A repeated apply must be empty: VPP 26.06 reports "already configured"
	// (and still exits 0) when the combined role is re-added.
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-enp2s0": true, "lyroute-enp4s0": true},
		Outside:       map[string]bool{"lyroute-enp4s0": true},
		OutputFeature: map[string]bool{"lyroute-enp4s0": true},
	}
	commands := nat44RoleReconcileCommandsForPlan(
		[]string{"lyroute-enp2s0"},
		[]string{"lyroute-enp4s0"},
		nil,
		[]string{"lyroute-enp4s0"},
		nat.BehaviorEndpointDependent,
		state,
	)
	if len(commands) != 0 {
		t.Fatalf("converged proxy underlay emitted NAT44 noise: %#v", commands)
	}
}

func TestNAT44RoleReconcileRevertsProxyOutputFeatureAfterEgressRemoval(t *testing.T) {
	// Removing the egress returns the WAN to the plain outside role instead of
	// leaving an orphaned output-feature.
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-enp2s0": true, "lyroute-enp4s0": true},
		Outside:       map[string]bool{"lyroute-enp4s0": true},
		OutputFeature: map[string]bool{"lyroute-enp4s0": true},
	}
	commands := nat44RoleReconcileCommandsForPlan(
		[]string{"lyroute-enp2s0"},
		[]string{"lyroute-enp4s0"},
		nil,
		nil,
		nat.BehaviorEndpointDependent,
		state,
	)
	text := strings.Join(commands, "\n")
	required := []string{
		"?set interface nat44 out lyroute-enp4s0 output-feature del",
		"?set interface nat44 out lyroute-enp4s0",
	}
	for _, want := range required {
		found := false
		for _, command := range commands {
			if command == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("proxy output-feature was not reverted to the plain outside role, missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "lyroute-enp2s0") {
		t.Fatalf("LAN role was changed during the revert:\n%s", text)
	}
}

func TestNAT44ReconcileCommandsCarryProxyUnderlayOutputFeature(t *testing.T) {
	plan := nat44TargetPlan()
	plan.OutputFeatureInterfaces = []string{"lyroute-wan0"}
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-lan0": true},
		Outside:       map[string]bool{"lyroute-wan0": true},
		OutputFeature: map[string]bool{},
		Pool:          []nat44PoolAddressEntry{{Address: "10.1.18.24", VRF: 0, VRFSet: true}},
	}
	text := strings.Join(nat44ReconcileCommands(plan, state), "\n")
	if !strings.Contains(text, "?set interface nat44 out lyroute-wan0 del") ||
		!strings.Contains(text, "?set interface nat44 out lyroute-wan0 output-feature") {
		t.Fatalf("NAT44 plan did not carry the proxy underlay output feature:\n%s", text)
	}
}

func TestNAT44InitializeForPlanUsesProxyOutputFeature(t *testing.T) {
	plan := nat44TargetPlan()
	plan.OutputFeatureInterfaces = []string{"lyroute-wan0"}
	text := strings.Join(nat44InitializeForPlan(plan), "\n")
	if !strings.Contains(text, "?set interface nat44 out lyroute-wan0 output-feature") {
		t.Fatalf("cold-start initialization dropped the proxy output-feature role:\n%s", text)
	}
}

func TestNAT44ReconcileCommandsSkipsPresentLegacyAddressDelete(t *testing.T) {
	plan := nat44TargetPlan()
	plan.PoolAddresses = []NAT44PoolAddress{{Interface: "lyroute-wan0", Address: "10.1.18.24"}}
	state := nat44RuntimeState{
		Known:         true,
		Inside:        map[string]bool{"lyroute-lan0": true},
		Outside:       map[string]bool{"lyroute-wan0": true},
		OutputFeature: map[string]bool{"lyroute-wan0": true},
		Pool:          []nat44PoolAddressEntry{{Address: "10.1.18.24", VRFSet: true, VRF: 0}},
	}
	commands := strings.Join(nat44ReconcileCommands(plan, state), "\n")
	if strings.Contains(commands, "?nat44 add interface address lyroute-wan0 del") {
		t.Fatalf("healthy tenant pool still deleted its interface address:\n%s", commands)
	}
}

func TestApplyNAT44DoesNotReinitializeConvergedState(t *testing.T) {
	client := &nat44LifecycleClient{replies: map[string]Reply{
		"vpp.nat44.state": {
			Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{
				{Command: "show nat44 interfaces", Stdout: "lyroute-lan0 in\nlyroute-wan0 out\n"},
				{Command: "show nat44 addresses", Stdout: "10.1.18.24\n  tenant vrf: 0\n"},
			}},
		},
		"vpp.nat44-ed.snapshot": {Payload: NAT44Readback{Behavior: nat.BehaviorEndpointDependent}},
	}}
	if _, err := (Adapter{Client: client}).ApplyNAT44(context.Background(), nat44TargetPlan(), Snapshot{}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range client.operations {
		if operation.Name == "vpp.nat44.initialize" {
			t.Fatalf("converged NAT44 apply re-sent initialization: %#v", operation.VPPCtlCommands)
		}
	}
}

func TestNAT44IdempotentFailureClassification(t *testing.T) {
	for _, test := range []struct {
		command string
		stderr  string
		want    bool
	}{
		{"nat44 add address 10.1.18.24 tenant-vrf 0", "NAT address already in use", true},
		{"nat44 add interface address lyroute-wan0 del", "no such address", true},
		{"?set interface nat44 out lyroute-wan0", "already exists", true},
		{"set interface nat44: del lyroute-wan0 failed", "failed", true},
		{"nat44 add static mapping local 192.168.88.2 external 10.1.18.35", "invalid address", false},
		{"set interface state lyroute-wan0 up", "already exists", false},
	} {
		if got := nat44IdempotentFailure(test.command, test.stderr); got != test.want {
			t.Fatalf("nat44IdempotentFailure(%q, %q) = %v, want %v", test.command, test.stderr, got, test.want)
		}
	}
}
