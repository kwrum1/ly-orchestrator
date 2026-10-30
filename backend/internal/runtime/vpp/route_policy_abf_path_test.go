package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/selfheal"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

const (
	proxyABFPolicyID = 18784
	proxyABFACLID    = 18082
	proxyABFTableID  = 33162
)

func proxyEgressRoutePolicy() trafficpolicy.RoutePolicy {
	return trafficpolicy.RoutePolicy{
		ID:       "lan-proxy-test",
		Priority: 10,
		Action:   "route",
		Egress:   "egress-proxy",
		Match:    trafficpolicy.Match{Sources: []string{"192.168.99.0/24"}, Destinations: []string{"1.1.1.1/32"}},
		Path:     &trafficpolicy.WANPath{VPPInterface: "lypxinf9d5aa", NextHop: "198.18.168.118"},
	}
}

func proxyABFSpec(via string) routePolicyVPPCTLSpec {
	return routePolicyVPPCTLSpec{
		policyID: proxyABFPolicyID,
		aclID:    proxyABFACLID,
		tableID:  proxyABFTableID,
		priority: 10,
		tag:      "ly-route-lan-proxy-test",
		ingress:  "lyroute-enp4s0",
		via:      via,
		apply:    true,
		policy:   proxyEgressRoutePolicy(),
	}
}

func TestResolveProxyABFPathUsesTheLiveServiceTAP(t *testing.T) {
	// The plan froze `lypxinold`, but the egress service network was rebuilt
	// with a different TAP id. The /30 proves which live TAP belongs to this
	// egress, so the ABF must follow the live name.
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlyroute-enp4s0 2 up 9000/0/0/0\nlypxinnew 7 up 1500/0/0/0\n"},
		"show interface address": {stdout: "lyroute-enp4s0 (up):\n  L3 192.168.99.1/24\nlypxinnew (up):\n  L3 198.18.168.117/30\n"},
	})
	channel := &vppctlChannel{binary: binary}
	resolved, from, to, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), proxyABFSpec("198.18.168.118 lypxinold"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.via != "198.18.168.118 lypxinnew" {
		t.Fatalf("resolved via = %q, want the live service TAP", resolved.via)
	}
	if from != "lypxinold" || to != "lypxinnew" {
		t.Fatalf("rewrite = %q -> %q", from, to)
	}
}

func TestResolveProxyABFPathFailsWhenTheEgressIsNotReady(t *testing.T) {
	// No proxy TAP exists at all: installing the planned path would make VPP
	// reject the ABF (or, worse, leave the policy without a path). The apply
	// must fail so the service network can be created and retried.
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlyroute-enp4s0 2 up 9000/0/0/0\nlyroute-enp5s0 4 up 9000/0/0/0\n"},
		"show interface address": {stdout: "lyroute-enp4s0 (up):\n  L3 192.168.99.1/24\nlyroute-enp5s0 (up):\n  L3 10.1.18.25/24\n"},
	})
	channel := &vppctlChannel{binary: binary}
	_, _, _, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), proxyABFSpec("198.18.168.118 lypxinf9d5aa"))
	if err == nil || !strings.Contains(err.Error(), "is not live") {
		t.Fatalf("error = %v, want service-network-not-ready failure", err)
	}
}

func TestResolveProxyABFPathRejectsAForeignServiceNetwork(t *testing.T) {
	// A TAP with the service prefix exists, but it carries another egress's
	// /30. Matching it by name alone would steer this policy into a network
	// that has no route back.
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlypxinother 8 up 1500/0/0/0\n"},
		"show interface address": {stdout: "lypxinother (up):\n  L3 198.18.200.117/30\n"},
	})
	channel := &vppctlChannel{binary: binary}
	_, _, _, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), proxyABFSpec("198.18.168.118 lypxinf9d5aa"))
	if err == nil || !strings.Contains(err.Error(), "is not live") {
		t.Fatalf("error = %v, want foreign-network refusal", err)
	}
}

