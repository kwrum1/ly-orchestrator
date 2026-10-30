package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// policyTenantFakeVPPCTL writes a stateful vppctl stand-in that models just the
// pieces of the tenant-table lifecycle under test: IPv4 tables, the 0.0.0.0/0
// entry of a table, and the pre-NAT classifier inventory. It can also simulate
// the two real failure modes seen at boot: a best-effort route write that
// "succeeds" without storing anything, and a hard VPP rejection.
func policyTenantFakeVPPCTL(t *testing.T) (string, string) {
	t.Helper()
	state := t.TempDir()
	script := `#!/bin/sh
STATE="${FAKE_VPP_STATE:?state is required}"
if [ -n "$FAKE_VPPCTL_LOG" ]; then printf '%s\n' "$*" >> "$FAKE_VPPCTL_LOG"; fi
MODE="${FAKE_ROUTE_ADD_MODE:-ok}"
prefix_key() { printf '%s' "$1" | tr '/' '_'; }

case "$1 $2 $3" in
  "show ip table")
    if [ -f "$STATE/tables" ]; then sed 's/^/table_id:/' "$STATE/tables"; fi
    exit 0
    ;;
  "show ip fib")
    table="$5"
    if ! grep -qx "$table" "$STATE/tables" 2>/dev/null; then
      echo "no such table $table" >&2
      exit 1
    fi
    echo "ipv4-VRF:$table, fib_index:2, flow hash:[src dst sport dport proto ] epoch:0 flags:none locks:[CLI:1, ]"
    key=$(prefix_key "0.0.0.0/0")
    if [ -f "$STATE/route_${table}_${key}" ]; then
      echo "0.0.0.0/0"
      echo "  unicast-ip4-chain"
      echo "  [@0]: dpo-load-balance: [proto:ip4 index:36 buckets:1 uRPF:52 to:[0:0]]"
      echo "    [0] [@5]: ipv4 via $(cat "$STATE/route_${table}_${key}")"
    fi
    echo "0.0.0.0/32"
    echo "  unicast-ip4-chain"
    echo "    [@0]: dpo-drop ip4"
    exit 0
    ;;
  "show interface address")
    echo "$4 (up):"
    echo "  L3 192.168.99.1/24"
    exit 0
    ;;
  "show ly-route pre-nat-route")
    iface=$(cat "$STATE/prenat_if" 2>/dev/null || echo 0)
    lan_prefix=$(cat "$STATE/prenat_prefix" 2>/dev/null || echo 0.0.0.0/0)
    rules=$(wc -l < "$STATE/prenat_rules" 2>/dev/null || echo 0)
    echo "enabled 1 interface $iface lan-prefix $lan_prefix rules $rules radix-nodes $rules"
    if [ -f "$STATE/prenat_rules" ]; then
      while read -r id priority table skip_nat bypass; do
        echo "rule id $id priority $priority prefixes 1 table $table fib-index 2 skip-nat $skip_nat bypass $bypass"
      done < "$STATE/prenat_rules"
    fi
    exit 0
    ;;
  "show acl-plugin acl")
    exit 0
    ;;
esac

case "$1 $2" in
  "ip table")
    if [ "$3" = "add" ]; then
      echo "$4" >> "$STATE/tables"
    fi
    exit 0
    ;;
  "ip route")
    action="$3"
    shift 3
    if [ "$1" = "table" ]; then table="$2"; shift 2; fi
    prefix="$1"
    shift
    key=$(prefix_key "$prefix")
    if [ "$action" = "add" ]; then
      if [ "$MODE" = "fail" ]; then
        echo "ip route add $prefix: unknown interface in next hop" >&2
        exit 1
      fi
      if [ "$MODE" = "silent-once" ] && [ ! -f "$STATE/silent-write" ]; then
        touch "$STATE/silent-write"
        exit 0
      fi
      if [ "$1" = "via" ]; then shift; fi
      printf '%s\n' "$*" > "$STATE/route_${table}_${key}"
      exit 0
    fi
    if [ "$action" = "del" ]; then
      rm -f "$STATE/route_${table}_${key}"
      exit 0
    fi
    exit 0
    ;;
  "set ly-route")
    case "$4" in
      interface)
        printf '%s' "$5" > "$STATE/prenat_if"
        printf '%s' "$7" > "$STATE/prenat_prefix"
        exit 0
        ;;
      add)
        # set ly-route pre-nat-route add id <id> priority <p> ... table <t> [skip-nat] [bypass]
        shift 4
        id=0; priority=0; table=0; skip_nat=0; bypass=0
        while [ $# -gt 0 ]; do
          case "$1" in
            id) id="$2"; shift 2 ;;
            priority) priority="$2"; shift 2 ;;
            table) table="$2"; shift 2 ;;
            skip-nat) skip_nat=1; shift ;;
            bypass) bypass=1; shift ;;
            *) shift ;;
          esac
        done
        printf '%s %s %s %s %s\n' "$id" "$priority" "$table" "$skip_nat" "$bypass" >> "$STATE/prenat_rules"
        exit 0
        ;;
      del)
        # set ly-route pre-nat-route del id <id>
        shift 4
        if [ "$1" = "id" ] && [ -f "$STATE/prenat_rules" ]; then
          grep -v "^$2 " "$STATE/prenat_rules" > "$STATE/prenat_rules.tmp" || true
          mv "$STATE/prenat_rules.tmp" "$STATE/prenat_rules"
        fi
        exit 0
        ;;
      clear)
        rm -f "$STATE/prenat_rules"
        exit 0
        ;;
    esac
    exit 0
    ;;
  "set ip")
    exit 0
    ;;
  "show abf")
    echo "Invalid policy ID"
    exit 0
    ;;
esac

echo "unsupported command: $*" >&2
exit 64
`
	binary := filepath.Join(t.TempDir(), "vppctl")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_VPP_STATE", state)
	t.Setenv("LY_ROUTE_LAN_INTERFACE", "enp4s0")
	return binary, state
}

