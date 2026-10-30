package httpapi

import (
	"context"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/runtime/vpp"
)

func TestSecurityGenerationIncludesStandaloneACL(t *testing.T) {
	server := New()
	policy := trafficpolicy.Config{SecurityACLs: []trafficpolicy.SecurityACL{{
		ID: "block-client", Priority: 5, Action: "deny",
		Match: trafficpolicy.Match{Sources: []string{"192.168.50.151/32"}, Destinations: []string{"10.67.0.1/32"}, Protocols: []string{"tcp"}, DestPorts: []string{"18001"}, Direction: "input"},
	}}}
	generation, err := server.currentSecurityGeneration(context.Background(), policy, []vpp.AddressAssignment{{Role: "lan", VPPInterface: "lyroute-ens34"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 1 || generation.ACLs[0].Interface != "lyroute-ens34" {
		t.Fatalf("standalone ACL generation = %#v", generation)
	}
	found := false
	for _, rule := range generation.ACLs[0].Rules {
		if rule.ID == "block-client" {
			found = true
		}
	}
	if !found {
		t.Fatalf("standalone ACL generation lost block-client: %#v", generation.ACLs[0].Rules)
	}
}

// The P0 regression: with two LANs the generation used the first LAN for every
// ACL, so a rule naming 192.168.99.0/24 was attached to the 192.168.88.x port.
func TestSecurityGenerationBindsACLToTheLANItsSourceNames(t *testing.T) {
	server := New()
	policy := trafficpolicy.Config{SecurityACLs: []trafficpolicy.SecurityACL{{
		ID: "acl-block-ssh", Priority: 10, Action: "deny",
		Match: trafficpolicy.Match{Sources: []string{"192.168.99.0/24"}, Destinations: []string{"10.1.18.100/32"}, Protocols: []string{"tcp"}, DestPorts: []string{"22"}, Direction: "input"},
	}}}
	assignments := []vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "enp3s0", VPPInterface: "lyroute-enp3s0", CIDR: "192.168.88.1/24"},
		{Role: "lan", LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", CIDR: "192.168.99.1/24"},
	}
	generation, err := server.currentSecurityGeneration(context.Background(), policy, assignments)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.ACLs) != 1 || generation.ACLs[0].Interface != "lyroute-enp4s0" || generation.ACLs[0].Direction != "input" {
		t.Fatalf("ACL generation = %#v, want only lyroute-enp4s0 input", generation.ACLs)
	}
}
