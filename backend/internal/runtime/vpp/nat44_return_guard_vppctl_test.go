package vpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
)

func TestRemoveNATReturnGuardAcceptsNoABFAttachments(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_VPP_INTERFACE", "lyroute-ens34")
	guard := natReturnGuardForPortMapping(nat.PortMapping{
		ID:              "first-port-map",
		ExternalAddress: "10.67.0.12",
		InternalHost:    "192.168.50.200",
		InternalPort:    8080,
		Protocol:        "tcp",
	})
	policyCommand := fmt.Sprintf("show abf policy %d", guard.policyID())
	responses := map[string]fakeVPPResponse{
		guard.preNATBypassDeleteCommand(): {stdout: "deleted\n"},
		policyCommand:                     {stdout: "Invalid policy\n"},
		"show ip fib summary":             {stdout: "IPv4 unicast:\n"},
		"show acl-plugin acl":             {stdout: ""},
		"show abf attach lyroute-ens34":   {retval: 1},
	}
	channel := vppctlChannel{binary: writeFakeVPPCTL(t, responses)}
	if _, err := channel.removeNATReturnGuard(context.Background(), Operation{RequestID: "txn-first-port-map", Resource: guard.resource}, guard); err != nil {
		t.Fatal(err)
	}
}

// TestReplaceNATReturnGuardVerifiesNarrowedACL pins the live apply contract:
// the ACL created for a port map is limited to the mapping's protocol and
// external port, and the post-create verification must expect that same rule.
// The old any/any expectation failed every port-map apply after the guard was
// installed, leaving the mapping absent from VPP and the resource stuck at
// desired_not_applied.
func TestReplaceNATReturnGuardVerifiesNarrowedACL(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_VPP_INTERFACE", "lyroute-ens34")
	guard := natReturnGuardForPortMapping(nat.PortMapping{
		ID:              "pm-http",
		Protocol:        "tcp",
		ExternalAddress: "10.1.18.25",
		ExternalPort:    8088,
		InternalHost:    "192.168.99.50",
		InternalPort:    80,
		WANInterface:    "lyroute-enp5s0",
		WANNextHop:      "10.1.18.1",
	})
	prefix, err := guard.sourcePrefix()
	if err != nil {
		t.Fatal(err)
	}
	const aclID = 7
	dir := t.TempDir()
	policyFlag := filepath.Join(dir, "policy.installed")
	attachFlag := filepath.Join(dir, "attach.installed")
	logPath := filepath.Join(dir, "commands.log")
	aclOutput := fmt.Sprintf("acl-index %d count 1 tag {%s}\n  0: ipv4 permit src %s dst 0.0.0.0/0 proto 6 sport 0-65535 dport 8088-8088\n", aclID, guard.tag(), prefix)
	policyID := itoa(guard.policyID())
	script := "#!/bin/sh\n" +
		"cmd=\"$*\"\n" +
		"if [ -n \"$FAKE_VPPCTL_LOG\" ]; then printf '%s\\n' \"$cmd\" >> \"$FAKE_VPPCTL_LOG\"; fi\n" +
		"case \"$cmd\" in\n" +
		"  \"show acl-plugin acl index " + itoa(aclID) + "\")\n" +
		"    cat <<'VPP_ACL'\n" + aclOutput + "VPP_ACL\n    ;;\n" +
		"  \"show acl-plugin acl\")\n    ;;\n" +
		"  \"show abf policy " + policyID + "\")\n" +
		"    if [ -f \"" + policyFlag + "\" ]; then echo 'abf:[0]: policy:" + policyID + " acl:" + itoa(aclID) + "'; else echo 'Invalid policy'; fi\n    ;;\n" +
		"  \"abf policy add \"*)\n" +
		"    : > \"" + policyFlag + "\"\n    ;;\n" +
		"  \"abf attach ip4 policy \"*)\n" +
		"    : > \"" + attachFlag + "\"\n    ;;\n" +
		"  \"abf attach ip4 del \"*)\n" +
		"    rm -f \"" + attachFlag + "\"\n    ;;\n" +
		"  \"show abf attach lyroute-ens34\")\n" +
		"    if [ -f \"" + attachFlag + "\" ]; then echo 'policy:" + policyID + "'; fi\n    ;;\n" +
		"  \"show ip fib summary\")\n    echo 'IPv4 unicast:'\n    ;;\n" +
		"  \"show ly-route pre-nat-route\")\n    echo 'rule id " + policyID + " priority 0 bypass 1'\n    ;;\n" +
		"  \"set acl-plugin acl permit \"*)\n    echo 'index:" + itoa(aclID) + "'\n    ;;\n" +
		"esac\n" +
		"exit 0\n"
	binary := filepath.Join(dir, "vppctl")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_VPPCTL_LOG", logPath)
	channel := vppctlChannel{binary: binary}
	if _, err := channel.replaceNATReturnGuard(context.Background(), Operation{RequestID: "txn-guard", Resource: guard.resource}, guard); err != nil {
		logged, _ := os.ReadFile(logPath)
		t.Fatalf("narrowed return guard verification failed: %v\ncommands:\n%s", err, logged)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "set acl-plugin acl permit src "+prefix+" dst 0.0.0.0/0 proto 6 sport 0-65535 dport 8088-8088") {
		t.Fatalf("guard ACL was not narrowed in the apply commands:\n%s", logged)
	}
}

