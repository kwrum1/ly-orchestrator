package vpp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const lanDHCPTestLCPOutput = `lcp default netns '<unset>'
lcp lcp-sync on
itf-pair: [0] lyroute-enp3s0 tap4096 lylan-enp3s0 101 type tap
itf-pair: [1] lyroute-enp4s0 tap4097 lylan-enp4s0 102 type tap
`

const lanDHCPTestLCPRecreated = `lcp default netns '<unset>'
lcp lcp-sync on
itf-pair: [0] lyroute-enp3s0 tap4096 lylan-enp3s0 101 type tap
itf-pair: [1] lyroute-enp4s0 tap4098 lylan-enp4s0 103 type tap
`

const lanDHCPTestLCPEmpty = "lcp default netns '<unset>'\nlcp lcp-sync on\n"

func lanDHCPTestRedirect(tap string) string {
	return ` rx lyroute-enp4s0 via:
   path-list:[40] locks:1 flags:no-uRPF, uRPF-list: None
    path:[46] pl-index:40 ip4 weight=1 pref=0 dvr:  oper-flags:resolved,
       ` + tap + `
 forwarding
  [@3]: ip4-dvr-` + tap + `-dpo l3
`
}

// lanDHCPTestFeatures is the `show interface features <tap>` shape linux-cp
// produces for a healthy pair (vppctl emits CRLF line endings).
func lanDHCPTestFeatures(tap string, withXConnect bool) string {
	body := "Feature paths configured on " + tap + "...\r\n\r\nnsh-eth-output:\r\n  none configured\r\n\r\nip4-unicast:\r\n"
	if withXConnect {
		body += "  linux-cp-xc-ip4\r\n"
	}
	body += "  ip4-not-enabled\r\n\r\nl2-output-nonip:\r\n  none configured\r\n"
	return body
}

