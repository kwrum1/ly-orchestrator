package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoutePolicyDeleteDetachesEveryInterfaceAndClearsClassifier locks in the
// teardown order for a proxy policy: detach from every interface, delete the
// ABF policy/ACL, remove the pre-NAT rule, and only then drop the tenant table.
func TestRoutePolicyDeleteDetachesEveryInterfaceAndClearsClassifier(t *testing.T) {
	fixture := newABFSelfHealFixture("lan-proxy-test")
	binary, state := writeRoutePolicyDeleteVPPCTL(t, fixture, "attached")
	channel := &vppctlChannel{binary: binary}

	results, err := channel.removeRoutePolicyState(context.Background(), fixture.operation(), routePolicyVPPCTLSpec{
		policyID: fixture.policyID,
		aclID:    fixture.aclID,
		tableID:  fixture.tableID,
		tag:      fixture.tag,
		ingress:  "lyroute-enp4s0",
		apply:    false,
	})
	if err != nil {
		t.Fatalf("delete cleanup failed: %v", err)
	}
	joined := strings.Join(commandTexts(results), "\n")
	for _, want := range []string{
		fmt.Sprintf("abf attach ip4 del policy %d lyroute-enp3s0", fixture.policyID),
		fmt.Sprintf("abf attach ip4 del policy %d lyroute-enp4s0", fixture.policyID),
		fmt.Sprintf("abf policy del id %d acl %d via 198.18.168.118 lypxinf9d5aa", fixture.policyID, fixture.aclID),
		fmt.Sprintf("set ly-route pre-nat-route del id %d", fixture.policyID),
		fmt.Sprintf("ip table del %d", fixture.tableID),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cleanup is missing %q:\n%s", want, joined)
		}
	}
	if fileExists(filepath.Join(state, "prenat")) {
		t.Fatal("pre-NAT classifier rule survived deletion")
	}
	if fileExists(filepath.Join(state, "table")) {
		t.Fatal("tenant table survived deletion")
	}
	if fileExists(filepath.Join(state, "abf")) || fileExists(filepath.Join(state, "acl")) {
		t.Fatal("ABF policy/ACL survived deletion")
	}
	if fileHasContent(filepath.Join(state, "attach")) {
		t.Fatal("ABF attachment survived deletion")
	}
}

// TestRoutePolicyDeleteUsesBareFormForDanglingPath covers the E36 shape: the
// proxy TAP is already gone, so the live path cannot be reconstructed. VPP
// still accepts `abf policy del id <id>` and the readback must prove it.
func TestRoutePolicyDeleteUsesBareFormForDanglingPath(t *testing.T) {
	fixture := newABFSelfHealFixture("lan-proxy-test")
	binary, state := writeRoutePolicyDeleteVPPCTL(t, fixture, "dangling")
	channel := &vppctlChannel{binary: binary}

	results, err := channel.removeRoutePolicyState(context.Background(), fixture.operation(), routePolicyVPPCTLSpec{
		policyID: fixture.policyID,
		aclID:    fixture.aclID,
		tableID:  fixture.tableID,
		tag:      fixture.tag,
		ingress:  "lyroute-enp4s0",
		apply:    false,
	})
	if err != nil {
		t.Fatalf("dangling ABF cleanup failed: %v", err)
	}
	joined := strings.Join(commandTexts(results), "\n")
	if !strings.Contains(joined, fmt.Sprintf("abf policy del id %d\n", fixture.policyID)) {
		t.Fatalf("dangling policy was not deleted with the bare form:\n%s", joined)
	}
	if fileExists(filepath.Join(state, "abf")) {
		t.Fatal("dangling ABF policy survived deletion")
	}
}

