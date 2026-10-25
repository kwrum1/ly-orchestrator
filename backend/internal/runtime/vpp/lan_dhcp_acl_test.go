package vpp

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/trafficpolicy"
)

func commandText(commands []string) string { return strings.Join(commands, "\n") }

// The seeded "Default WAN Inbound Deny" rule compiles to an unconditional
// catch-all deny. An earlier revision attached it to the LAN output (which
// blackholes DHCP replies and every client return path) or to WAN ingress
// (which drops NAT44 return traffic before translation). It must never be
// deployed; NAT44-ED is stateful for unsolicited inbound traffic.
func TestSecurityACLCommandsNeverDeployBuiltInWANDeny(t *testing.T) {
	acl := trafficpolicy.SecurityACL{ID: builtInWANDenyACLID, Action: "deny"}
	commands := securityACLCommands(acl)
	if len(commands) != 0 {
		t.Fatalf("built-in WAN deny must not be deployed, got %#v", commands)
	}
}

func TestBuildACLQoSOperationsSkipsBuiltInWANDeny(t *testing.T) {
	operations, err := BuildACLQoSOperations(ACLQoSPlan{
		TransactionID:       "txn-lan-deny",
		IngressVPPInterface: "lyroute-enp0s4",
		ACLs:                []trafficpolicy.SecurityACL{{ID: builtInWANDenyACLID, Action: "deny"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.Name != "vpp.security-acl.rollback-delete" {
			t.Fatalf("built-in WAN deny must only be self-heal cleaned, got %#v", operations)
		}
		if !strings.Contains(commandText(operation.VPPCtlCommands), "?delete acl-plugin acl index") {
			t.Fatalf("self-heal cleanup must delete the stale ACL: %#v", operation.VPPCtlCommands)
		}
	}
}

func TestBuildOperationsSkipsBuiltInWANDeny(t *testing.T) {
	plan := provenPlan(Plan{
		RequestID: "req-builtin-wan-deny",
		Policy:    trafficpolicy.Config{SecurityACLs: []trafficpolicy.SecurityACL{{ID: builtInWANDenyACLID, Action: "deny"}}},
		AddressAssignments: []AddressAssignment{
			{ID: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24", Role: "lan"},
			{ID: "wan", LinuxInterface: "enp0s3", VPPInterface: "lyroute-enp0s3", CIDR: "100.77.0.2/24", Role: "wan"},
		},
	}, "enp0s3", "enp0s4")
	for _, operation := range mustBuildOperations(t, plan) {
		if operation.Name != "vpp.security-acl" {
			continue
		}
		if strings.Contains(commandText(operation.VPPCtlCommands), builtInWANDenyACLID) {
			t.Fatalf("built-in WAN deny operation must not be rendered: %#v", operation.VPPCtlCommands)
		}
	}
}

func TestBuildACLQoSOperationsKeepLANScopedSecurityACLs(t *testing.T) {
	acl := trafficpolicy.SecurityACL{
		ID:     "guest-block",
		Action: "deny",
		Match: trafficpolicy.Match{
			Sources:      []string{"192.168.77.0/24"},
			Destinations: []string{"10.9.0.0/16"},
			Protocols:    []string{"tcp"},
			Direction:    "input",
		},
	}
	operations, err := BuildACLQoSOperations(ACLQoSPlan{TransactionID: "txn-guest", ACLs: []trafficpolicy.SecurityACL{acl}})
	if err != nil {
		t.Fatal(err)
	}
	var user []Operation
	for _, operation := range operations {
		if operation.Resource == acl.ID {
			user = append(user, operation)
		}
	}
	if len(user) != 1 {
		t.Fatalf("operations = %#v, want the user ACL to stay attachable", operations)
	}
	text := commandText(user[0].VPPCtlCommands)
	if !strings.Contains(text, "?set interface input acl intfc lyroute-$LY_ROUTE_LAN_INTERFACE ip4-table ") {
		t.Fatalf("user security ACL lost its LAN attachment:\n%s", text)
	}
}

func lanDHCPTestPlan() Plan {
	return Plan{
		RequestID: "req-lan-dhcp",
		AddressAssignments: []AddressAssignment{
			{ID: "lan", LinuxInterface: "enp0s4", VPPInterface: "lyroute-enp0s4", CIDR: "192.168.77.2/24", Role: "lan"},
			{ID: "wan", LinuxInterface: "enp0s3", VPPInterface: "lyroute-enp0s3", CIDR: "100.77.0.2/24", Role: "wan"},
		},
		NAT: nat.CompiledConfig{
			Behavior: nat.BehaviorEndpointDependent,
			PortMappings: []nat.PortMapping{{
				ID:              "port-map",
				WANInterface:    "lyroute-enp0s3",
				ExternalAddress: "100.77.0.2",
				ExternalPort:    18080,
				InternalHost:    "192.168.77.1",
				InternalPort:    18088,
				Protocol:        "tcp",
			}},
		},
	}
}

func TestNAT44InitializeAddsOutsideWANPoolAddress(t *testing.T) {
	operations := mustBuildOperations(t, provenPlan(lanDHCPTestPlan(), "enp0s3", "enp0s4"))
	text := ""
	for _, operation := range operations {
		if operation.Name == "vpp.nat44.initialize" {
			text = commandText(operation.VPPCtlCommands)
			break
		}
	}
	if text == "" {
		t.Fatal("NAT44 initialize operation is missing")
	}
	for _, required := range []string{
		"?nat44 add interface address lyroute-enp0s3",
		"show nat44 addresses",
		"?set interface nat44 in lyroute-enp0s4",
		"set interface nat44 out lyroute-enp0s3 output-feature",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("NAT44 initialize is missing %q:\n%s", required, text)
		}
	}
}

func TestNAT44InitializeFallsBackToWANAssignment(t *testing.T) {
	plan := lanDHCPTestPlan()
	plan.NAT.PortMappings = nil
	outside := natOutsideInterfaces(plan)
	if len(outside) != 1 || outside[0] != "lyroute-enp0s3" {
		t.Fatalf("nat outside interfaces = %#v, want the configured WAN link", outside)
	}
}

func TestApplyNAT44AddsOutsideWANPoolAddress(t *testing.T) {
	mapping := nat.PortMapping{
		ID:              "web",
		Protocol:        "tcp",
		ExternalAddress: "100.77.0.2",
		ExternalPort:    18080,
		InternalHost:    "192.168.77.1",
		InternalPort:    18088,
		WANInterface:    "lyroute-enp0s3",
	}
	client := &nat44LifecycleClient{replies: map[string]Reply{
		"vpp.nat44-ed.snapshot": {Payload: NAT44Readback{PortMappings: []nat.PortMapping{mapping}}},
	}}
	_, err := (Adapter{Client: client}).ApplyNAT44(context.Background(), NAT44Plan{
		TransactionID:       "txn-nat-pool",
		Behavior:            nat.BehaviorEndpointDependent,
		IngressVPPInterface: "lyroute-enp0s4",
		WANIngressInterface: "lyroute-enp0s3",
		PortMappings:        []nat.PortMapping{mapping},
	}, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	initialize := ""
	for _, operation := range client.operations {
		if operation.Name == "vpp.nat44.initialize" {
			initialize = commandText(operation.VPPCtlCommands)
			break
		}
	}
	if initialize == "" {
		t.Fatal("NAT44 initialize operation was not applied")
	}
	for _, required := range []string{
		"?nat44 add interface address lyroute-enp0s3",
		"show nat44 addresses",
	} {
		if !strings.Contains(initialize, required) {
			t.Fatalf("NAT44 initialize is missing %q:\n%s", required, initialize)
		}
	}
}

func TestNAT44OutsideInterfacesPreferMappingsThenWANLink(t *testing.T) {
	plan := NAT44Plan{WANIngressInterface: "lyroute-enp0s3"}
	if got := nat44OutsideInterfaces(plan); len(got) != 1 || got[0] != "lyroute-enp0s3" {
		t.Fatalf("outside interfaces = %#v, want the configured WAN link", got)
	}
	plan.PortMappings = []nat.PortMapping{{WANInterface: "pppoe_session0"}}
	if got := nat44OutsideInterfaces(plan); len(got) != 1 || got[0] != "pppoe_session0" {
		t.Fatalf("outside interfaces = %#v, want the explicit mapping WAN", got)
	}
}
