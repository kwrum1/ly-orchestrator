package vpp

import "testing"

func TestSecurityACLTargetsUseResolvedLANInterface(t *testing.T) {
	channel := vppctlChannel{lanVPPInterface: " lyroute-ens34 "}
	targets := channel.securityACLTargets(Operation{}, "input")
	if len(targets) != 1 || targets[0].Interface != "lyroute-ens34" || targets[0].Direction != "input" {
		t.Fatalf("security ACL targets = %#v, want lyroute-ens34 input", targets)
	}
}

func TestSecurityACLTargetsUseGeneratedWANCommand(t *testing.T) {
	operation := Operation{VPPCtlCommands: []string{"?set interface input acl intfc pppoe_session1 ip4-table 42"}}
	targets := (vppctlChannel{lanVPPInterface: "lyroute-ens34"}).securityACLTargets(operation, "output")
	if len(targets) != 1 || targets[0].Interface != "pppoe_session1" || targets[0].Direction != "input" {
		t.Fatalf("security ACL targets = %#v, want pppoe_session1 input", targets)
	}
}

func TestSecurityACLTargetsKeepEveryLANAttachment(t *testing.T) {
	operation := Operation{VPPCtlCommands: []string{
		"?set interface input acl intfc lyroute-enp3s0 ip4-table 42",
		"?set interface input acl intfc lyroute-enp4s0 ip4-table 42",
		"?set interface output acl intfc lyroute-enp4s0 ip4-table 42",
	}}
	targets := (vppctlChannel{lanVPPInterface: "lyroute-enp3s0"}).securityACLTargets(operation, "both")
	want := []securityACLAttachment{
		{Interface: "lyroute-enp3s0", Direction: "input"},
		{Interface: "lyroute-enp4s0", Direction: "input"},
		{Interface: "lyroute-enp4s0", Direction: "output"},
	}
	if len(targets) != len(want) {
		t.Fatalf("security ACL targets = %#v, want %#v", targets, want)
	}
	for index := range want {
		if targets[index] != want[index] {
			t.Fatalf("security ACL targets = %#v, want %#v", targets, want)
		}
	}
}
