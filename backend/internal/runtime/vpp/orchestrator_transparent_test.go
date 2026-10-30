package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/orchestrator"
)

func TestBuildTransparentOrchestratorCommandsExpandsOrderedIPObjects(t *testing.T) {
	policy := orchestrator.PolicyView{
		SchemaVersion: 1,
		IPObjects: []orchestrator.IPObjectView{
			{ID: "office", Prefixes: []string{"192.168.10.0/24", "2001:db8:10::/64"}},
		},
		Groups: []orchestrator.PolicyGroupView{
			{ID: "security", Position: 10, Rules: []orchestrator.PolicyRuleView{
				{ID: "office-firewall", Sequence: 10, Match: orchestrator.PolicyMatchInput{Sources: []string{"office"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolTCP, DestinationPorts: []orchestrator.PortRangeInput{{Start: 443, End: 443}}}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionVia, Group: "firewall"}},
			}},
		},
		Default: orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
	}
	commands, err := BuildTransparentOrchestratorCommands(TransparentOrchestratorConfig{Generation: "sha256-test", Topology: transparentTestTopology(), Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for _, expected := range []string{
		"candidate boundary wan lyroute-wan0 lan lyroute-lan0",
		"candidate group firewall wan-facing lyroute-fw-wan lan-facing lyroute-fw-lan",
		"family ip4 src 192.168.10.0/24 dst 0.0.0.0/0 proto 6 sport 0-65535 dport 443-443",
		"family ip6 src 2001:db8:10::/64 dst ::/0 proto 6 sport 0-65535 dport 443-443",
		"candidate default direct",
		"commit generation sha256-test",
		"show ly-route orchestrator",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, joined)
		}
	}
}

func TestBuildTransparentOrchestratorCommandsFollowPersistedGroupAndRuleOrder(t *testing.T) {
	forward := orchestrator.PolicyView{
		SchemaVersion: 1,
		Groups: []orchestrator.PolicyGroupView{
			{ID: "first", Position: 10, Rules: []orchestrator.PolicyRuleView{
				{ID: "first-a", Sequence: 10, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolAny}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionDirect}},
				{ID: "first-b", Sequence: 20, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolAny}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionDirect}},
			}},
			{ID: "second", Position: 20, Rules: []orchestrator.PolicyRuleView{
				{ID: "second-a", Sequence: 10, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolAny}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionDirect}},
			}},
		},
		Default: orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
	}
	reordered := orchestrator.PolicyView{
		SchemaVersion: 1,
		Groups: []orchestrator.PolicyGroupView{
			{ID: "second", Position: 10, Rules: []orchestrator.PolicyRuleView{
				{ID: "second-a", Sequence: 10, Match: forward.Groups[1].Rules[0].Match, Action: forward.Groups[1].Rules[0].Action},
			}},
			{ID: "first", Position: 20, Rules: []orchestrator.PolicyRuleView{
				{ID: "first-b", Sequence: 10, Match: forward.Groups[0].Rules[1].Match, Action: forward.Groups[0].Rules[1].Action},
				{ID: "first-a", Sequence: 20, Match: forward.Groups[0].Rules[0].Match, Action: forward.Groups[0].Rules[0].Action},
			}},
		},
		Default: orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
	}
	before, err := BuildTransparentOrchestratorCommands(TransparentOrchestratorConfig{Generation: "forward", Topology: transparentTestTopology(), Policy: &forward})
	if err != nil {
		t.Fatal(err)
	}
	after, err := BuildTransparentOrchestratorCommands(TransparentOrchestratorConfig{Generation: "reordered", Topology: transparentTestTopology(), Policy: &reordered})
	if err != nil {
		t.Fatal(err)
	}
	indexOf := func(commands []string, fragment string) int {
		for index, command := range commands {
			if strings.Contains(command, fragment) {
				return index
			}
		}
		return -1
	}
	for name, commands := range map[string][]string{"forward": before, "reordered": after} {
		first := indexOf(commands, "rule id first-a")
		second := indexOf(commands, "rule id first-b")
		if first < 0 || second < 0 {
			t.Fatalf("%s commands missing rule: %v", name, commands)
		}
		if name == "forward" && first > second {
			t.Fatalf("forward rule order = first-a@%d first-b@%d", first, second)
		}
		if name == "reordered" && first < second {
			t.Fatalf("reordered rule order = first-b@%d first-a@%d", second, first)
		}
	}
	if indexOf(before, "rule id first-a") > indexOf(before, "rule id second-a") {
		t.Fatal("forward group order was not preserved")
	}
	if indexOf(after, "rule id second-a") > indexOf(after, "rule id first-a") {
		t.Fatal("reordered group order was not preserved")
	}
	if strings.Join(before, "\n") == strings.Join(after, "\n") {
		t.Fatal("order-only change produced identical dataplane commands")
	}
}

