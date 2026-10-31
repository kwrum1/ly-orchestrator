package vpp

import (
	"strings"
	"testing"
)

func TestMirrorSpanCommandsDisableBeforeAnyDestination(t *testing.T) {
	mirror := &MirrorConfig{
		Destination:    "lyroute-eth3",
		LinuxInterface: "eth3",
		Sources: []MirrorSource{
			{Interface: "lyroute-eth1", LinuxInterface: "eth1", Direction: "both", L2: true},
			{Interface: "lyroute-eth2", LinuxInterface: "eth2", Direction: "rx", L2: false},
		},
	}
	commands := MirrorSpanCommands(mirror, []string{"lyroute-eth1", "lyroute-eth2", "lyroute-eth3"})

	firstSet := -1
	for index, command := range commands {
		if strings.HasPrefix(command, "set interface span ") {
			firstSet = index
			break
		}
	}
	if firstSet < 0 {
		t.Fatalf("no span set command: %#v", commands)
	}
	for index := 0; index < firstSet; index++ {
		if !strings.Contains(commands[index], "disable") {
			t.Fatalf("command %d runs before cleanup: %q", index, commands[index])
		}
	}
	for _, interfaceName := range []string{"lyroute-eth1", "lyroute-eth2", "lyroute-eth3"} {
		if !containsCommand(commands, "?set interface span "+interfaceName+" l2 disable") {
			t.Fatalf("missing l2 cleanup for %s: %#v", interfaceName, commands)
		}
		if !containsCommand(commands, "?set interface span "+interfaceName+" disable") {
			t.Fatalf("missing plain cleanup for %s: %#v", interfaceName, commands)
		}
	}
	if !containsCommand(commands, "set interface span lyroute-eth1 l2 destination lyroute-eth3 both") {
		t.Fatalf("missing l2 rx/tx session: %#v", commands)
	}
	if !containsCommand(commands, "set interface span lyroute-eth2 destination lyroute-eth3 rx") {
		t.Fatalf("non-L2 session must not emit l2 keyword: %#v", commands)
	}
	if commands[len(commands)-1] != "show interface span" {
		t.Fatalf("span readback must be the last command: %#v", commands)
	}
}

func TestMirrorSpanCommandsWithoutMirrorStillCleans(t *testing.T) {
	commands := MirrorSpanCommands(nil, []string{"lyroute-eth1", "lyroute-eth2"})
	if len(commands) != 5 {
		t.Fatalf("commands = %#v, want 2x2 disables plus readback", commands)
	}
	if strings.Join(commands, "\n") != strings.Join([]string{
		"?set interface span lyroute-eth1 l2 disable",
		"?set interface span lyroute-eth1 disable",
		"?set interface span lyroute-eth2 l2 disable",
		"?set interface span lyroute-eth2 disable",
		"show interface span",
	}, "\n") {
		t.Fatalf("cleanup order = %#v", commands)
	}
}

func TestParseMirrorSessionsReadsVPPTable(t *testing.T) {
	output := `Source                           Destination                       Device       L2
lyroute-enp2s0                   lyroute-enp3s0                   (    rx) (  none)
lyroute-enp4s0                   lyroute-enp3s0                   (  none) (  both)
`
	sessions := ParseMirrorSessions(output)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v", sessions)
	}
	if sessions[0].Source != "lyroute-enp2s0" || sessions[0].Destination != "lyroute-enp3s0" || sessions[0].Direction != "rx" || sessions[0].L2 {
		t.Fatalf("first session = %#v", sessions[0])
	}
	if sessions[1].Source != "lyroute-enp4s0" || sessions[1].Direction != "both" || !sessions[1].L2 {
		t.Fatalf("second session = %#v", sessions[1])
	}
	if parsed := ParseMirrorSessions(""); len(parsed) != 0 {
		t.Fatalf("empty span table parsed as %#v", parsed)
	}
}

func TestMirrorReadbackMatchesRequiresExactGeneration(t *testing.T) {
	mirror := &MirrorConfig{Destination: "lyroute-enp3s0", Sources: []MirrorSource{
		{Interface: "lyroute-enp2s0", Direction: "both", L2: true},
	}}
	live := "Source                           Destination                       Device       L2\n" +
		"lyroute-enp2s0                   lyroute-enp3s0                   (  none) (  both)\n"
	if err := MirrorReadbackMatches(live, mirror); err != nil {
		t.Fatalf("matching session rejected: %v", err)
	}
	if err := MirrorReadbackMatches("", mirror); err == nil {
		t.Fatal("missing session accepted")
	}
	if err := MirrorReadbackMatches(live, nil); err == nil {
		t.Fatal("stale session accepted after the mirror was removed")
	}
	wrongDirection := strings.ReplaceAll(live, "(  none) (  both)", "(    rx) (  both)")
	if err := MirrorReadbackMatches(wrongDirection, mirror); err == nil {
		t.Fatal("wrong direction accepted")
	}
	extra := live + "lyroute-enp4s0                   lyroute-enp3s0                   (  none) (  both)\n"
	if err := MirrorReadbackMatches(extra, mirror); err == nil {
		t.Fatal("unexpected extra source accepted")
	}
}

func TestMirrorSpanOperationRequiresTakenOverPorts(t *testing.T) {
	attachments := []NativeAttachment{{LinuxInterface: "eth1", VPPInterface: "lyroute-eth1"}}
	plan := Plan{Mirror: &MirrorConfig{Destination: "lyroute-eth9", LinuxInterface: "eth9", Sources: []MirrorSource{
		{Interface: "lyroute-eth1", LinuxInterface: "eth1", Direction: "both", L2: true},
	}}}
	if _, err := mirrorSpanOperation(plan, attachments); err == nil {
		t.Fatal("mirror destination that is not attached was accepted")
	}
	plan.Mirror.Destination = "lyroute-eth1"
	plan.Mirror.LinuxInterface = "eth1"
	plan.Mirror.Sources = []MirrorSource{{Interface: "lyroute-eth9", LinuxInterface: "eth9", Direction: "both", L2: true}}
	if _, err := mirrorSpanOperation(plan, attachments); err == nil {
		t.Fatal("mirror source that is not attached was accepted")
	}
}

func TestMirrorCleanupCommandsCoverDestinationAndSources(t *testing.T) {
	commands := MirrorCleanupCommands(MirrorConfig{Destination: "lyroute-eth3", Sources: []MirrorSource{{Interface: "lyroute-eth1"}}})
	for _, want := range []string{
		"?set interface span lyroute-eth1 l2 disable",
		"?set interface span lyroute-eth3 l2 disable",
		"show interface span",
	} {
		if !containsCommand(commands, want) {
			t.Fatalf("cleanup missing %q: %#v", want, commands)
		}
	}
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}