func TestNATReturnGuardUsesExternalAddressAndOriginalWANPath(t *testing.T) {
	guard := natReturnGuardForPortMapping(nat.PortMapping{
		ID:              "public-web",
		ExternalAddress: "10.67.0.11",
		WANInterface:    "pppoe_session0",
		WANNextHop:      "10.67.0.1",
		InternalHost:    "192.168.88.20",
		InternalPort:    443,
		Protocol:        "tcp",
	})
	prefix, err := guard.sourcePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "192.168.88.20/32" {
		t.Fatalf("source prefix = %q", prefix)
	}
	if guard.via() != "10.67.0.1 pppoe_session0" {
		t.Fatalf("return path = %q", guard.via())
	}
}

func TestNATReturnGuardBypassesProxyPolicyBeforeNAT(t *testing.T) {
	guard := natReturnGuardForPortMapping(nat.PortMapping{
		ID:           "public-web",
		InternalHost: "192.168.88.20",
		InternalPort: 443,
		Protocol:     "tcp",
	})
	command, err := guard.preNATBypassCommand()
	if err != nil {
		t.Fatal(err)
	}
	want := "source 192.168.88.20/32 destination 0.0.0.0/0 protocol tcp sport 443-443 dport 0-65535 bypass"
	if !strings.Contains(command, want) {
		t.Fatalf("pre-NAT bypass command = %q, want %q", command, want)
	}
	if err := verifyNATPreRouteBypass("rule id "+itoa(guard.policyID())+" priority 0 bypass 1", guard); err != nil {
		t.Fatal(err)
	}
}

func TestNATReturnGuardPersistentCommandsRecreateACLAndPreNATBypass(t *testing.T) {
	mapping := nat.PortMapping{
		ID:              "public-web",
		ExternalAddress: "10.67.0.11",
		ExternalPort:    8443,
		InternalHost:    "192.168.88.20",
		InternalPort:    443,
		Protocol:        "tcp",
		WANInterface:    "pppoe_session0",
		WANNextHop:      "10.67.0.1",
	}
	guard := natReturnGuardForPortMapping(mapping)
	commands := strings.Join(natReturnGuardPersistentCommands(mapping, "192.168.88.0/24"), "\n")
	for _, want := range []string{
		"set ly-route pre-nat-route interface lyroute-$LY_ROUTE_LAN_INTERFACE lan-prefix 192.168.88.0/24",
		fmt.Sprintf("set acl-plugin acl index %d permit src 192.168.88.20/32", guard.aclID()),
		fmt.Sprintf("abf policy add id %d acl %d via 10.67.0.1 pppoe_session0", guard.policyID(), guard.aclID()),
		fmt.Sprintf("abf attach ip4 policy %d priority %d lyroute-$LY_ROUTE_LAN_INTERFACE", guard.policyID(), natReturnGuardPriority),
		"source 192.168.88.20/32 destination 0.0.0.0/0 protocol tcp sport 443-443 dport 0-65535 bypass",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("persistent NAT guard commands missing %q:\n%s", want, commands)
		}
	}
}

