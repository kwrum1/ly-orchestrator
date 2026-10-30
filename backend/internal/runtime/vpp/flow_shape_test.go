package vpp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/flow"
)

func TestFlowShapeCommandsUsePluginCLIContract(t *testing.T) {
	target := flow.Target{
		Kind:        "vpp.shape",
		RuleID:      "shape-lan",
		Shaping:     &flow.Shaping{RateBPS: 1_000_000, BurstBPS: 100_000, Direction: "downlink", HostIsolation: flow.HostIsolationAuto},
		Attachments: []string{"output:lyroute-eth1"},
	}
	commands := flowTargetCommands(target)
	if len(commands) != 2 || commands[1] != "show ly-route smart-qos" {
		t.Fatalf("commands = %#v, want one set plus show ly-route smart-qos", commands)
	}
	want := "set ly-route smart-qos interface lyroute-eth1 rate 1000 burst-bytes 12500 host-isolation destination"
	if commands[0] != want {
		t.Fatalf("shape command = %q, want %q", commands[0], want)
	}
}

func TestFlowShapeDefaultBurstOmitsTokenAndUsesUplinkHostKey(t *testing.T) {
	target := flow.Target{
		Kind:        "vpp.shape",
		RuleID:      "shape-wan",
		Shaping:     &flow.Shaping{RateBPS: 1_000_500, Direction: "uplink"},
		Attachments: []string{"output:lyroute-wan0"},
	}
	joined := strings.Join(flowTargetCommands(target), "\n")
	if !strings.Contains(joined, "set ly-route smart-qos interface lyroute-wan0 rate 1001 host-isolation source") {
		t.Fatalf("shape command = %q, want rounded kbps and source isolation", joined)
	}
	if strings.Contains(joined, "burst-bytes") {
		t.Fatalf("shape command = %q, default burst must be left to the plugin", joined)
	}
}

func TestFlowShapeGroupCommandsAreDeterministic(t *testing.T) {
	group := flow.VPPObjectGroup{Kind: "vpp.shape", Objects: []flow.VPPObject{
		{RuleID: "shape-lan", Shaping: &flow.Shaping{RateBPS: 1_000_000, Direction: "downlink"}, Attachments: []string{"output:lyroute-eth1"}},
		{RuleID: "shape-lan", Shaping: &flow.Shaping{RateBPS: 1_000_000, Direction: "uplink"}, Attachments: []string{"output:lyroute-wan0"}},
	}}
	first := strings.Join(flowGroupCommands(group), "\n")
	second := strings.Join(flowGroupCommands(group), "\n")
	if first != second {
		t.Fatalf("shape group commands are not deterministic:\n%s\n---\n%s", first, second)
	}
	if strings.Count(first, "show ly-route smart-qos") != 1 {
		t.Fatalf("show readback must be deduplicated, got %q", first)
	}
}

func TestResolveFlowShapingInterfacesExpandsLANAndWAN(t *testing.T) {
	compiled := flow.CompiledIntent{VPPGroups: []flow.VPPObjectGroup{{Kind: "vpp.shape", Objects: []flow.VPPObject{{
		RuleID:  "shape-both",
		Shaping: &flow.Shaping{RateBPS: 1_000_000, Direction: "both"},
		Attachments: []string{
			"output:lyroute-$LY_ROUTE_LAN_INTERFACE",
			"output:lyroute-$LY_ROUTE_WAN_INTERFACE",
		},
	}}}}}
	assignments := []AddressAssignment{
		{LinuxInterface: "eth1", VPPInterface: "lyroute-eth1", Role: "lan"},
		{LinuxInterface: "wan0", VPPInterface: "lyroute-wan0", Role: "wan"},
	}
	if err := ResolveFlowShapingInterfaces(&compiled, assignments); err != nil {
		t.Fatal(err)
	}
	objects := compiled.VPPGroups[0].Objects
	if len(objects) != 2 {
		t.Fatalf("objects = %#v, want one per interface", objects)
	}
	byInterface := map[string]flow.VPPObject{}
	for _, object := range objects {
		byInterface[shapeObjectInterface(object)] = object
	}
	lan := byInterface["lyroute-eth1"]
	if lan.Shaping == nil || lan.Shaping.Direction != "downlink" || lan.Shaping.HostIsolation != flow.HostIsolationDestination {
		t.Fatalf("LAN object = %#v, want downlink/destination", lan)
	}
	wan := byInterface["lyroute-wan0"]
	if wan.Shaping == nil || wan.Shaping.Direction != "uplink" || wan.Shaping.HostIsolation != flow.HostIsolationSource {
		t.Fatalf("WAN object = %#v, want uplink/source", wan)
	}
}

func TestResolveFlowShapingInterfacesFailsClosedWithoutWAN(t *testing.T) {
	compiled := flow.CompiledIntent{VPPGroups: []flow.VPPObjectGroup{{Kind: "vpp.shape", Objects: []flow.VPPObject{{
		RuleID:      "shape-up",
		Shaping:     &flow.Shaping{RateBPS: 1_000_000, Direction: "uplink"},
		Attachments: []string{"output:lyroute-$LY_ROUTE_WAN_INTERFACE"},
	}}}}}
	err := ResolveFlowShapingInterfaces(&compiled, []AddressAssignment{{VPPInterface: "lyroute-eth1", Role: "lan"}})
	if err == nil || !strings.Contains(err.Error(), "uplink requires a configured WAN interface") {
		t.Fatalf("ResolveFlowShapingInterfaces = %v, want a missing-WAN error", err)
	}
	if err := ValidateFlowShapingInterfaces(compiled, nil); err == nil {
		t.Fatal("ValidateFlowShapingInterfaces accepted an uplink shape without a WAN")
	}
}

