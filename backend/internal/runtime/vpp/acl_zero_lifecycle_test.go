package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cold first boot allocates ACL index 0 for the LAN DHCP broadcast bypass.
// The next apply must be able to replace it without a manual VPP edit.
func TestACLIndexZeroDeleteReachesVppctl(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vppctl.log")
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"show acl-plugin acl index 0":   {stdout: "acl-index 0 count 1\n  0: ipv4 permit src 192.0.2.0/24\n  used in lookup context index: \n"},
		"show abf policy":               {stdout: ""},
		"show ly-route pre-nat-route":   {stdout: ""},
		"delete acl-plugin acl index 0": {stdout: "Deleted ACL index:0\n"},
	})
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.doCommands(context.Background(), Operation{
		Name:           "vpp.lan-control-lcp",
		VPPCtlCommands: []string{"delete acl-plugin acl index 0"},
	}); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "delete acl-plugin acl index 0") {
		t.Fatalf("index 0 delete did not reach vppctl:\n%s", logged)
	}
}
