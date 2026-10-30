package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/vppfake"
)

func proxyServiceTestNetwork() proxy.ProxyServiceNetwork {
	network := proxy.ServiceNetworkForEgressID("proxy-media")
	network.NATPoolAddress = "10.1.18.53"
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	return network
}

func proxyServiceTestSteering(network proxy.ProxyServiceNetwork) proxy.VPPSteeringInstruction {
	return proxy.VPPSteeringInstruction{
		EgressID:       network.EgressID,
		UnderlayWANID:  "j1900-pub",
		TargetKind:     "vpp.proxy-service.network",
		ServiceNetwork: network,
		UnderlayRoute:  network.UnderlayRoute,
	}
}

func TestFilterProxyNATCommandsSkipsConvergedMutations(t *testing.T) {
	network := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(network)
	commands := proxySteeringCommands(steering, nat.BehaviorEndpointDependent)
	roles := "lyroute-enp4s0 output-feature in out\n"
	addresses := network.NATPoolAddress + "\n  tenant vrf: " + itoa(network.OutboundTableID) + "\n"
	filtered := strings.Join(filterProxyNATCommands(commands, roles, addresses, steering), "\n")
	for _, forbidden := range []string{
		"nat44 plugin enable",
		"nat44 add address " + network.NATPoolAddress,
		"set interface nat44 in " + network.EgressVPPInterface,
		"set interface nat44 out " + network.EgressVPPInterface,
		"set interface nat44 out lyroute-enp4s0 output-feature",
	} {
		if strings.Contains(filtered, forbidden) {
			t.Fatalf("converged proxy apply still emits %q:\n%s", forbidden, filtered)
		}
	}
	if !strings.Contains(filtered, "show ip fib table "+itoa(network.OutboundTableID)) {
		t.Fatalf("filtered apply lost its readback:\n%s", filtered)
	}
}

func TestFilterProxyNATCommandsKeepsFirstApplyMutations(t *testing.T) {
	network := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(network)
	commands := proxySteeringCommands(steering, nat.BehaviorEndpointDependent)
	filtered := strings.Join(filterProxyNATCommands(commands, "lyroute-enp4s0 out", "", steering), "\n")
	for _, required := range []string{
		"nat44 add address " + network.NATPoolAddress,
		"set interface nat44 out lyroute-enp4s0 output-feature",
	} {
		if !strings.Contains(filtered, required) {
			t.Fatalf("first proxy apply lost %q:\n%s", required, filtered)
		}
	}
	if strings.Contains(filtered, "set interface nat44 in "+network.EgressVPPInterface) {
		t.Fatalf("clean egress still emitted a delete:\n%s", filtered)
	}
}

// proxyFakeShow runs one read command against the deterministic VPP fake.
func proxyFakeShow(t *testing.T, state *vppfake.State, command string) string {
	t.Helper()
	stdout, stderr, code := vppfake.Execute(state, command)
	if code != 0 || strings.TrimSpace(stderr) != "" {
		t.Fatalf("fake vppctl %q = %q/%q/%d", command, stdout, stderr, code)
	}
	return stdout
}

// proxyExecuteCommands replays an operation command list against the fake and
// returns the same typed results a live vppctl channel would report.
func proxyExecuteCommands(t *testing.T, state *vppfake.State, commands []string) []VPPCTLCommandResult {
	t.Helper()
	results := make([]VPPCTLCommandResult, 0, len(commands))
	for _, raw := range commands {
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		stdout, stderr, code := vppfake.Execute(state, command)
		results = append(results, VPPCTLCommandResult{Command: command, Stdout: stdout, Stderr: stderr, Retval: int32(code)})
	}
	return results
}

func proxyUnderlayTestState() *vppfake.State {
	state := vppfake.NewState()
	state.Interfaces = []*vppfake.Interface{
		{Name: "lyroute-enp2s0", NATIn: true},
		{Name: "lyroute-enp4s0", NATOut: true},
	}
	return state
}

func proxyTestPoolAddresses(network proxy.ProxyServiceNetwork) string {
	return network.NATPoolAddress + "\n  tenant VRF: " + itoa(network.OutboundTableID) + "\n"
}

// TestProxyUnderlayRoleIsReplacedBeforeTheAdd pins the VPP 26.06 contract: the
// combined role cannot be added on top of a live plain outside role, so the
// plain role is removed first and the combined add happens before the
// operation's own `show nat44 interfaces` readback.
func TestProxyUnderlayRoleIsReplacedBeforeTheAdd(t *testing.T) {
	network := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(network)
	commands := proxySteeringCommands(steering, nat.BehaviorEndpointDependent)
	filtered := strings.Join(filterProxyNATCommands(commands, "lyroute-enp4s0 out\n", "", steering), "\n")

	delIndex := strings.Index(filtered, "?set interface nat44 out lyroute-enp4s0 del")
	addIndex := strings.Index(filtered, "?set interface nat44 out lyroute-enp4s0 output-feature")
	if delIndex < 0 || addIndex < 0 || delIndex > addIndex {
		t.Fatalf("plain outside role was not replaced before the output-feature add:\n%s", filtered)
	}
	showIndex := strings.Index(filtered, "show nat44 interfaces")
	if showIndex < 0 || showIndex < addIndex {
		t.Fatalf("role change was emitted after the readback:\n%s", filtered)
	}
}