func policyTenantOperation(t *testing.T) Operation {
	t.Helper()
	route := trafficpolicy.RoutePolicy{
		ID:       "lan-internet",
		Priority: 100,
		Action:   "route",
		Egress:   "wan-main",
		Path:     &trafficpolicy.WANPath{VPPInterface: "lyroute-enp5s0", NextHop: "10.1.18.1"},
		Match: trafficpolicy.Match{
			Sources:      []string{"192.168.99.0/24"},
			Destinations: []string{"0.0.0.0/0"},
		},
	}
	operations, err := BuildRouteWANGroupOperations(RouteWANGroupPlan{
		TransactionID:       "txn-tenant-default",
		IngressVPPInterface: "lyroute-enp4s0",
		AddressAssignments: []AddressAssignment{{
			ID: "lan99", LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", CIDR: "192.168.99.1/24", Role: "lan",
		}},
		Routes: []trafficpolicy.RoutePolicy{route},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Name != "vpp.route-policy" {
		t.Fatalf("operations = %#v, want one route policy", operations)
	}
	return operations[0]
}

func policyTenantChannel(t *testing.T, binary string) *vppctlChannel {
	t.Helper()
	channel, err := NewProductionVPPCTLClient(binary).OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	typed, ok := channel.(*vppctlChannel)
	if !ok {
		t.Fatalf("channel type = %T", channel)
	}
	return typed
}

func policyTenantRouteFile(t *testing.T, state string, tableID int) string {
	t.Helper()
	return filepath.Join(state, fmt.Sprintf("route_%d_0.0.0.0_0", tableID))
}

func TestPreNATRoutePolicyCreatesTenantDefaultRoute(t *testing.T) {
	binary, state := policyTenantFakeVPPCTL(t)
	operation := policyTenantOperation(t)
	tableID := stableID("route-table:"+operation.Resource, 50000, 49999)

	if _, err := policyTenantChannel(t, binary).doPreNATRoutePolicyLifecycle(context.Background(), operation); err != nil {
		t.Fatalf("apply route policy: %v", err)
	}
	payload, err := os.ReadFile(policyTenantRouteFile(t, state, tableID))
	if err != nil {
		t.Fatalf("tenant default route was not written: %v", err)
	}
	if got := strings.TrimSpace(string(payload)); got != "10.1.18.1 lyroute-enp5s0" {
		t.Fatalf("tenant default route = %q, want 10.1.18.1 lyroute-enp5s0", got)
	}
}

func TestPreNATRoutePolicyRebuildsMissingTenantDefaultRouteAfterRestart(t *testing.T) {
	// The first best-effort write reports success but stores nothing, which is
	// what a cold boot looks like when the WAN underlay is not ready yet.
	t.Setenv("FAKE_ROUTE_ADD_MODE", "silent-once")
	binary, state := policyTenantFakeVPPCTL(t)
	operation := policyTenantOperation(t)
	tableID := stableID("route-table:"+operation.Resource, 50000, 49999)

	if _, err := policyTenantChannel(t, binary).doPreNATRoutePolicyLifecycle(context.Background(), operation); err != nil {
		t.Fatalf("apply route policy with a silently dropped default route: %v", err)
	}
	payload, err := os.ReadFile(policyTenantRouteFile(t, state, tableID))
	if err != nil {
		t.Fatalf("tenant default route was not repaired: %v", err)
	}
	if !strings.Contains(string(payload), "10.1.18.1") {
		t.Fatalf("repaired tenant default route = %q", payload)
	}
}

func TestPreNATRoutePolicyFailsWhenTenantDefaultCannotBeWritten(t *testing.T) {
	t.Setenv("FAKE_ROUTE_ADD_MODE", "fail")
	binary, state := policyTenantFakeVPPCTL(t)
	operation := policyTenantOperation(t)
	tableID := stableID("route-table:"+operation.Resource, 50000, 49999)

	_, err := policyTenantChannel(t, binary).doPreNATRoutePolicyLifecycle(context.Background(), operation)
	if err == nil {
		t.Fatal("a tenant table without a forwarding default must fail the apply")
	}
	if !strings.Contains(err.Error(), "default") && !strings.Contains(err.Error(), "unknown interface") {
		t.Fatalf("error = %v, want a tenant default route failure", err)
	}
	if _, statErr := os.Stat(policyTenantRouteFile(t, state, tableID)); statErr == nil {
		t.Fatal("the failed tenant default route must not be recorded as installed")
	}
}

func TestPreNATRoutePolicyTenantDefaultRouteIsIdempotent(t *testing.T) {
	binary, state := policyTenantFakeVPPCTL(t)
	operation := policyTenantOperation(t)
	tableID := stableID("route-table:"+operation.Resource, 50000, 49999)
	channel := policyTenantChannel(t, binary)

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := channel.doPreNATRoutePolicyLifecycle(context.Background(), operation); err != nil {
			t.Fatalf("apply attempt %d: %v", attempt, err)
		}
	}
	payload, err := os.ReadFile(policyTenantRouteFile(t, state, tableID))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(payload)), "\n") + 1; lines != 1 {
		t.Fatalf("tenant default route stored %d times: %q", lines, payload)
	}
	rules, err := os.ReadFile(filepath.Join(state, "prenat_rules"))
	if err != nil {
		t.Fatal(err)
	}
	policyID := stableID("route-abf:"+operation.Resource, 10000, 8999)
	if count := strings.Count(string(rules), fmt.Sprintf("%d ", policyID)); count != 1 {
		t.Fatalf("policy rule stored %d times, want one: %q", count, rules)
	}
}

