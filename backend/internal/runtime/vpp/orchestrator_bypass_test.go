package vpp

import (
	"testing"
)

func TestParseTransparentOrchestratorBypass(t *testing.T) {
	output := `state running
generation abc
default drop
bypass state on
bypass cause auto
bypass reason probe_failed_3/3:interface:ens161_operstate_down
bypass forced no
bypass packets 42
group grp-a wan-facing lyroute-ens224 lan-facing lyroute-ens160
`
	enabled, cause, reason, forced, packets := ParseTransparentOrchestratorBypass(output)
	if !enabled || cause != "auto" || forced || packets != 42 {
		t.Fatalf("parse = %t %q %q %t %d", enabled, cause, reason, forced, packets)
	}
	if reason != "probe_failed_3/3:interface:ens161_operstate_down" {
		t.Fatalf("reason = %q", reason)
	}

	disabled, cause, reason, forced, packets := ParseTransparentOrchestratorBypass("state running\ngeneration abc\nbypass state off\nbypass cause none\nbypass reason none\nbypass forced no\nbypass packets 0\n")
	if disabled || cause != "" || reason != "" || forced || packets != 0 {
		t.Fatalf("disabled parse = %t %q %q %t %d", disabled, cause, reason, forced, packets)
	}
}

func TestBuildTransparentOrchestratorBypassCommandsSanitizesTokens(t *testing.T) {
	on := BuildTransparentOrchestratorBypassCommands(true, "auto", "probe failed 3/3: ens161")
	if len(on) != 2 || on[1] != "show ly-route orchestrator" {
		t.Fatalf("on commands = %#v", on)
	}
	want := "set ly-route orchestrator bypass on cause auto reason probe_failed_3/3:_ens161"
	if on[0] != want {
		t.Fatalf("on command = %q, want %q", on[0], want)
	}
	off := BuildTransparentOrchestratorBypassCommands(false, "manual", "manual_clear")
	if len(off) != 2 || off[0] != "set ly-route orchestrator bypass off cause manual reason manual_clear" {
		t.Fatalf("off commands = %#v", off)
	}
}

func TestParseGroupBypassAndProbes(t *testing.T) {
	output := `state running
generation abc
group-bypass grp-a state off forced no cause none packets 0 reason none
group-bypass grp-b state on forced no cause auto packets 30 reason flow_no_response_4/4
probe-targets 2
group-probe grp-a dst 192.168.10.82 port 80 proto tcp attempts 14 syn 14 synack 0 rst 14 responses 0 icmp-unreachable 0 window 8
group-probe grp-b dst 192.0.2.53 port 53 proto udp attempts 4 syn 0 synack 0 rst 0 responses 2 icmp-unreachable 1 window 8
`
	groups := ParseTransparentOrchestratorGroupBypass(output)
	if len(groups) != 2 || groups[0].Name != "grp-a" || groups[0].Enabled {
		t.Fatalf("group parse = %#v", groups)
	}
	if !groups[1].Enabled || groups[1].Packets != 30 || groups[1].Cause != "auto" || groups[1].Reason != "flow_no_response_4/4" {
		t.Fatalf("group b parse = %#v", groups[1])
	}
	probes := ParseTransparentOrchestratorGroupProbes(output)
	if len(probes) != 2 || probes[0].Attempts != 14 || probes[0].RST != 14 || probes[0].WindowSeconds != 8 {
		t.Fatalf("probe parse = %#v", probes)
	}
	if probes[1].Protocol != "udp" || probes[1].Responses != 2 || probes[1].ICMPUnreachable != 1 {
		t.Fatalf("udp probe parse = %#v", probes[1])
	}
	if count := ParseTransparentOrchestratorProbeTargets(output); count != 2 {
		t.Fatalf("probe target count = %d", count)
	}
}

func TestBuildGroupBypassAndProbeCommands(t *testing.T) {
	commands := BuildTransparentOrchestratorGroupBypassCommands("grp-b", true, "auto", "flow no response")
	want := "set ly-route orchestrator bypass group grp-b on cause auto reason flow_no_response"
	if len(commands) != 2 || commands[0] != want {
		t.Fatalf("group bypass commands = %#v", commands)
	}
	probes := BuildTransparentOrchestratorProbeCommands([]GroupProbeTarget{
		{Group: "grp-a", DstIP: "192.0.2.10", DstPort: 443, Protocol: "tcp", WindowSeconds: 8},
		{Group: "grp-b", DstIP: "192.0.2.53", DstPort: 53, Protocol: "udp", WindowSeconds: 10},
	})
	wantProbe := "set ly-route orchestrator probe group grp-b dst 192.0.2.53 port 53 proto udp window 10"
	if len(probes) != 4 || probes[0] != "set ly-route orchestrator probe clear" || probes[2] != wantProbe || probes[3] != "show ly-route orchestrator" {
		t.Fatalf("probe commands = %#v", probes)
	}
}

func TestTransparentOrchestratorReadbackCarriesBypass(t *testing.T) {
	readback := TransparentOrchestratorReadback{
		Generation: "abc", State: "running", Default: "drop",
		Bypass: true, BypassCause: "auto", BypassReason: "probe", BypassForced: false, BypassPackets: 7,
	}
	if !readback.Bypass || readback.BypassPackets != 7 || readback.BypassCause != "auto" {
		t.Fatalf("readback lost bypass fields: %#v", readback)
	}
}
