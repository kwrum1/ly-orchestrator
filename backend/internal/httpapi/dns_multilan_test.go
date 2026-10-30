package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

func TestDNSTransparentInterceptionSupported(t *testing.T) {
	single := []vpp.AddressAssignment{
		{Role: "lan", VPPInterface: "lyroute-ens256"},
		{Role: "wan", VPPInterface: "lyroute-ens224"},
	}
	if !dnsTransparentInterceptionSupported(single) {
		t.Fatalf("single LAN must support transparent DNS interception")
	}
	multi := []vpp.AddressAssignment{
		{Role: "lan", VPPInterface: "lyroute-ens256"},
		{Role: "lan", VPPInterface: "lyroute-lab20"},
	}
	if !dnsTransparentInterceptionSupported(multi) {
		t.Fatalf("multi-LAN must support DNS interception (the plugin keeps a vector of attachment slots)")
	}
	if dnsTransparentInterceptionSupported(nil) {
		t.Fatalf("no LAN must not support DNS interception")
	}
}