func TestResolveProxyABFPathRejectsAnUnconfiguredPlannedTAP(t *testing.T) {
	// The planned TAP exists but the address step has not run yet. VPP would
	// accept the ABF and blackhole the policy, so the apply must fail instead.
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlypxinf9d5aa 7 up 1500/0/0/0\n"},
		"show interface address": {stdout: "lypxinf9d5aa (up):\n"},
	})
	channel := &vppctlChannel{binary: binary}
	_, _, _, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), proxyABFSpec("198.18.168.118 lypxinf9d5aa"))
	if err == nil || !strings.Contains(err.Error(), "does not carry") {
		t.Fatalf("error = %v, want unconfigured-service-network failure", err)
	}
}

func TestValidateRoutePolicyABFViaRejectsLocalAndForeignPaths(t *testing.T) {
	if err := validateRoutePolicyABFVia(proxyABFSpec("local")); err == nil || !strings.Contains(err.Error(), "local/receive") {
		t.Fatalf("via local error = %v, want refusal", err)
	}
	if err := validateRoutePolicyABFVia(proxyABFSpec("10.1.18.1 lyroute-enp5s0")); err == nil || !strings.Contains(err.Error(), "not a service path") {
		t.Fatalf("WAN via error = %v, want refusal", err)
	}
	if err := validateRoutePolicyABFVia(proxyABFSpec("198.18.168.118 lypxinother")); err == nil || !strings.Contains(err.Error(), "planned service TAP") {
		t.Fatalf("foreign TAP error = %v, want refusal", err)
	}
	if err := validateRoutePolicyABFVia(proxyABFSpec("198.18.168.118 lypxinf9d5aa")); err != nil {
		t.Fatalf("valid service path was refused: %v", err)
	}
	// Deletes must stay able to remove a live `local` path: that is the repair.
	deleteSpec := proxyABFSpec("local")
	deleteSpec.apply = false
	if err := validateRoutePolicyABFVia(deleteSpec); err != nil {
		t.Fatalf("delete of a local path was refused: %v", err)
	}
}

// writeProxyABFStateVPPCTL models one live ABF policy with a single path. The
// path is `receive`/`dpo-receive` unless the `restored` marker exists, in which
// case it is the wanted service path.
func writeProxyABFStateVPPCTL(t *testing.T) (binary, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	path := filepath.Join(t.TempDir(), "vppctl")
	template := `#!/bin/sh
state='{{state}}'
cmd="$*"
case "$cmd" in
  "show abf policy {{policy}}")
    if [ -f "$state/deleted" ]; then
      echo "Invalid policy ID:{{policy}}"
      exit 0
    fi
    printf 'abf:[4]: policy:{{policy}} acl:{{acl}}\n'
    printf '     path-list:[96] locks:2 flags:shared,no-uRPF, uPRF-list:83 len:1 itfs:[7, ]\n'
    if [ -f "$state/restored" ]; then
      printf '      path:[111] pl-index:96 ip4 weight=1 pref=0 attached-nexthop:  oper-flags:resolved,\n'
      printf '        198.18.168.118 lypxinf9d5aa\n'
      printf '      [@0]: ipv4 via 198.18.168.118 lypxinf9d5aa: mtu:1500\n'
    else
      printf '      path:[111] pl-index:96 ip4 weight=1 pref=0 receive:  oper-flags:resolved, cfg-flags:local,\n'
      printf '        [@0]: dpo-receive\n'
    fi;;
  "show abf policy")
    if [ -f "$state/deleted" ]; then :; else printf 'abf:[4]: policy:{{policy}} acl:{{acl}}\n'; fi;;
  "show acl-plugin acl"|"show acl-plugin acl index {{acl}}")
    if [ -f "$state/acl-deleted" ]; then :; else
      printf 'acl-index {{acl}} count 1 tag {ly-route-lan-proxy-test}\n'
      printf '  0: ipv4 permit src 0.0.0.0/0 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535\n'
    fi;;
  "show abf attach lyroute-enp4s0")
    if [ -f "$state/detached" ]; then :; else printf ' ipv4 abf-interface-attach: policy:{{policy}} priority:10\n'; fi;;
  "show ip fib summary")
    printf 'ipv4-VRF:{{table}}, fib_index:6, flow hash:[src dst]\n';;
  "show ip fib table {{table}}") :;;
  "show ly-route pre-nat-route") printf 'enabled 1 interface lyroute-enp4s0 lan-prefix 192.168.99.0/24 rules 0 radix-nodes 0\n';;
  "set ly-route pre-nat-route del id {{policy}}") :;;
  "abf attach ip4 del policy {{policy}} lyroute-enp4s0")
    touch "$state/detached";;
  "abf policy del id {{policy}} acl {{acl}} via local"|"abf policy del id {{policy}} acl {{acl}} via 198.18.168.118 lypxinf9d5aa"|"abf policy del id {{policy}}")
    touch "$state/deleted";;
  "delete acl-plugin acl index {{acl}}")
    touch "$state/acl-deleted";;
  "ip route del table {{table}} 0.0.0.0/0"|"ip route del table {{table}} 0.0.0.0/32"|"ip table del {{table}}") :;;
  *) echo "unsupported: $cmd" >&2; exit 64;;
esac
`
	script := strings.NewReplacer(
		"{{state}}", stateDir,
		"{{policy}}", fmt.Sprintf("%d", proxyABFPolicyID),
		"{{acl}}", fmt.Sprintf("%d", proxyABFACLID),
		"{{table}}", fmt.Sprintf("%d", proxyABFTableID),
	).Replace(template)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, stateDir
}

