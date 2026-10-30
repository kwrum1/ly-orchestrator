package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/selfheal"
)

type abfSelfHealFixture struct {
	resource string
	policyID int
	aclID    int
	tableID  int
	tag      string
}

func newABFSelfHealFixture(resource string) abfSelfHealFixture {
	return abfSelfHealFixture{
		resource: resource,
		policyID: stableID("route-abf:"+resource, 10000, 8999),
		aclID:    stableID("route-acl:"+resource, 10000, 49999),
		tableID:  stableID("route-table:"+resource, 50000, 49999),
		tag:      "ly-route-" + safeTag(resource),
	}
}

func (fixture abfSelfHealFixture) operation() Operation {
	return Operation{
		Name:     "vpp.route-policy",
		Resource: fixture.resource,
		VPPCtlCommands: []string{
			fmt.Sprintf("set acl-plugin acl index %d permit src 0.0.0.0/0 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535 tag %s", fixture.aclID, fixture.tag),
			fmt.Sprintf("abf policy add id %d acl %d via 198.18.0.1 lypxinffdc88", fixture.policyID, fixture.aclID),
			fmt.Sprintf("abf attach ip4 policy %d priority 10 lyroute-ens34", fixture.policyID),
			fmt.Sprintf("show abf policy %d", fixture.policyID),
			fmt.Sprintf("show acl-plugin acl index %d", fixture.aclID),
			"show abf attach lyroute-ens34",
			fmt.Sprintf("show ip fib table %d", fixture.tableID),
		},
	}
}

func writeABFSelfHealVPPCTL(t *testing.T, fixture abfSelfHealFixture, aclTag string) string {
	t.Helper()
	stateDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "vppctl")
	template := `#!/bin/sh
state='{{state}}'
cmd="$*"
case "$cmd" in
  "show abf policy {{policy}}")
    if [ -f "$state/deleted" ]; then
      echo "Invalid policy ID:{{policy}}"
    elif [ -f "$state/repaired" ]; then
      printf 'abf:[0]: policy:{{policy}} acl:{{acl}}\n path-list:[17] locks:1 flags:shared len:1\n  path:[21] pl-index:17 ip4 weight=1 pref=0\n     [@0]: ipv4 via 198.18.0.1 lypxinffdc88\n'
    else
      printf 'abf:[0]: policy:{{policy}} acl:{{acl}}\n no forwarding\n'
    fi;;
  "show abf policy")
    if [ -f "$state/deleted" ]; then :; elif [ -f "$state/repaired" ]; then printf 'abf:[0]: policy:{{policy}} acl:{{acl}}\n'; else printf 'abf:[0]: policy:{{policy}} acl:{{acl}}\n no forwarding\n'; fi;;
  "show acl-plugin acl"|"show acl-plugin acl index {{acl}}")
    if [ ! -f "$state/acl_deleted" ]; then
      cat <<'ACL'
acl-index {{acl}} count 1 tag {{{tag}}}
  0: ipv4 permit src 0.0.0.0/0 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535
ACL
    fi;;
  "show abf attach lyroute-ens34") :;;
  "show ip fib summary") printf 'ipv4-VRF:{{table}}, fib_index:6, flow hash:[src dst]\n';;
  "show ip fib table {{table}}") :;;
  "show ly-route pre-nat-route") :;;
  "set ly-route pre-nat-route del id {{policy}}") :;;
  "abf policy add id {{policy}} acl {{acl}} via 198.18.0.1 lypxinffdc88") touch "$state/repaired";;
  "abf attach ip4 del policy {{policy}} lyroute-ens34") :;;
  "abf policy del id {{policy}} acl {{acl}} via 198.18.0.1 lypxinffdc88") touch "$state/deleted";;
  "delete acl-plugin acl index {{acl}}") touch "$state/acl_deleted";;
  *) echo "unsupported: $cmd" >&2; exit 64;;
esac
`
	script := strings.NewReplacer(
		"{{state}}", stateDir,
		"{{policy}}", fmt.Sprintf("%d", fixture.policyID),
		"{{acl}}", fmt.Sprintf("%d", fixture.aclID),
		"{{table}}", fmt.Sprintf("%d", fixture.tableID),
		"{{tag}}", aclTag,
	).Replace(template)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOwnedABFPolicyWithZeroPathsIsRepairedThenReplaced(t *testing.T) {
	fixture := newABFSelfHealFixture("lan-proxy-test")
	binary := writeABFSelfHealVPPCTL(t, fixture, fixture.tag)
	channel := &vppctlChannel{binary: binary}
	ctx, recorder := selfheal.WithRecorder(context.Background())

	results, err := channel.removeRoutePolicyState(ctx, fixture.operation(), routePolicyVPPCTLSpec{
		policyID: fixture.policyID,
		aclID:    fixture.aclID,
		tableID:  fixture.tableID,
		tag:      fixture.tag,
		ingress:  "lyroute-ens34",
		via:      "198.18.0.1 lypxinffdc88",
		apply:    true,
	})
	if err != nil {
		t.Fatalf("owned 0-path ABF policy was not self-healed: %v", err)
	}
	joined := strings.Join(commandTexts(results), "\n")
	if !strings.Contains(joined, fmt.Sprintf("abf policy add id %d acl %d via 198.18.0.1 lypxinffdc88", fixture.policyID, fixture.aclID)) {
		t.Fatalf("missing path was not repaired before deletion:\n%s", joined)
	}
	if !strings.Contains(joined, fmt.Sprintf("abf policy del id %d", fixture.policyID)) {
		t.Fatalf("repaired policy was not deleted:\n%s", joined)
	}
	if got := recorder.Items(); len(got) != 1 || got[0] != "abf" {
		t.Fatalf("self-healed scopes = %v, want [abf]", got)
	}
}

