package vpp

import (
	"strings"
	"testing"
)

// Every LAN gateway prefix must be routed through the interface that owns it.
// Using the first LAN for all prefixes made the intercept FIB reply to
// VLAN20/VLAN30 queries out of VLAN10, so DNS died on every LAN except the
// first one after a reboot.
func TestDNSTransparentCommandsRouteEachPrefixThroughItsOwnLAN(t *testing.T) {
	assignments := []AddressAssignment{
		{Role: "lan", LinuxInterface: "ens256", VPPInterface: "lyroute-ens256", CIDR: "192.168.10.1/24"},
		{Role: "lan", LinuxInterface: "lab20", VPPInterface: "lyroute-lab20", CIDR: "192.168.20.1/24"},
		{Role: "lan", LinuxInterface: "lab30", VPPInterface: "lyroute-lab30", CIDR: "192.168.30.1/24"},
		{Role: "wan", LinuxInterface: "ens224", VPPInterface: "lyroute-ens224", CIDR: "10.99.0.1/24"},
	}
	interception, ok := dnsTransparentInterception(assignments)
	if !ok {
		t.Fatal("transparent interception not detected for multi-LAN assignments")
	}
	if interception.LANInterface != "lyroute-ens256" {
		t.Fatalf("LANInterface = %q, want the first LAN", interception.LANInterface)
	}
	commands := strings.Join(dnsTransparentCommands(interception), "\n")
	want := []string{
		"ip route add table 100 192.168.10.1/24 via lyroute-ens256",
		"ip route add table 100 192.168.20.1/24 via lyroute-lab20",
		"ip route add table 100 192.168.30.1/24 via lyroute-lab30",
	}
	for _, command := range want {
		if !strings.Contains(commands, command) {
			t.Fatalf("commands missing %q:\n%s", command, commands)
		}
	}
	if strings.Contains(commands, "192.168.20.1/24 via lyroute-ens256") {
		t.Fatalf("VLAN20 prefix still routed through the first LAN:\n%s", commands)
	}
}

// A plan persisted before per-prefix routing only has the flat prefix list.
func TestDNSTransparentCommandsFallBackForLegacyPlans(t *testing.T) {
	legacy := DNSTransparentInterception{
		LANInterface:  "lyroute-ens256",
		LANInterfaces: []string{"lyroute-ens256"},
		IPv4Prefixes:  []string{"192.168.10.1/24"},
	}
	commands := strings.Join(dnsTransparentCommands(legacy), "\n")
	if !strings.Contains(commands, "ip route add table 100 192.168.10.1/24 via lyroute-ens256") {
		t.Fatalf("legacy plan route missing:\n%s", commands)
	}
}