func TestNATSnapshotPreservesMappingAndMarksMissingReturnGuardAsDrift(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_VPP_INTERFACE", "lyroute-ens192")
	mapping := nat.PortMapping{ID: "public-web", Protocol: "tcp", ExternalAddress: "10.67.0.11", ExternalPort: 18080, InternalHost: "192.168.88.100", InternalPort: 18080, WANInterface: "pppoe_session0"}
	guard := natReturnGuardForPortMapping(mapping)
	request := SnapshotRequest{
		AllowMissing:          true,
		VerifyNATReturnGuards: true,
		NATPortMappings:       []string{mapping.ID},
		Candidates:            SnapshotCandidates{NATPortMappings: []nat.PortMapping{mapping}},
	}
	results := []VPPCTLCommandResult{
		{Command: "show nat44 static mappings", Stdout: "NAT44 static mappings:\n  TCP local 192.168.88.100:18080 external 10.67.0.11:18080 vrf 0\n"},
		{Command: "show acl-plugin acl", Stdout: ""},
		{Command: "show abf policy " + itoa(guard.policyID()), Stdout: "Invalid policy"},
		{Command: "show abf attach lyroute-ens192", Stdout: ""},
	}
	readback, err := decodeVPPCTLNAT44(request, results)
	if err != nil {
		t.Fatal(err)
	}
	if len(readback.PortMappings) != 1 || readback.PortMappings[0].ReturnPathGuard {
		t.Fatalf("missing guard readback = %#v, want mapping with guard=false", readback.PortMappings)
	}
}

func TestNATSnapshotFailsClosedWhenReturnGuardIsRequired(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_VPP_INTERFACE", "lyroute-ens192")
	mapping := nat.PortMapping{ID: "public-web", Protocol: "tcp", ExternalAddress: "10.67.0.11", ExternalPort: 18080, InternalHost: "192.168.88.100", InternalPort: 18080, WANInterface: "pppoe_session0"}
	guard := natReturnGuardForPortMapping(mapping)
	request := SnapshotRequest{
		VerifyNATReturnGuards: true,
		NATPortMappings:       []string{mapping.ID},
		Candidates:            SnapshotCandidates{NATPortMappings: []nat.PortMapping{mapping}},
	}
	results := []VPPCTLCommandResult{
		{Command: "show nat44 static mappings", Stdout: "NAT44 static mappings:\n  TCP local 192.168.88.100:18080 external 10.67.0.11:18080 vrf 0\n"},
		{Command: "show acl-plugin acl", Stdout: ""},
		{Command: "show abf policy " + itoa(guard.policyID()), Stdout: "Invalid policy"},
		{Command: "show abf attach lyroute-ens192", Stdout: ""},
	}
	_, err := decodeVPPCTLNAT44(request, results)
	if err == nil || !strings.Contains(err.Error(), "ACL is missing") {
		t.Fatalf("missing guard error = %v", err)
	}
}

func TestNATSnapshotTreatsMismatchedReturnGuardAsRepairableDrift(t *testing.T) {
	t.Setenv("LY_ROUTE_LAN_VPP_INTERFACE", "lyroute-ens192")
	mapping := nat.PortMapping{ID: "public-web", Protocol: "tcp", ExternalAddress: "10.67.0.11", ExternalPort: 18080, InternalHost: "192.168.88.100", InternalPort: 18080, WANInterface: "pppoe_session0", WANNextHop: "10.67.0.1"}
	guard := natReturnGuardForPortMapping(mapping)
	request := SnapshotRequest{
		AllowMissing:          true,
		VerifyNATReturnGuards: true,
		NATPortMappings:       []string{mapping.ID},
		Candidates:            SnapshotCandidates{NATPortMappings: []nat.PortMapping{mapping}},
	}
	results := []VPPCTLCommandResult{
		{Command: "show nat44 static mappings", Stdout: "NAT44 static mappings:\n  TCP local 192.168.88.100:18080 external 10.67.0.11:18080 vrf 0\n"},
		{Command: "show acl-plugin acl", Stdout: "acl-index 6 count 1 tag {" + guard.tag() + "}\n  0: ipv4 permit src 10.67.0.12/32 dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535\n"},
		{Command: "show abf policy " + itoa(guard.policyID()), Stdout: "abf:[0]: policy:" + itoa(guard.policyID()) + " acl:6\n"},
		{Command: "show abf attach lyroute-ens192", Stdout: "policy:" + itoa(guard.policyID()) + "\n"},
	}

	readback, err := decodeVPPCTLNAT44(request, results)
	if err != nil {
		t.Fatal(err)
	}
	if len(readback.PortMappings) != 1 || readback.PortMappings[0].ReturnPathGuard {
		t.Fatalf("mismatched guard readback = %#v, want mapping with guard=false", readback.PortMappings)
	}
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}

