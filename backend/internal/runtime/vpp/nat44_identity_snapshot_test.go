package vpp

import "testing"

func TestParseNAT44RowsIgnoresInterfaceIdentityMapping(t *testing.T) {
	output := "NAT44 static mappings:\n" +
		" identity mapping 100.64.0.102 vrf 0\n" +
		" tcp local 192.168.88.100:8080 external 100.64.0.102:18080 vrf 0\n"
	statics, ports, err := parseNAT44Rows(output)
	if err != nil {
		t.Fatalf("identity mapping broke the NAT snapshot: %v", err)
	}
	if len(statics) != 0 {
		t.Fatalf("identity mapping was treated as a static mapping: %#v", statics)
	}
	if len(ports) != 1 || ports[0].internalPort != 8080 || ports[0].externalPort != 18080 {
		t.Fatalf("port mappings = %#v", ports)
	}
}
