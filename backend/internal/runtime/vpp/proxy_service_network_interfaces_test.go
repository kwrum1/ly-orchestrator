package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/proxy"
)

// proxyServiceRenamedNetwork returns the live topology for a restart where the
// service TAPs were rebuilt under a different suffix than the persisted plan.
func proxyServiceRenamedNetwork(planned proxy.ProxyServiceNetwork) proxy.ProxyServiceNetwork {
	live := planned
	live.IngressVPPInterface = "lypxinlive01"
	live.EgressVPPInterface = "lypxoutlive01"
	return live
}

func proxyServiceNetworkFakeResponses(live proxy.ProxyServiceNetwork, roles, addresses string) map[string]fakeVPPResponse {
	tableID := itoa(live.OutboundTableID)
	return map[string]fakeVPPResponse{
		"show interface": {stdout: "Name Idx State MTU (L3/IP4/IP6/MPLS) Counter Count\n" +
			"lyroute-enp4s0 1 up 9000/0/0/0\n" +
			live.IngressVPPInterface + " 7 up 9000/0/0/0\n" +
			live.EgressVPPInterface + " 8 up 9000/0/0/0\n"},
		"show nat44 interfaces":                              {stdout: roles},
		"show nat44 addresses":                               {stdout: addresses},
		"show interface address " + live.IngressVPPInterface: {stdout: live.IngressVPPInterface + " (up):\n  L3 " + live.IngressCIDR + "\n"},
		"show interface address " + live.EgressVPPInterface:  {stdout: live.EgressVPPInterface + " (up):\n  L3 " + live.EgressCIDR + "\n"},
		"show ip fib table " + tableID:                       {stdout: "ipv4-VRF:" + tableID + ", fib_index:2\n"},
		"show tap":                                           {stdout: live.IngressVPPInterface + "\n" + live.EgressVPPInterface + "\n"},
	}
}

func proxyServiceNetworkOperationFixture() Operation {
	planned := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(planned)
	return Operation{
		Name:           "vpp.proxy-service.network",
		RequestID:      "runtime-proxyname",
		Resource:       steering.EgressID,
		Payload:        steering,
		VPPCtlCommands: proxySteeringCommands(steering, nat.BehaviorEndpointDependent),
	}
}

func readCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// TestProxyServiceNetworkApplyUsesLiveInterfaceNames pins the regression: the
// persisted plan carries lypxin<old>/lypxout<old>, the live topology only has
// the renamed pair, and the operation must execute (and verify) against the
// live names instead of failing `show interface address` with retval 1.
func TestProxyServiceNetworkApplyUsesLiveInterfaceNames(t *testing.T) {
	planned := proxyServiceTestNetwork()
	live := proxyServiceRenamedNetwork(planned)
	roles := "lyroute-enp4s0 output-feature in out\n"
	addresses := proxyTestPoolAddresses(planned)

	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	binary := writeFakeVPPCTL(t, proxyServiceNetworkFakeResponses(live, roles, addresses))
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	operation := proxyServiceNetworkOperationFixture()
	reply, err := channel.Do(context.Background(), operation)
	if err != nil {
		t.Fatalf("proxy service apply rejected a renamed live TAP pair: %v", err)
	}

	envelope, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok {
		t.Fatalf("reply payload = %T", reply.Payload)
	}
	if envelope.ProxyServiceNetwork == nil ||
		envelope.ProxyServiceNetwork.IngressVPPInterface != live.IngressVPPInterface ||
		envelope.ProxyServiceNetwork.EgressVPPInterface != live.EgressVPPInterface {
		t.Fatalf("resolved service network = %#v, want %q/%q", envelope.ProxyServiceNetwork, live.IngressVPPInterface, live.EgressVPPInterface)
	}
	log := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(log, planned.IngressVPPInterface) || strings.Contains(log, planned.EgressVPPInterface) {
		t.Fatalf("executed commands still reference the stale plan names:\n%s", log)
	}
	if !strings.Contains(log, "show interface address "+live.IngressVPPInterface) ||
		!strings.Contains(log, "show interface address "+live.EgressVPPInterface) {
		t.Fatalf("readback did not use the live TAP names:\n%s", log)
	}
	if err := verifySupplementalOperation(proxyServiceNetworkVerificationOperation(operation, *envelope.ProxyServiceNetwork), envelope.CommandResults); err != nil {
		t.Fatalf("supplemental readback rejected the renamed live TAPs: %v", err)
	}
}

