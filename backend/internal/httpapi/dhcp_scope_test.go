package httpapi

import (
	"context"
	"testing"

	"ly-route/backend/internal/runtime/vpp"
)

// The default DHCP scope must be the logical LAN, never the management or WAN
// network. Only an assignment explicitly marked as LAN may become a Dnsmasq
// control interface, and the router/DNS address is derived from that LAN.
func TestDHCPLANControlInterfaceRejectsManagementAndWAN(t *testing.T) {
	server := &Server{}
	for label, role := range map[string]string{"management": "management", "wan": "wan", "unset": ""} {
		t.Run(label, func(t *testing.T) {
			_, _, ok := server.dhcpLANControlInterface(context.Background(), "eth0", []vpp.AddressAssignment{{
				ID:             "eth0",
				LinuxInterface: "eth0",
				VPPInterface:   "lyroute-eth0",
				CIDR:           "10.0.2.15/24",
				Role:           role,
			}})
			if ok {
				t.Fatalf("assignment role %q must not be accepted as a DHCP LAN", role)
			}
		})
	}
}

func TestDHCPLANControlInterfaceUsesLANScopeOnly(t *testing.T) {
	server := &Server{}
	assignments := []vpp.AddressAssignment{
		{ID: "eth0", LinuxInterface: "eth0", VPPInterface: "lyroute-eth0", CIDR: "10.0.2.15/24", Role: "management"},
		{ID: "eth2", LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", CIDR: "192.168.88.1/24", Role: "lan"},
	}
	controlInterface, router, ok := server.dhcpLANControlInterface(context.Background(), "eth2", assignments)
	if !ok || controlInterface != "lylan-eth2" || router != "192.168.88.1" {
		t.Fatalf("LAN scope resolved to interface=%q router=%q ok=%t", controlInterface, router, ok)
	}
}