// TestProxyUnderlayApplyPassesReadback is the end-to-end contract: running the
// command list a new egress produces installs `output-feature in out` on the
// underlay WAN, and the supplemental readback that previously failed now
// proves it.
func TestProxyUnderlayApplyPassesReadback(t *testing.T) {
	network := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(network)
	state := proxyUnderlayTestState()
	addresses := proxyTestPoolAddresses(network)

	roles := proxyFakeShow(t, state, "show nat44 interfaces")
	commands := filterProxyNATCommands(proxySteeringCommands(steering, nat.BehaviorEndpointDependent), roles, addresses, steering)
	proxyExecuteCommands(t, state, commands)

	live := proxyFakeShow(t, state, "show nat44 interfaces")
	if !strings.Contains(live, "lyroute-enp4s0 output-feature in out") {
		t.Fatalf("underlay WAN role after apply = %q, want output-feature in out", live)
	}
	results := []VPPCTLCommandResult{
		{Command: "show interface address " + network.IngressVPPInterface, Stdout: network.IngressVPPInterface + " " + network.IngressCIDR},
		{Command: "show interface address " + network.EgressVPPInterface, Stdout: network.EgressVPPInterface + " " + network.EgressCIDR},
		{Command: "show ip fib table " + itoa(network.OutboundTableID), Stdout: "ipv4-VRF:" + itoa(network.OutboundTableID)},
		{Command: "show nat44 addresses", Stdout: addresses},
		{Command: "show nat44 interfaces", Stdout: live},
		{Command: "show tap", Stdout: network.IngressVPPInterface},
	}
	operation := Operation{Name: "vpp.proxy-service.network", Resource: steering.EgressID, Payload: steering}
	if err := verifySupplementalOperation(operation, results); err != nil {
		t.Fatalf("proxy supplemental readback rejected the converged role: %v", err)
	}
}

// TestProxyUnderlayRepeatApplyIsSilent proves a second apply emits no NAT44
// role mutation and leaves the role inventory untouched.
func TestProxyUnderlayRepeatApplyIsSilent(t *testing.T) {
	network := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(network)
	state := proxyUnderlayTestState()
	addresses := proxyTestPoolAddresses(network)

	first := filterProxyNATCommands(proxySteeringCommands(steering, nat.BehaviorEndpointDependent), proxyFakeShow(t, state, "show nat44 interfaces"), addresses, steering)
	proxyExecuteCommands(t, state, first)
	live := proxyFakeShow(t, state, "show nat44 interfaces")

	second := filterProxyNATCommands(proxySteeringCommands(steering, nat.BehaviorEndpointDependent), live, addresses, steering)
	for _, raw := range second {
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
		if isProxyUnderlayRoleCommand(command, "lyroute-enp4s0") {
			t.Fatalf("repeat apply replayed the underlay role command %q:\n%s", command, strings.Join(second, "\n"))
		}
	}
	proxyExecuteCommands(t, state, second)
	if got := proxyFakeShow(t, state, "show nat44 interfaces"); got != live {
		t.Fatalf("repeat apply changed the NAT44 role inventory:\nbefore:\n%s\nafter:\n%s", live, got)
	}
}

// TestProxyUnderlayRoleRevertsAfterEgressRemoval covers removing the egress:
// the combined role is torn down and the gateway's plain outside role returns.
func TestProxyUnderlayRoleRevertsAfterEgressRemoval(t *testing.T) {
	state := proxyUnderlayTestState()
	live := proxyFakeShow(t, state, "show nat44 interfaces")
	if !strings.Contains(live, "lyroute-enp4s0 out") {
		t.Fatalf("fake WAN outside role missing: %q", live)
	}
	proxyExecuteCommands(t, state, []string{"?set interface nat44 out lyroute-enp4s0 del", "?set interface nat44 out lyroute-enp4s0 output-feature"})
	live = proxyFakeShow(t, state, "show nat44 interfaces")
	if !strings.Contains(live, "lyroute-enp4s0 output-feature in out") {
		t.Fatalf("proxy role was not installed: %q", live)
	}

	revert := proxyUnderlayRoleRevertCommands("lyroute-enp4s0", live)
	if len(revert) == 0 {
		t.Fatal("removing the egress left the output-feature role behind")
	}
	proxyExecuteCommands(t, state, revert)
	reverted := proxyFakeShow(t, state, "show nat44 interfaces")
	if !strings.Contains(reverted, "lyroute-enp4s0 out") || strings.Contains(reverted, "output-feature") {
		t.Fatalf("underlay role after egress removal = %q, want the plain outside role", reverted)
	}
	if again := proxyUnderlayRoleRevertCommands("lyroute-enp4s0", reverted); len(again) != 0 {
		t.Fatalf("repeated removal replayed role commands: %#v", again)
	}
}

// TestProxyUnderlayRoleFailureCarriesVPPReason keeps the real VPP reason in
// the error instead of reporting a generic snapshot failure two minutes later.
func TestProxyUnderlayRoleFailureCarriesVPPReason(t *testing.T) {
	reply := Reply{Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{
		{Command: "set interface nat44 out lyroute-enp4s0 output-feature", Stdout: "set interface nat44: add lyroute-enp4s0 failed\n", Retval: 0},
	}}}
	err := verifyProxyUnderlayRoleCommand(reply, "lyroute-enp4s0")
	if err == nil {
		t.Fatal("silently dropped output-feature role was reported as success")
	}
	if !strings.Contains(err.Error(), "add lyroute-enp4s0 failed") || !strings.Contains(err.Error(), "output-feature") {
		t.Fatalf("error lost the real VPP reason: %v", err)
	}
	if err := verifyProxyUnderlayRoleCommand(Reply{Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{
		{Command: "set interface nat44 out lyroute-enp4s0 output-feature", Stdout: "", Retval: 0},
	}}}, "lyroute-enp4s0"); err != nil {
		t.Fatalf("successful role command reported as failure: %v", err)
	}
}