func TestForeignABFPolicyWithZeroPathsIsRefused(t *testing.T) {
	fixture := newABFSelfHealFixture("lan-proxy-test")
	binary := writeABFSelfHealVPPCTL(t, fixture, "foreign-service-acl")
	channel := &vppctlChannel{binary: binary}

	results, err := channel.removeRoutePolicyState(context.Background(), fixture.operation(), routePolicyVPPCTLSpec{
		policyID: fixture.policyID,
		aclID:    fixture.aclID,
		tableID:  fixture.tableID,
		tag:      fixture.tag,
		ingress:  "lyroute-ens34",
		via:      "198.18.0.1 lypxinffdc88",
		apply:    true,
	})
	if err == nil {
		t.Fatal("foreign 0-path ABF policy was repaired instead of refused")
	}
	if !strings.Contains(err.Error(), "not owned by ly-route") {
		t.Fatalf("error = %v, want explicit ownership refusal", err)
	}
	joined := strings.Join(commandTexts(results), "\n")
	if strings.Contains(joined, "abf policy add") || strings.Contains(joined, "abf policy del") {
		t.Fatalf("foreign policy was mutated:\n%s", joined)
	}
}

func commandTexts(results []VPPCTLCommandResult) []string {
	commands := make([]string, 0, len(results))
	for _, result := range results {
		commands = append(commands, strings.TrimSpace(result.Command))
	}
	return commands
}

func TestRoutePolicyUsesLiveProxyServiceInterface(t *testing.T) {
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlypxinnew 7 up 9000/0/0/0\n"},
		"show interface address": {stdout: "lypxinnew (up):\n  L3 198.18.0.0/30\n"},
	})
	channel := &vppctlChannel{binary: binary}
	spec := routePolicyVPPCTLSpec{policyID: 11198, via: "198.18.0.1 lypxinold", apply: true}
	resolved, from, to, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.via != "198.18.0.1 lypxinnew" || from != "lypxinold" || to != "lypxinnew" {
		t.Fatalf("resolved via=%q from=%q to=%q", resolved.via, from, to)
	}
	rewritten := replaceVPPCTLInterfaceToken([]string{"abf policy add id 1 acl 2 via 198.18.0.1 lypxinold"}, from, to)
	if got := rewritten[0]; got != "abf policy add id 1 acl 2 via 198.18.0.1 lypxinnew" {
		t.Fatalf("command rewrite = %q", got)
	}
}

func TestRoutePolicyAmbiguousProxyServiceInterfaceFailsClosed(t *testing.T) {
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show interface":         {stdout: "Name Idx State MTU\nlypxinone 7 up 9000/0/0/0\nlypxintwo 8 up 9000/0/0/0\n"},
		"show interface address": {stdout: "lypxinone (up):\n  L3 198.18.0.0/30\nlypxintwo (up):\n  L3 198.18.0.0/30\n"},
	})
	channel := &vppctlChannel{binary: binary}
	spec := routePolicyVPPCTLSpec{policyID: 11198, via: "198.18.0.1 lypxinold", apply: true}
	_, _, _, err := channel.resolveRoutePolicyLiveProxyPath(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatalf("error = %v, want ambiguous-interface failure", err)
	}
}

func TestACLInventoryTag(t *testing.T) {
	output := "acl-index 7 count 1 tag {ly-route-lan_proxy_test}\n  0: ipv4 permit\n"
	if got := aclInventoryTag(output, 7); got != "ly-route-lan_proxy_test" {
		t.Fatalf("tag = %q", got)
	}
	if got := aclInventoryTag(output, 8); got != "" {
		t.Fatalf("tag of absent ACL = %q", got)
	}
}