func TestBuildTransparentOrchestratorCommandsRejectsUnknownViaGroup(t *testing.T) {
	policy := orchestrator.PolicyView{SchemaVersion: 1, Groups: []orchestrator.PolicyGroupView{{ID: "default", Position: 10, Rules: []orchestrator.PolicyRuleView{{ID: "bad", Sequence: 10, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolAny}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionVia, Group: "missing"}}}}}, Default: orchestrator.ActionInput{Kind: orchestrator.ActionDirect}}
	_, err := BuildTransparentOrchestratorCommands(TransparentOrchestratorConfig{Generation: "test", Topology: transparentTestTopology(), Policy: &policy})
	if err == nil || !strings.Contains(err.Error(), "unknown orchestration group") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildTransparentOrchestratorDisableCommandsLocksAndReadsBack(t *testing.T) {
	commands := BuildTransparentOrchestratorDisableCommands()
	want := []string{"set ly-route orchestrator disable", "show ly-route orchestrator"}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("disable commands = %#v, want %#v", commands, want)
	}
}

func transparentTestTopology() orchestrator.TopologyView {
	return orchestrator.TopologyView{
		SchemaVersion: 1,
		Interfaces: []orchestrator.InterfaceView{
			{Name: "wan", Role: orchestrator.RoleWAN, Port: "wan0"},
			{Name: "lan", Role: orchestrator.RoleLAN, Port: "lan0"},
		},
		Groups: []orchestrator.GroupView{{Name: "firewall", Ports: []orchestrator.DirectedPortView{{Interface: "fw-wan", Direction: orchestrator.DirectionWANFacing}, {Interface: "fw-lan", Direction: orchestrator.DirectionLANFacing}}}},
	}
}

func TestBuildTransparentOrchestratorCommandsKeepsPortDirectionAndRanges(t *testing.T) {
	policy := orchestrator.PolicyView{
		SchemaVersion: 1,
		Groups: []orchestrator.PolicyGroupView{{ID: "route", Position: 10, Rules: []orchestrator.PolicyRuleView{
			{ID: "via-dport-range", Sequence: 10, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolTCP, DestinationPorts: []orchestrator.PortRangeInput{{Start: 443, End: 444}}}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionVia, Group: "firewall"}},
			{ID: "drop-dport-range", Sequence: 20, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolTCP, DestinationPorts: []orchestrator.PortRangeInput{{Start: 4443, End: 4444}}}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionDrop}},
			{ID: "via-sport", Sequence: 30, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolTCP, SourcePorts: []orchestrator.PortRangeInput{{Start: 443, End: 443}}}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionVia, Group: "firewall"}},
			{ID: "tcp-all", Sequence: 40, Match: orchestrator.PolicyMatchInput{Sources: []string{"any"}, Destinations: []string{"any"}, Protocol: orchestrator.ProtocolTCP}, Action: orchestrator.ActionInput{Kind: orchestrator.ActionVia, Group: "firewall"}},
		}}},
		Default: orchestrator.ActionInput{Kind: orchestrator.ActionDirect},
	}
	commands, err := BuildTransparentOrchestratorCommands(TransparentOrchestratorConfig{Generation: "port-contract", Topology: transparentTestTopology(), Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for _, expected := range []string{
		"id via-dport-range group-position 10 sequence 10 action via target firewall family ip4 src 0.0.0.0/0 dst 0.0.0.0/0 proto 6 sport 0-65535 dport 443-444",
		"id drop-dport-range group-position 10 sequence 20 action drop target none family ip4 src 0.0.0.0/0 dst 0.0.0.0/0 proto 6 sport 0-65535 dport 4443-4444",
		"id via-sport group-position 10 sequence 30 action via target firewall family ip4 src 0.0.0.0/0 dst 0.0.0.0/0 proto 6 sport 443-443 dport 0-65535",
		"id tcp-all group-position 10 sequence 40 action via target firewall family ip4 src 0.0.0.0/0 dst 0.0.0.0/0 proto 6 sport 0-65535 dport 0-65535",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, joined)
		}
	}
	if strings.Count(joined, "id tcp-all ") != 2 {
		t.Fatalf("non-port TCP rule should produce one IPv4 and one IPv6 variant, got:\n%s", joined)
	}
}
