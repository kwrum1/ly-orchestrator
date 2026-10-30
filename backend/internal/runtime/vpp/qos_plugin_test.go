package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/flow"
)

// writeQoSCapabilityVPPCTL creates a vppctl stub that answers the two read-only probes
// with the supplied inventory and `qos record` output. `qos record` is only
// reached when the inventory does not carry qos_plugin.so, so a stub installed
// for a legacy build intentionally fails the test if it is called.
func writeQoSCapabilityVPPCTL(t *testing.T, inventory, qosRecord, shaping string) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "vppctl")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"show plugins\") printf '%s\\n' " + qosShellQuoteLines(inventory) + " ;;\n" +
		"  \"qos record\") printf '%s\\n' " + qosShellQuote(qosRecord) + " ;;\n" +
		"  \"show ly-route smart-qos\") printf '%s\\n' " + qosShellQuote(shaping) + " ;;\n" +
		"  *) echo \"unknown input '$*'\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func qosShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func qosShellQuoteLines(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	quoted := make([]string, 0, len(lines))
	for _, line := range lines {
		quoted = append(quoted, qosShellQuote(line))
	}
	return strings.Join(quoted, " ")
}

func TestProbeQoSCapabilityReadsCoreQoSWithoutAPlugin(t *testing.T) {
	// VPP 26.06: qos record/store/mark live in vnet core, so a healthy build
	// has no qos_plugin.so. The pre-fix probe returned unsupported here.
	binary := writeQoSCapabilityVPPCTL(t,
		" 1. acl_plugin.so 26.06 Access Control Lists\n 62. policer_plugin.so 26.06 Policer",
		"qos record: interface must be specified",
		"unknown input `smart-qos'",
	)
	capability, err := ProbeQoSCapability(context.Background(), binary)
	if err != nil {
		t.Fatalf("ProbeQoSCapability error = %v", err)
	}
	if !capability.CoreQoS.Known || !capability.CoreQoS.Value {
		t.Fatalf("CoreQoS = %+v, want a known-present core CLI", capability.CoreQoS)
	}
	if !capability.Policer.Value {
		t.Fatalf("Policer = %+v, want present", capability.Policer)
	}
	if capability.FlowRate.Value {
		t.Fatalf("FlowRate = %+v, want absent", capability.FlowRate)
	}
	if status := capability.Status(); status != CapabilityUnsupported {
		t.Fatalf("Status = %q, want unsupported", status)
	}
	if missing := capability.Missing(); len(missing) != 2 || missing[0] != FlowRateRequirement || missing[1] != ShapingRequirement {
		t.Fatalf("Missing = %#v, want the flow-rate plugin and shaping CLI", missing)
	}
	if capability.Source != "runtime_probe" {
		t.Fatalf("Source = %q, want runtime_probe", capability.Source)
	}
}

func TestProbeQoSCapabilityAcceptsLegacyQoSPlugin(t *testing.T) {
	// Older builds keep the QoS features in qos_plugin.so; the inventory must
	// be accepted as proof without a CLI probe.
	binary := writeQoSCapabilityVPPCTL(t,
		" 1. qos_plugin.so 24.10 Quality of Service\n 62. policer_plugin.so 24.10 Policer\n 70. ly_route_smart_qos_plugin.so 24.10 Ly Route flow rate",
		"should-not-be-called",
		"state locked\nalgorithm fq-codel\nqualification production\nscheduler-thread 0",
	)
	capability, err := ProbeQoSCapability(context.Background(), binary)
	if err != nil {
		t.Fatalf("ProbeQoSCapability error = %v", err)
	}
	if status := capability.Status(); status != CapabilitySupported {
		t.Fatalf("Status = %q (%s), want supported", status, capability.Summary())
	}
}

func TestProbeQoSCapabilityReportsEveryMissingFacility(t *testing.T) {
	binary := writeQoSCapabilityVPPCTL(t,
		" 1. acl_plugin.so 26.06 Access Control Lists\n 41. abf_plugin.so 26.06 ABF",
		"qos record: interface must be specified",
		"unknown input `smart-qos'",
	)
	capability, err := ProbeQoSCapability(context.Background(), binary)
	if err != nil {
		t.Fatalf("ProbeQoSCapability error = %v", err)
	}
	missing := capability.Missing()
	if len(missing) != 3 || missing[0] != FlowRateRequirement || missing[1] != ShapingRequirement || missing[2] != PolicerRequirement {
		t.Fatalf("Missing = %#v, want flow-rate, shaping and policer", missing)
	}
	if !strings.Contains(capability.Summary(), "traffic-control unsupported") {
		t.Fatalf("Summary = %q, want an unsupported diagnosis", capability.Summary())
	}
}