func TestNATReturnGuardKeepsEthernetNextHop(t *testing.T) {
	guard := natReturnGuardForStaticMapping(nat.StaticMapping{
		ID:              "public-host",
		ExternalAddress: "203.0.113.10",
		WANInterface:    "GigabitEthernet0/1/0",
		WANNextHop:      "203.0.113.1",
	})
	if guard.via() != "203.0.113.1 GigabitEthernet0/1/0" {
		t.Fatalf("return path = %q", guard.via())
	}
}

func TestObservedABFDeleteViaParsesPPPoEAttachedNextHop(t *testing.T) {
	output := `abf:[6]: policy:22371 acl:6
     path-list:[124] locks:1 flags:shared,no-uRPF, uRPF-list: None
      path:[135] pl-index:124 ip4 weight=1 pref=0 attached-nexthop:  oper-flags:resolved,
        10.67.0.1 pppoe_session0 (p2p)
      [@0]: ipv4 [features] via 0.0.0.0 pppoe_session0: mtu:9000 next:6 flags:[features]`

	if got, want := observedABFDeleteVia(output), "10.67.0.1 pppoe_session0"; got != want {
		t.Fatalf("ABF delete path = %q, want %q", got, want)
	}
}

func TestNATMappingDeleteOperationsRetainPayloadForGuardCleanup(t *testing.T) {
	mapping := nat.PortMapping{ID: "public-web", Protocol: "tcp", ExternalAddress: "203.0.113.10", ExternalPort: 8443, InternalHost: "192.168.88.20", InternalPort: 443}
	operations, err := BuildNAT44Operations(NAT44Plan{
		TransactionID:        "txn-delete",
		DeletePortMappings:   []string{mapping.ID},
		ReadbackPortMappings: []nat.PortMapping{mapping},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("operations = %#v", operations)
	}
	if got, ok := operations[0].Payload.(nat.PortMapping); !ok || got != mapping {
		t.Fatalf("delete payload = %#v", operations[0].Payload)
	}
	if !nat44MappingOperationDeletes(operations[0]) {
		t.Fatal("delete operation was classified as apply")
	}
}

func TestNATMappingApplyIsNotClassifiedAsDelete(t *testing.T) {
	mapping := nat.PortMapping{ID: "public-web", Protocol: "tcp", ExternalAddress: "203.0.113.10", ExternalPort: 8443, InternalHost: "192.168.88.20", InternalPort: 443}
	operations, err := BuildNAT44Operations(NAT44Plan{TransactionID: "txn-apply", PortMappings: []nat.PortMapping{mapping}})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || nat44MappingOperationDeletes(operations[0]) {
		t.Fatalf("apply operation classification = %#v", operations)
	}
	if !slices.Contains(operations[0].VPPCtlCommands, "?nat44 add static mapping tcp local 192.168.88.20 443 external 203.0.113.10 8443") {
		t.Fatalf("mapping commands = %#v", operations[0].VPPCtlCommands)
	}
}

func TestNATFullConeMappingApplyIsNotClassifiedAsDelete(t *testing.T) {
	mapping := nat.PortMapping{ID: "full-cone-web", Protocol: "udp", ExternalAddress: "203.0.113.11", ExternalPort: 5353, InternalHost: "192.168.88.21", InternalPort: 5353}
	operations, err := BuildNAT44Operations(NAT44Plan{
		TransactionID: "txn-full-cone-apply",
		Behavior:      nat.BehaviorFullCone,
		PortMappings:  []nat.PortMapping{mapping},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || nat44MappingOperationDeletes(operations[0]) {
		t.Fatalf("full-cone apply classification = %#v", operations)
	}
	if !slices.Contains(operations[0].VPPCtlCommands, "?nat44 ei add static mapping udp local 192.168.88.21 5353 external 203.0.113.11 5353") {
		t.Fatalf("full-cone mapping commands = %#v", operations[0].VPPCtlCommands)
	}
}