// TestRoutePolicyDeleteFailsWhenClassifierSurvives makes a failed classifier
// teardown fatal instead of returning an applied delete.
func TestRoutePolicyDeleteFailsWhenClassifierSurvives(t *testing.T) {
	fixture := newABFSelfHealFixture("lan-proxy-test")
	binary, _ := writeRoutePolicyDeleteVPPCTL(t, fixture, "classifier-stuck")
	channel := &vppctlChannel{binary: binary}

	_, err := channel.removeRoutePolicyState(context.Background(), fixture.operation(), routePolicyVPPCTLSpec{
		policyID: fixture.policyID,
		aclID:    fixture.aclID,
		tableID:  fixture.tableID,
		tag:      fixture.tag,
		ingress:  "lyroute-enp4s0",
		apply:    false,
	})
	if err == nil || !strings.Contains(err.Error(), "pre-NAT rule") {
		t.Fatalf("error = %v, want a surviving pre-NAT rule failure", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestABFDeleteViaResolvesIfIndexToLiveInterface(t *testing.T) {
	output := "abf:[1]: policy:18784 acl:1\n" +
		" path-list:[84] locks:1 flags:shared len:1\n" +
		"  path:[100] pl-index:84 ip4 weight=1 pref=0 attached-nexthop:  oper-flags:drop,\n" +
		"    198.18.168.118 if_index:3\n" +
		"    unresolved\n"
	via := observedABFDeleteVia(output)
	if via != "198.18.168.118 if_index:3" {
		t.Fatalf("observed via = %q", via)
	}
	if got := resolveABFDeleteViaInterface(via, map[int]string{3: "lypxinf9d5aa"}); got != "198.18.168.118 lypxinf9d5aa" {
		t.Fatalf("resolved via = %q, want the live interface name", got)
	}
	if got := resolveABFDeleteViaInterface(via, nil); got != via {
		t.Fatalf("unresolved via = %q, want the original token", got)
	}
}

func fileHasContent(path string) bool {
	payload, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(payload)) != ""
}

// writeRoutePolicyDeleteVPPCTL writes a fake vppctl with live ABF, ACL,
// attachments, classifier and tenant-table state. mode selects the live ABF
// path shape or the failure injection.
func writeRoutePolicyDeleteVPPCTL(t *testing.T, fixture abfSelfHealFixture, mode string) (string, string) {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(t.TempDir(), "vppctl")

	policyPath := fmt.Sprintf(`abf:[0]: policy:%d acl:%d
 path-list:[1] locks:1 flags:shared len:1
  path:[2] pl-index:1 ip4 weight=1 pref=0 attached-nexthop:  oper-flags:resolved,
    198.18.168.118 lypxinf9d5aa
    [@0]: ipv4 via 198.18.168.118 lypxinf9d5aa: mtu:1500
`, fixture.policyID, fixture.aclID)
	if mode == "dangling" {
		policyPath = fmt.Sprintf(`abf:[1]: policy:%d acl:%d
 path-list:[1] locks:1 flags:shared,len:1
  path:[2] pl-index:1 ip4 weight=1 pref=0
    [@0]: dpo-drop
`, fixture.policyID, fixture.aclID)
	}

	template := `#!/bin/sh
set -u
state='{{state}}'
cmd="$*"
case "$cmd" in
  "show interface")
    printf 'Name Idx State MTU\nlyroute-enp3s0 5 up 9000/0/0/0\nlyroute-enp4s0 6 up 9000/0/0/0\nlypxinf9d5aa 7 up 1500/0/0/0\n';;
  "show abf policy {{policy}}")
    if [ -f "$state/abf" ]; then cat "$state/abf"; else printf 'Invalid policy ID:{{policy}}\n'; fi;;
  "show abf attach"*) 
    iface=$(printf '%s\n' "$cmd" | awk '{print $4}')
    if grep -qx "$iface" "$state/attach" 2>/dev/null; then
      printf 'ipv4:\n abf-interface-attach: policy:{{policy}} priority:10\n'
    fi;;
  "abf attach ip4 del policy {{policy}}"*)
    iface=$(printf '%s\n' "$cmd" | awk '{print $7}')
    grep -vx "$iface" "$state/attach" >"$state/attach.tmp" 2>/dev/null || true
    mv "$state/attach.tmp" "$state/attach";;
  "abf policy del id {{policy}} acl {{acl}} via "*)
    rm -f "$state/abf";;
  "abf policy del id {{policy}}")
    rm -f "$state/abf";;
  "show acl-plugin acl"|"show acl-plugin acl index {{acl}}")
    if [ -f "$state/acl" ]; then printf 'acl-index {{acl}} count 1 tag {{{tag}}}\n'; fi;;
  "delete acl-plugin acl index {{acl}}")
    rm -f "$state/acl";;
  "show ip fib summary")
    printf 'ipv4-VRF:{{table}}, fib_index:3, flow hash:[src dst]\n';;
  "show ip fib table {{table}}")
    if [ -f "$state/table" ]; then printf '{{prefix}}/0\n  fib_index:3\n'; fi;;
  "ip route del table {{table}}"*)
    rm -f "$state/table";;
  "ip table del {{table}}")
    rm -f "$state/table";;
  "show ly-route pre-nat-route")
    if [ -f "$state/prenat" ]; then
      printf 'enabled 1 interface lyroute-enp4s0 lan-prefix 192.168.99.0/24 rules 1 radix-nodes 1\nrule id {{policy}} priority 10 prefixes 1 table {{table}} fib-index 3 skip-nat 1 bypass 0\n'
    else
      printf 'enabled 1 interface lyroute-enp4s0 lan-prefix 192.168.99.0/24 rules 0 radix-nodes 0\n'
    fi;;
  "set ly-route pre-nat-route del id {{policy}}")
    if [ '{{mode}}' = classifier-stuck ]; then :; else rm -f "$state/prenat"; fi;;
  *) echo "unsupported: $cmd" >&2; exit 64;;
esac
`
	script := strings.NewReplacer(
		"{{state}}", state,
		"{{policy}}", fmt.Sprintf("%d", fixture.policyID),
		"{{acl}}", fmt.Sprintf("%d", fixture.aclID),
		"{{table}}", fmt.Sprintf("%d", fixture.tableID),
		"{{tag}}", fixture.tag,
		"{{mode}}", mode,
		"{{prefix}}", "0.0.0.0",
	).Replace(template)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "abf"), []byte(policyPath), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "acl"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "attach"), []byte("lyroute-enp3s0\nlyroute-enp4s0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "prenat"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "table"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, state
}
