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