// TestProxyServiceNetworkApplyFailsClearlyWhenNoTAPExists proves a missing
// topology is a loud failure naming the interfaces, not a silent skip and not
// the opaque retval 1 from `show interface address`.
func TestProxyServiceNetworkApplyFailsClearlyWhenNoTAPExists(t *testing.T) {
	planned := proxyServiceTestNetwork()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	responses := map[string]fakeVPPResponse{
		"show interface": {stdout: "Name Idx State MTU (L3/IP4/IP6/MPLS) Counter Count\nlyroute-enp4s0 1 up 9000/0/0/0\n"},
	}
	binary := writeFakeVPPCTL(t, responses)
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	_, err = channel.Do(context.Background(), proxyServiceNetworkOperationFixture())
	if err == nil {
		t.Fatal("missing service TAPs must fail the apply")
	}
	for _, want := range []string{"no live service TAP after create/repair", planned.IngressVPPInterface, planned.EgressVPPInterface} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err.Error(), want)
		}
	}
	if log := strings.Join(readCommandLog(t, logPath), "\n"); strings.Contains(log, "show interface address") {
		t.Fatalf("readback ran against a missing interface:\n%s", log)
	}
}

// TestProxyServiceNetworkCleanupIsIdempotentWhenTAPsAreGone covers rollback and
// reconciliation cleanup: the plan name is stale because the TAPs were already
// removed, and that must complete instead of failing.
func TestProxyServiceNetworkCleanupIsIdempotentWhenTAPsAreGone(t *testing.T) {
	planned := proxyServiceTestNetwork()
	steering := proxyServiceTestSteering(planned)
	operation := Operation{
		Name:           "vpp.proxy-service.network.rollback-delete",
		RequestID:      "runtime-proxyname-rollback",
		Resource:       steering.EgressID,
		Payload:        steering,
		VPPCtlCommands: proxySteeringDeleteCommands(steering),
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":        {stdout: "Name Idx State MTU (L3/IP4/IP6/MPLS) Counter Count\nlyroute-enp4s0 1 up 9000/0/0/0\n"},
		"show nat44 interfaces": {stdout: "lyroute-enp4s0 out\n"},
		"show nat44 addresses":  {stdout: ""},
		"show tap":              {stdout: ""},
	})
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatalf("cleanup of already-removed TAPs failed: %v", err)
	}
	log := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(log, "show interface address") {
		t.Fatalf("cleanup read back an interface that does not exist:\n%s", log)
	}
	if !strings.Contains(log, "show tap") {
		t.Fatalf("cleanup did not leave an inventory readback:\n%s", log)
	}
}

// TestProxyServiceNetworkRepeatApplyIsSilent proves a converged second apply
// emits no TAP creation, renaming or NAT44 mutation.
func TestProxyServiceNetworkRepeatApplyIsSilent(t *testing.T) {
	planned := proxyServiceTestNetwork()
	live := proxyServiceRenamedNetwork(planned)
	roles := "lyroute-enp4s0 output-feature in out\n"
	addresses := proxyTestPoolAddresses(planned)

	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	binary := writeFakeVPPCTL(t, proxyServiceNetworkFakeResponses(live, roles, addresses))
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	operation := proxyServiceNetworkOperationFixture()
	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	first := readCommandLog(t, logPath)
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatalf("repeat apply failed: %v", err)
	}
	second := readCommandLog(t, logPath)

	joined := strings.Join(second, "\n")
	for _, forbidden := range []string{"create tap", "set interface name", "nat44 add address", "set interface nat44"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("repeat apply replayed %q:\n%s", forbidden, joined)
		}
	}
	// The second pass may serve cached read-only inventory, so compare the
	// mutations only: a converged apply must re-assert the same idempotent
	// address/route block and add nothing.
	firstMutations, secondMutations := writeCommands(first), writeCommands(second)
	if strings.Join(firstMutations, "\n") != strings.Join(secondMutations, "\n") {
		t.Fatalf("repeat apply produced different mutations:\nfirst:\n%s\nsecond:\n%s", strings.Join(firstMutations, "\n"), strings.Join(secondMutations, "\n"))
	}
}

// writeCommands keeps only the mutating CLI lines of a fake command log.
func writeCommands(log []string) []string {
	mutations := make([]string, 0, len(log))
	for _, line := range log {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "show ") {
			continue
		}
		mutations = append(mutations, line)
	}
	return mutations
}

// TestResolveProxyServiceInterfaceNameRejectsAmbiguousTopology keeps the
// resolver from guessing when several service TAPs are live.
func TestResolveProxyServiceInterfaceNameRejectsAmbiguousTopology(t *testing.T) {
	live := map[string]bool{"lypxinAAAAAA": true, "lypxinBBBBBB": true, "lypxoutAAAAAA": true}
	_, present, err := resolveProxyServiceInterfaceName(live, "lypxinCCCCCC", proxyServiceIngressPrefix)
	if err == nil || present {
		t.Fatalf("ambiguous ingress topology resolved to present=%t, err=%v", present, err)
	}
	if !strings.Contains(err.Error(), "lypxinAAAAAA") || !strings.Contains(err.Error(), "lypxinBBBBBB") {
		t.Fatalf("ambiguity error does not list the candidates: %v", err)
	}
}