func TestVerifyShapeResultMatchesRateBurstAndIsolation(t *testing.T) {
	object := flow.VPPObject{
		RuleID:      "shape-lan",
		Shaping:     &flow.Shaping{RateBPS: 1_000_000, BurstBPS: 100_000, Direction: "downlink"},
		Attachments: []string{"output:lyroute-eth1"},
	}
	valid := "state running\nalgorithm fq-codel\nqualification production\nscheduler-thread 1\n" +
		"interface lyroute-eth1 enabled rate-kbps 1000 burst-bytes 12500 host-isolation destination queued 12 enqueued 100 transmitted 88 aqm-drops 0 overflow-drops 0\n"
	results := []VPPCTLCommandResult{{Command: "show ly-route smart-qos", Stdout: valid}}
	if err := verifyShapeResult(results, object); err != nil {
		t.Fatalf("verifyShapeResult(valid) = %v", err)
	}
	drift := strings.Replace(valid, "rate-kbps 1000", "rate-kbps 2000", 1)
	if err := verifyShapeResult([]VPPCTLCommandResult{{Command: "show ly-route smart-qos", Stdout: drift}}, object); err == nil || !qosObjectParametersMismatch(err) {
		t.Fatalf("shape parameter drift = %v, want a repairable mismatch", err)
	}
	for _, mutation := range []string{
		strings.Replace(valid, "rate-kbps 1000", "rate-kbps 2000", 1),
		strings.Replace(valid, "burst-bytes 12500", "burst-bytes 65536", 1),
		strings.Replace(valid, "host-isolation destination", "host-isolation source", 1),
		"state locked\nalgorithm fq-codel\nqualification production\nscheduler-thread 1\n",
	} {
		results[0].Stdout = mutation
		if err := verifyShapeResult(results, object); err == nil {
			t.Fatalf("verifyShapeResult accepted %q", mutation)
		}
	}
}

func TestFlowShapeDeleteCommandsDisableAndReadBack(t *testing.T) {
	target := flow.Target{
		Kind:        "vpp.shape",
		RuleID:      "shape-lan",
		Shaping:     &flow.Shaping{RateBPS: 1_000_000, Direction: "downlink"},
		Attachments: []string{"output:lyroute-eth1"},
	}
	commands := flowTargetDeleteCommands(target)
	want := []string{"?set ly-route smart-qos interface lyroute-eth1 disable", "show ly-route smart-qos"}
	if len(commands) != len(want) || commands[0] != want[0] || commands[1] != want[1] {
		t.Fatalf("delete commands = %#v, want %#v", commands, want)
	}
}

func TestFlowShapeApplyIsIdempotentAndDeleteProvesAbsence(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	binary := writeFakeVPPCTL(t, map[string]fakeVPPResponse{
		"set ly-route smart-qos interface lyroute-eth1 rate 1000 burst-bytes 12500 host-isolation destination": {stdout: ""},
		"show ly-route smart-qos": {stdout: "state running\nalgorithm fq-codel\nqualification production\nscheduler-thread 1\n" +
			"interface lyroute-eth1 enabled rate-kbps 1000 burst-bytes 12500 host-isolation destination queued 0 enqueued 0 transmitted 0 aqm-drops 0 overflow-drops 0\n"},
		"set ly-route smart-qos interface lyroute-eth1 disable": {stdout: ""},
	})
	channel := &vppctlChannel{binary: binary, cache: newVPPCTLCache(defaultVPPCTLCacheTTL)}
	group := flow.VPPObjectGroup{Kind: "vpp.shape", Objects: []flow.VPPObject{{
		RuleID:      "shape-lan",
		Shaping:     &flow.Shaping{RateBPS: 1_000_000, BurstBPS: 100_000, Direction: "downlink", HostIsolation: flow.HostIsolationDestination},
		Attachments: []string{"output:lyroute-eth1"},
	}}}
	operation := Operation{Name: "vpp.qos", VPPCtlCommands: flowGroupCommands(group)}
	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatalf("first shape apply: %v", err)
	}
	if _, err := channel.Do(context.Background(), operation); err != nil {
		t.Fatalf("second shape apply: %v", err)
	}
	deleteOperation := Operation{Name: "vpp.qos", VPPCtlCommands: flowGroupDeleteCommands(group)}
	if _, err := channel.Do(context.Background(), deleteOperation); err != nil {
		t.Fatalf("shape delete: %v", err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logged), "rate 1000 burst-bytes 12500") != 2 {
		t.Fatalf("apply log = %s, want the same set command twice", logged)
	}
	if !strings.Contains(string(logged), "interface lyroute-eth1 disable") {
		t.Fatalf("delete log = %s, want a disable command", logged)
	}

	object := group.Objects[0]
	present := []VPPCTLCommandResult{{Command: "show ly-route smart-qos", Stdout: "state running\nalgorithm fq-codel\nqualification production\nscheduler-thread 1\n" +
		"interface lyroute-eth1 enabled rate-kbps 1000 burst-bytes 12500 host-isolation destination queued 0 enqueued 0 transmitted 0 aqm-drops 0 overflow-drops 0\n"}}
	if absent, err := shapeObjectAbsent(present, object); err != nil || absent {
		t.Fatalf("shapeObjectAbsent(present) = %v/%v, want present", absent, err)
	}
	cleared := []VPPCTLCommandResult{{Command: "show ly-route smart-qos", Stdout: "state locked\nalgorithm fq-codel\nqualification production\nscheduler-thread 1\n"}}
	if absent, err := shapeObjectAbsent(cleared, object); err != nil || !absent {
		t.Fatalf("shapeObjectAbsent(cleared) = %v/%v, want absent", absent, err)
	}
}
