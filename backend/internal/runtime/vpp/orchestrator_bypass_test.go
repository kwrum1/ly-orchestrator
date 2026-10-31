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

func TestTransparentOrchestratorReadbackCarriesBypass(t *testing.T) {
	readback := TransparentOrchestratorReadback{
		Generation: "abc", State: "running", Default: "drop",
		Bypass: true, BypassCause: "auto", BypassReason: "probe", BypassForced: false, BypassPackets: 7,
	}
	if !readback.Bypass || readback.BypassPackets != 7 || readback.BypassCause != "auto" {
		t.Fatalf("readback lost bypass fields: %#v", readback)
	}
}
