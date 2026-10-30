package vpp

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/trafficpolicy"
)

// The seeded default WAN deny is an unconditional catch-all. Attaching it to
// any interface is destructive: LAN output/input blackholes DHCP replies and
// every client return path, and WAN input drops NAT44 return traffic and
// port-forwarded flows before translation. NAT44-ED is stateful, so the rule
// must never be deployed.
func TestBuildOperationsDefaultWANDenyIsNeverDeployed(t *testing.T) {
	acl := trafficpolicy.SecurityACL{
		ID:     "sec-acl-default-deny-wan",
		Action: "deny",
		Match: trafficpolicy.Match{
			Sources:      []string{"0.0.0.0/0"},
			Destinations: []string{"0.0.0.0/0"},
			Protocols:    []string{"any"},
			Direction:    "input",
		},
	}
	commands := strings.Join(securityACLCommands(acl), "\n")
	if commands != "" {
		t.Fatalf("default WAN deny must not be deployed: %q", commands)
	}
}