func TestRoutePolicySelfHealsASingleWrongABFPath(t *testing.T) {
	// E35 repairs a policy whose path list is empty. A `local`/receive path is
	// the same class of defect with one path: every readback sees a policy, but
	// the matched LAN flow is punted to the host stack. This patch must treat it
	// as a repair and remove it before the correct path lands.
	binary, _ := writeProxyABFStateVPPCTL(t)
	channel := &vppctlChannel{binary: binary}
	ctx, recorder := selfheal.WithRecorder(context.Background())

	results, err := channel.removeRoutePolicyState(ctx, Operation{
		Name:     "vpp.route-policy",
		Resource: "lan-proxy-test",
	}, proxyABFSpec("198.18.168.118 lypxinf9d5aa"))
	if err != nil {
		t.Fatalf("single wrong ABF path was not repaired: %v", err)
	}
	joined := strings.Join(commandTexts(results), "\n")
	if !strings.Contains(joined, fmt.Sprintf("abf policy del id %d acl %d via local", proxyABFPolicyID, proxyABFACLID)) {
		t.Fatalf("receive path was not deleted through its live target:\n%s", joined)
	}
	if !strings.Contains(joined, fmt.Sprintf("abf attach ip4 del policy %d", proxyABFPolicyID)) {
		t.Fatalf("stale attach was not removed:\n%s", joined)
	}
	got := recorder.Items()
	if len(got) != 1 || got[0] != "abf" {
		t.Fatalf("self-heal scopes = %v, want [abf]", got)
	}
}

func TestRoutePolicyKeepsAnAlreadyCorrectABFPath(t *testing.T) {
	// A second apply of the correct path must not record a repair.
	binary, stateDir := writeProxyABFStateVPPCTL(t)
	if err := os.WriteFile(filepath.Join(stateDir, "restored"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	channel := &vppctlChannel{binary: binary}
	ctx, recorder := selfheal.WithRecorder(context.Background())
	if _, err := channel.removeRoutePolicyState(ctx, Operation{
		Name:     "vpp.route-policy",
		Resource: "lan-proxy-test",
	}, proxyABFSpec("198.18.168.118 lypxinf9d5aa")); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if got := recorder.Items(); len(got) != 0 {
		t.Fatalf("self-heal recorded for an already correct path: %v", got)
	}
}