func TestProbeQoSCapabilityTreatsUnknownCommandAsAbsent(t *testing.T) {
	binary := writeQoSCapabilityVPPCTL(t,
		" 1. acl_plugin.so 26.06 Access Control Lists\n 62. policer_plugin.so 26.06 Policer\n 70. ly_route_smart_qos_plugin.so 26.06 flow rate",
		"unknown input `qos record'",
		"unknown input `smart-qos'",
	)
	capability, err := ProbeQoSCapability(context.Background(), binary)
	if err != nil {
		t.Fatalf("ProbeQoSCapability error = %v", err)
	}
	if !capability.CoreQoS.Known || capability.CoreQoS.Value {
		t.Fatalf("CoreQoS = %+v, want a known-absent core CLI", capability.CoreQoS)
	}
	if missing := capability.Missing(); len(missing) != 2 || missing[0] != CoreQoSRequirement || missing[1] != ShapingRequirement {
		t.Fatalf("Missing = %#v, want core QoS and shaping", missing)
	}
}

func TestProbeQoSCapabilityKeepsUnknownFactsUnknown(t *testing.T) {
	binary := writeQoSCapabilityVPPCTL(t,
		" 1. acl_plugin.so 26.06 Access Control Lists\n 62. policer_plugin.so 26.06 Policer\n 70. ly_route_smart_qos_plugin.so 26.06 flow rate",
		"",
		"state locked\nalgorithm fq-codel\nqualification production\nscheduler-thread 0",
	)
	capability, err := ProbeQoSCapability(context.Background(), binary)
	if err != nil {
		t.Fatalf("ProbeQoSCapability error = %v", err)
	}
	if capability.CoreQoS.Known {
		t.Fatalf("CoreQoS = %+v, want unknown", capability.CoreQoS)
	}
	if status := capability.Status(); status != CapabilityUnknown {
		t.Fatalf("Status = %q, want unknown", status)
	}
}

func TestProbeQoSCapabilityFailsClosedWhenVVPPCTLCannotRun(t *testing.T) {
	capability, err := ProbeQoSCapability(context.Background(), filepath.Join(t.TempDir(), "missing-vppctl"))
	if err == nil {
		t.Fatal("missing vppctl did not report an error")
	}
	if status := capability.Status(); status != CapabilityUnknown {
		t.Fatalf("Status = %q, want unknown", status)
	}
	if strings.Contains(capability.Summary(), "verified") {
		t.Fatalf("Summary = %q, must not claim a verified dataplane", capability.Summary())
	}
}

func TestMissingQoSCapabilitiesMapsCompiledActions(t *testing.T) {
	capability := QoSCapability{CoreQoS: KnownFact(true), FlowRate: KnownFact(false), Policer: KnownFact(false), Source: "test"}
	groups := []flow.VPPObjectGroup{
		{Kind: "vpp.acl.drop"},
		{Kind: "vpp.qos.mark"},
		{Kind: "vpp.behavior.rate"},
		{Kind: "vpp.policer"},
	}
	missing := MissingQoSCapabilities(capability, groups)
	if len(missing) != 2 || missing[0] != FlowRateRequirement || missing[1] != PolicerRequirement {
		t.Fatalf("MissingQoSCapabilities = %#v, want flow-rate and policer", missing)
	}

	supported, unsupported, missing := SupportedQoSGroups(capability, groups)
	if missing == nil {
		t.Fatal("SupportedQoSGroups reported no missing facility")
	}
	if len(supported) != 2 || supported[0].Kind != "vpp.acl.drop" || supported[1].Kind != "vpp.qos.mark" {
		t.Fatalf("supported = %#v, want acl.drop and qos.mark", supported)
	}
	if len(unsupported) != 2 || unsupported[0].Kind != "vpp.behavior.rate" || unsupported[1].Kind != "vpp.policer" {
		t.Fatalf("unsupported = %#v, want behavior.rate and policer", unsupported)
	}

	targets := []flow.Target{{Kind: "vpp.acl.drop"}, {Kind: "vpp.behavior.rate"}}
	kept, dropped := SupportedQoSTargets(capability, targets)
	if len(kept) != 1 || kept[0].Kind != "vpp.acl.drop" || len(dropped) != 1 || dropped[0].Kind != "vpp.behavior.rate" {
		t.Fatalf("SupportedQoSTargets = %#v / %#v", kept, dropped)
	}
}

func TestMissingQoSCapabilitiesTreatsUnknownAsMissing(t *testing.T) {
	// Unknown facts are not present: a plan must not claim it can enforce an
	// action it never observed support for.
	capability := QoSCapability{CoreQoS: UnknownFact("probe failed"), FlowRate: KnownFact(true), Policer: KnownFact(true)}
	if missing := MissingQoSCapabilities(capability, []flow.VPPObjectGroup{{Kind: "vpp.qos.mark"}}); len(missing) != 1 || missing[0] != CoreQoSRequirement {
		t.Fatalf("missing = %#v, want the core QoS CLI", missing)
	}
	if status := capability.Status(); status != CapabilityUnknown {
		t.Fatalf("Status = %q, want unknown", status)
	}
}
