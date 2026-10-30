package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForbiddenVPPCTLCommandACLDeletes(t *testing.T) {
	// VPP assigns ACL index 0 to the first ACL, and the gateway's
	// lan_dhcp_broadcast ACL lands there on a clean first boot. The guard must
	// reject only malformed or negative indices, otherwise every re-apply and
	// rollback fails with "ACL index must be a positive integer".
	for _, command := range []string{
		"delete acl-plugin acl",
		"?delete acl-plugin acl",
		"delete acl-plugin acl index",
		"delete acl-plugin acl index -1",
		"delete acl-plugin acl index -3",
		"delete acl-plugin acl index all",
		"delete acl-plugin acl index x",
		"acl-plugin del acl 7",
	} {
		if err := forbiddenVPPCTLCommand(command); err == nil {
			t.Fatalf("command %q was accepted, want refusal", command)
		}
	}
	for _, command := range []string{
		"delete acl-plugin acl index 0",
		"?delete acl-plugin acl index 0",
		"delete acl-plugin acl index 7",
		"?delete acl-plugin acl index 4294967295",
		"set acl-plugin acl permit src 192.0.2.0/24 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535 tag t",
		"show acl-plugin acl",
	} {
		if err := forbiddenVPPCTLCommand(command); err != nil {
			t.Fatalf("command %q was refused: %v", command, err)
		}
	}
}

func TestACLDeleteTarget(t *testing.T) {
	if id, ok := aclDeleteTarget("?delete acl-plugin acl index 12"); !ok || id != 12 {
		t.Fatalf("target = %d %v, want 12 true", id, ok)
	}
	// Index 0 is a legal ACL id: the delete must carry it through to the
	// reference check instead of being treated as "no target".
	if id, ok := aclDeleteTarget("delete acl-plugin acl index 0"); !ok || id != 0 {
		t.Fatalf("target of index 0 = %d %v, want 0 true", id, ok)
	}
	for _, command := range []string{"delete acl-plugin acl", "delete acl-plugin acl index -1", "delete acl-plugin acl index x", "abf policy del id 1"} {
		if _, ok := aclDeleteTarget(command); ok {
			t.Fatalf("command %q produced an ACL delete target", command)
		}
	}
}

func TestACLHasAttachedLookupContext(t *testing.T) {
	attached := "acl-index 3 count 1\n  applied inbound on sw_if_index: 1\n  used in lookup context index: 3\n"
	if !aclHasAttachedLookupContext(attached) {
		t.Fatal("attached ACL not detected")
	}
	detached := "acl-index 3 count 1\n  applied inbound on sw_if_index: \n  used in lookup context index: \n"
	if aclHasAttachedLookupContext(detached) {
		t.Fatal("detached ACL reported as referenced")
	}
	if aclHasAttachedLookupContext("acl-index 3 count 1\n  0: ipv4 permit src 0.0.0.0/0\n") {
		t.Fatal("ACL without a context line reported as referenced")
	}
}

func TestABFPolicyReferencesACL(t *testing.T) {
	output := "abf:[0]: policy:65000 acl:0\n path-list:[62]\nabf:[1]: policy:65001 acl:10\n"
	if !abfPolicyReferencesACL(output, 0) {
		t.Fatal("acl:0 not detected")
	}
	if !abfPolicyReferencesACL(output, 10) {
		t.Fatal("acl:10 not detected")
	}
	if abfPolicyReferencesACL(output, 1) {
		t.Fatal("acl:1 must not match acl:10")
	}
}

func TestReferencedACLDeleteIsSkipped(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show acl-plugin acl index 7": {stdout: "acl-index 7 count 1\n  0: ipv4 permit src 192.0.2.0/24\n  used in lookup context index: 0\n"},
		"show abf policy":             {stdout: "abf:[0]: policy:65000 acl:7\n"},
		"show ly-route pre-nat-route": {stdout: "enabled 1 interface lyroute-eth1 lan-prefix 192.0.2.0/24\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	_, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.service-chain.lifecycle",
		VPPCtlCommands: []string{"delete acl-plugin acl index 7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "delete acl-plugin acl index 7") {
		t.Fatalf("referenced ACL delete reached vppctl:\n%s", logged)
	}
}

func TestUnreferencedACLDeleteRuns(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show acl-plugin acl index 7":   {stdout: "acl-index 7 count 1\n  0: ipv4 permit src 192.0.2.0/24\n"},
		"show abf policy":               {stdout: ""},
		"show ly-route pre-nat-route":   {stdout: ""},
		"delete acl-plugin acl index 7": {stdout: "Deleted ACL index:7\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.service-chain.lifecycle",
		VPPCtlCommands: []string{"delete acl-plugin acl index 7"},
	}); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "delete acl-plugin acl index 7") {
		t.Fatalf("unreferenced ACL delete did not reach vppctl:\n%s", logged)
	}
}

func TestABFPolicyAddToMissingTableIsRefused(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show ip table":           {stdout: "[0] table_id:0 ipv4-VRF:0\n"},
		"show ip fib table 51234": {stdout: ""},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.route-policy",
		VPPCtlCommands: []string{"abf policy add id 15567 acl 7 via ip4-lookup-in-table 51234"},
	}); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "abf policy add") {
		t.Fatalf("ABF add to a missing table reached vppctl:\n%s", logged)
	}
}

func TestABFPolicyAddToTableWithoutForwardingDefaultIsRefused(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show ip table":           {stdout: "[0] table_id:0 ipv4-VRF:0\n[1] table_id:51234 ipv4-VRF:51234\n"},
		"show ip fib table 51234": {stdout: "0.0.0.0/0\n  unicast-ip4-chain\n    [@0]: dpo-drop ip4\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.route-policy",
		VPPCtlCommands: []string{"abf policy add id 15567 acl 7 via ip4-lookup-in-table 51234"},
	}); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "abf policy add") {
		t.Fatalf("ABF add to a drop-only table reached vppctl:\n%s", logged)
	}
}