func TestPreNATRoutePolicyDeleteRemovesTenantDefaultRoute(t *testing.T) {
	binary, state := policyTenantFakeVPPCTL(t)
	operation := policyTenantOperation(t)
	tableID := stableID("route-table:"+operation.Resource, 50000, 49999)
	channel := policyTenantChannel(t, binary)

	if _, err := channel.doPreNATRoutePolicyLifecycle(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	deletion := Operation{
		Name:           "vpp.route-policy.pre-delete",
		RequestID:      "txn-tenant-default",
		Resource:       operation.Resource,
		Payload:        operation.Payload,
		VPPCtlCommands: deleteRoutePolicyCommands(operation.Resource),
	}
	if _, err := channel.doPreNATRoutePolicyLifecycle(context.Background(), deletion); err != nil {
		t.Fatalf("delete route policy: %v", err)
	}
	if _, err := os.Stat(policyTenantRouteFile(t, state, tableID)); !os.IsNotExist(err) {
		t.Fatalf("tenant default route remains after deletion: %v", err)
	}
}

func TestVerifyRoutePolicyTenantDefaultRejectsHeaderOnlyTable(t *testing.T) {
	command := "show ip fib table 63477"
	headerOnly := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:63477, fib_index:2\n0.0.0.0/32\n  unicast-ip4-chain\n    [@0]: dpo-drop ip4\n"}}
	if err := verifyRoutePolicyTenantDefault(headerOnly, 63477, "10.1.18.1 lyroute-enp5s0"); err == nil {
		t.Fatal("a table with only drop entries must not satisfy the default route contract")
	}
	dropDefault := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:63477, fib_index:2\n0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: dpo-drop ip4\n"}}
	if err := verifyRoutePolicyTenantDefault(dropDefault, 63477, "10.1.18.1 lyroute-enp5s0"); err == nil {
		t.Fatal("a drop default must not satisfy the default route contract")
	}
	wrongVia := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:63477, fib_index:2\n0.0.0.0/0\n  unicast-ip4-chain\n    [0] [@5]: ipv4 via 10.0.0.1 lyroute-enp5s0\n"}}
	if err := verifyRoutePolicyTenantDefault(wrongVia, 63477, "10.1.18.1 lyroute-enp5s0"); err == nil {
		t.Fatal("a wrong next hop must not satisfy the default route contract")
	}
	prefixLookalike := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:63477, fib_index:2\n0.0.0.0/0\n  unicast-ip4-chain\n    [0] [@5]: ipv4 via 10.1.18.10 lyroute-enp5s0\n"}}
	if err := verifyRoutePolicyTenantDefault(prefixLookalike, 63477, "10.1.18.1"); err == nil {
		t.Fatal("a next hop that only shares a string prefix must not satisfy the contract")
	}
	live := []VPPCTLCommandResult{{Command: command, Stdout: "ipv4-VRF:63477, fib_index:2\n0.0.0.0/0\n  unicast-ip4-chain\n    [0] [@5]: ipv4 via 10.1.18.1 lyroute-enp5s0: mtu:9000\n"}}
	if err := verifyRoutePolicyTenantDefault(live, 63477, "10.1.18.1 lyroute-enp5s0"); err != nil {
		t.Fatalf("live default route rejected: %v", err)
	}
}

func TestRoutePolicyTenantDefaultViaSupportsDynamicWANWithoutGateway(t *testing.T) {
	operation := Operation{VPPCtlCommands: []string{
		"?ip table add 63477",
		"?ip route add table 63477 0.0.0.0/0 via lyroute-enp5s0",
	}}
	via, ok := routePolicyTenantDefaultVia(operation)
	if !ok || via != "lyroute-enp5s0" {
		t.Fatalf("dynamic WAN via = %q, %t; want the WAN interface", via, ok)
	}
	lookup := Operation{VPPCtlCommands: []string{"?ip route add table 63477 0.0.0.0/0 via ip4-lookup-in-table 64000"}}
	via, ok = routePolicyTenantDefaultVia(lookup)
	if !ok || via != "table 64000" {
		t.Fatalf("lookup via = %q, %t; want table 64000", via, ok)
	}
	if _, ok := routePolicyTenantDefaultVia(Operation{VPPCtlCommands: []string{"?ip table add 63477"}}); ok {
		t.Fatal("a policy without a default route must not require one")
	}
}

func TestDecodeVPPCTLRoutesRejectsNativePolicyWithoutTenantDefaultRoute(t *testing.T) {
	policy := trafficpolicy.RoutePolicy{
		ID:       "lan-internet",
		Priority: 100,
		Action:   "route",
		Egress:   "wan-main",
		Path:     &trafficpolicy.WANPath{VPPInterface: "lyroute-enp5s0", NextHop: "10.1.18.1"},
		Match: trafficpolicy.Match{
			Sources:      []string{"192.168.99.0/24"},
			Destinations: []string{"0.0.0.0/0"},
		},
	}
	policyID := stableID("route-abf:"+policy.ID, 10000, 8999)
	tableID := stableID("route-table:"+policy.ID, 50000, 49999)
	request := SnapshotRequest{
		TransactionID: "txn-native-default",
		RoutePolicies: []string{policy.ID},
		Candidates:    SnapshotCandidates{RoutePolicies: []trafficpolicy.RoutePolicy{policy}},
	}
	base := []VPPCTLCommandResult{
		{Command: "show acl-plugin acl", Stdout: ""},
		{Command: "show ly-route pre-nat-route", Stdout: fmt.Sprintf("enabled 1 interface lyroute-enp4s0 lan-prefix 192.168.99.0/24 rules 1 radix-nodes 1\nrule id %d priority 100 prefixes 1 table %d fib-index 2 skip-nat 0 bypass 0\n", policyID, tableID)},
	}
	headerOnly := append(append([]VPPCTLCommandResult(nil), base...), VPPCTLCommandResult{
		Command: fmt.Sprintf("show ip fib table %d", tableID),
		Stdout:  fmt.Sprintf("ipv4-VRF:%d, fib_index:2\n0.0.0.0/32\n  unicast-ip4-chain\n    [@0]: dpo-drop ip4\n", tableID),
	})
	if _, err := decodeVPPCTLRoutes(request, headerOnly); err == nil {
		t.Fatal("a native pre-NAT policy with an empty tenant table must not be reported as applied")
	}
	live := append(append([]VPPCTLCommandResult(nil), base...), VPPCTLCommandResult{
		Command: fmt.Sprintf("show ip fib table %d", tableID),
		Stdout:  fmt.Sprintf("ipv4-VRF:%d, fib_index:2\n0.0.0.0/0\n  unicast-ip4-chain\n    [0] [@5]: ipv4 via 10.1.18.1 lyroute-enp5s0: mtu:9000\n", tableID),
	})
	readback, err := decodeVPPCTLRoutes(request, live)
	if err != nil {
		t.Fatalf("live native policy readback: %v", err)
	}
	if len(readback.Policies) != 1 || readback.Policies[0].ID != policy.ID {
		t.Fatalf("readback = %#v", readback.Policies)
	}
}