// writeLANDHCPFakeVPPCTL installs a stateful fake vppctl for the DHCP delivery
// leg. `lcp delete` removes the pair and its redirect; `lcp create` brings the
// pair back with a new tap, which is what linux-cp does on a real re-creation.
func writeLANDHCPFakeVPPCTL(t *testing.T, lcp, redirects, features string, nat44 string, recreateRotatesPair bool) (binary string, logPath string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	lcpState := write("lcp", lcp)
	redirectState := write("redirects", redirects)
	nat := write("nat44.txt", nat44)
	recreatedLCP := write("lcp.recreated", lanDHCPTestLCPRecreated)
	recreatedRedirects := write("redirects.recreated", lanDHCPTestRedirect("tap4098"))
	recreatedFeatures := write("features.recreated", lanDHCPTestFeatures("tap4098", true))
	if !recreateRotatesPair {
		recreatedLCP = write("lcp.same", lcp)
		recreatedRedirects = write("redirects.same", redirects)
		recreatedFeatures = write("features.same", features)
	}
	logPath = filepath.Join(dir, "commands.log")
	binary = filepath.Join(dir, "vppctl")
	script := `#!/bin/sh
[ -n "$FAKE_VPPCTL_LOG" ] && printf '%s\n' "$*" >> "$FAKE_VPPCTL_LOG"
case "$*" in
  "show lcp")
    cat ` + lcpState + `
    ;;
  "show ip punt redirect")
    cat ` + redirectState + `
    ;;
  "show interface features tap4097")
    cat ` + write("features.4097", features) + `
    ;;
  "show interface features tap4098")
    cat ` + recreatedFeatures + `
    ;;
  "lcp delete lyroute-enp4s0")
    : > ` + lcpState + `
    : > ` + redirectState + `
    ;;
  "lcp create lyroute-enp4s0 host-if lylan-enp4s0")
    cp ` + recreatedLCP + ` ` + lcpState + `
    cp ` + recreatedRedirects + ` ` + redirectState + `
    ;;
  "lcp lcp-sync on")
    : ;;
  "show nat44 interfaces")
    cat ` + nat + `
    ;;
  "show nat44 ei interfaces")
    exit 0
    ;;
  *)
    echo "unsupported command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, logPath
}

func lanDHCPTestManagement() ManagementLCP {
	return ManagementLCP{
		Enabled:                 true,
		VPPInterface:            "lyroute-enp4s0",
		HostInterface:           "lylan-enp4s0",
		IPv4BroadcastLocal:      true,
		IPv4DHCPBroadcastBypass: true,
	}
}

func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
}

func readFakeCommands(t *testing.T, path string) []string {
	t.Helper()
	logged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(logged))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func requireCommandSequence(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestManagementLCPTapResolvesPairTap(t *testing.T) {
	if got := managementLCPTap(lanDHCPTestLCPOutput, "lyroute-enp4s0", "lylan-enp4s0"); got != "tap4097" {
		t.Fatalf("tap = %q, want tap4097", got)
	}
	if got := managementLCPTap(lanDHCPTestLCPOutput, "lyroute-enp4s0", "lylan-enp3s0"); got != "" {
		t.Fatalf("tap for mismatched host interface = %q, want empty", got)
	}
}

func TestLANDHCPPuntRedirectPresenceRejectsStaleTap(t *testing.T) {
	if !lanDHCPPuntRedirectPresent(lanDHCPTestRedirect("tap4097"), "lyroute-enp4s0", "tap4097") {
		t.Fatal("healthy redirect was not detected")
	}
	if lanDHCPPuntRedirectPresent(lanDHCPTestRedirect("tap4097"), "lyroute-enp4s0", "tap4096") {
		t.Fatal("redirect to a different tap was accepted")
	}
	if lanDHCPPuntRedirectPresent(lanDHCPTestRedirect("tap4096"), "lyroute-enp4s0", "tap4097") {
		t.Fatal("stale redirect was accepted as the delivery leg")
	}
	if lanDHCPPuntRedirectPresent(lanDHCPTestRedirect("tap4096"), "lyroute-enp5s0", "tap4096") {
		t.Fatal("redirect of another interface was matched")
	}
}

// The reply direction is a separate feature: an OFFER leaves the tap through
// linux-cp-xc-ip4. Missing it must be detected, not inferred from the forward
// redirect.
func TestLANHostXConnectPresent(t *testing.T) {
	if !lanHostXConnectPresent(lanDHCPTestFeatures("tap4097", true), lanHostXConnectFeature) {
		t.Fatal("linux-cp-xc-ip4 was not detected in the ip4-unicast arc")
	}
	if lanHostXConnectPresent(lanDHCPTestFeatures("tap4097", false), lanHostXConnectFeature) {
		t.Fatal("missing linux-cp-xc-ip4 was accepted")
	}
	// A feature with the same name in another arc must not count.
	other := "ip6-unicast:\r\n  linux-cp-xc-ip4\r\n\r\nip4-unicast:\r\n  ip4-not-enabled\r\n"
	if lanHostXConnectPresent(other, lanHostXConnectFeature) {
		t.Fatal("feature of another arc was accepted")
	}
}

// A healthy system emits readbacks only: re-applies must not re-create the LCP
// pair (that would flap the host tap and the LAN address on every apply).
func TestEnsureLANDHCPDeliveryKeepsHealthyPair(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, lanDHCPTestRedirect("tap4097"), lanDHCPTestFeatures("tap4097", true), "", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	results, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].Command != "show lcp" || results[2].Command != "show interface features tap4097" {
		t.Fatalf("readbacks = %#v", results)
	}
	requireCommandSequence(t, readFakeCommands(t, logPath), []string{
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4097",
	})
}

// A re-created tap gets a new name, so the repair must re-read the pair and
// verify both directions on the new tap.
func TestEnsureLANDHCPDeliveryRecreatesPairWhenRedirectIsMissing(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, "", lanDHCPTestFeatures("tap4097", true), "", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	results, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement())
	if err != nil {
		t.Fatal(err)
	}
	requireCommandSequence(t, readFakeCommands(t, logPath), []string{
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4097",
		"lcp delete lyroute-enp4s0",
		"lcp create lyroute-enp4s0 host-if lylan-enp4s0",
		"lcp lcp-sync on",
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4098",
	})
	last := results[len(results)-1]
	if last.Command != "show interface features tap4098" || !lanHostXConnectPresent(last.Stdout, lanHostXConnectFeature) {
		t.Fatalf("reply leg was not read back on the re-created tap: %#v", last)
	}
	for _, command := range readFakeCommands(t, logPath) {
		if strings.HasPrefix(command, "ip punt redirect add") {
			t.Fatalf("delivery leg was repaired with an unsupported CLI form: %s", command)
		}
	}
}

// The reported one-way failure: dnsmasq answers (forward leg fine) but the
// OFFER never leaves the tap because the xconnect feature is gone.
func TestEnsureLANDHCPDeliveryRecreatesPairWhenReplyFeatureIsMissing(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, lanDHCPTestRedirect("tap4097"), lanDHCPTestFeatures("tap4097", false), "", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement()); err != nil {
		t.Fatal(err)
	}
	requireCommandSequence(t, readFakeCommands(t, logPath), []string{
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4097",
		"lcp delete lyroute-enp4s0",
		"lcp create lyroute-enp4s0 host-if lylan-enp4s0",
		"lcp lcp-sync on",
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4098",
	})
}

func TestEnsureLANDHCPDeliveryRecreatesPairWhenRedirectPointsAtStaleTap(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, lanDHCPTestRedirect("tap4096"), lanDHCPTestFeatures("tap4097", true), "", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement()); err != nil {
		t.Fatal(err)
	}
	requireCommandSequence(t, readFakeCommands(t, logPath), []string{
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4097",
		"lcp delete lyroute-enp4s0",
		"lcp create lyroute-enp4s0 host-if lylan-enp4s0",
		"lcp lcp-sync on",
		"show lcp",
		"show ip punt redirect",
		"show interface features tap4098",
	})
}

// A repair is only useful when the readback proves it: a VPP that accepts the
// commands without installing the leg must fail the apply instead of reporting
// a delivered DHCP service.
func TestEnsureLANDHCPDeliveryFailsWhenRepairIsNotReadBack(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, "", lanDHCPTestFeatures("tap4097", false), "", false)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	_, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement())
	if err == nil || !strings.Contains(err.Error(), "absent from readback") {
		t.Fatalf("err = %v, want a readback failure", err)
	}
}

func TestEnsureLANDHCPDeliveryFailsWithoutLCPPair(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPEmpty, "", lanDHCPTestFeatures("tap4097", true), "", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	_, err := channel.ensureLANDHCPDelivery(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, lanDHCPTestManagement())
	if err == nil || !strings.Contains(err.Error(), "has no tap interface") {
		t.Fatalf("err = %v, want a missing-pair failure", err)
	}
}

func TestVerifyLANIsNotNAT44OutsideRejectsOutsideRole(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, lanDHCPTestRedirect("tap4097"), lanDHCPTestFeatures("tap4097", true), "NAT44 interfaces:\n lyroute-enp5s0 out\n lyroute-enp4s0 out\n", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	_, err := channel.verifyLANIsNotNAT44Outside(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, "lyroute-enp4s0")
	if err == nil || !strings.Contains(err.Error(), "NAT44 outside role") {
		t.Fatalf("err = %v, want the NAT44 outside role failure", err)
	}
}

func TestVerifyLANIsNotNAT44OutsideAcceptsInsideRole(t *testing.T) {
	requirePOSIXShell(t)
	binary, logPath := writeLANDHCPFakeVPPCTL(t, lanDHCPTestLCPOutput, lanDHCPTestRedirect("tap4097"), lanDHCPTestFeatures("tap4097", true), "NAT44 interfaces:\n lyroute-enp4s0 in\n lyroute-enp5s0 out\n", true)
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := &vppctlChannel{binary: binary}
	if _, err := channel.verifyLANIsNotNAT44Outside(context.Background(), Operation{Name: "vpp.lan-control-lcp"}, "lyroute-enp4s0"); err != nil {
		t.Fatalf("inside LAN was rejected: %v", err)
	}
}

func TestManagementLCPCommandsReadBackTheDHCPDeliveryLeg(t *testing.T) {
	commands := strings.Join(managementLCPCommands(lanDHCPTestManagement()), "\n")
	if !strings.Contains(commands, "show ip punt redirect") {
		t.Fatalf("static LCP plan does not read the DHCP delivery leg back:\n%s", commands)
	}
}
