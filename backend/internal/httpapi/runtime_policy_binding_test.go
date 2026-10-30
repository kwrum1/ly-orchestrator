package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/runtime/vpp"
)

// The seeded default WAN deny is an unconditional catch-all. It cannot be
// deployed on any interface without blackholing the LAN or NAT44 return
// traffic, so the dataplane policy excludes it for every WAN type.
func TestRuntimePolicyExcludesDefaultWANDenyForEveryWANType(t *testing.T) {
	policy := trafficpolicy.Config{SecurityACLs: []trafficpolicy.SecurityACL{
		{ID: "sec-acl-default-deny-wan"},
		{ID: "user-rule"},
	}}
	for name, assignments := range map[string][]vpp.AddressAssignment{
		"lan-only":    {{Role: "lan", VPPInterface: "lyroute-eth1"}},
		"static-wan":  {{Role: "lan", VPPInterface: "lyroute-eth1"}, {Role: "wan", VPPInterface: "lyroute-eth2"}},
		"dynamic-wan": {{Role: "lan", VPPInterface: "lyroute-eth1"}, {Role: "wan", VPPInterface: "pppoe_session0"}},
	} {
		filtered := runtimePolicyForAddressAssignments(policy, assignments)
		if len(filtered.SecurityACLs) != 1 || filtered.SecurityACLs[0].ID != "user-rule" {
			t.Fatalf("%s runtime ACLs = %#v, want only the user rule", name, filtered.SecurityACLs)
		}
	}
}
