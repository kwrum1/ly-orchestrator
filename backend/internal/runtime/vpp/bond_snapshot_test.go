package vpp

import "testing"

func TestGatewaySnapshotMatchesResourceIgnoresBondInterfaces(t *testing.T) {
	physical := InterfaceState{Name: "lyroute-enp2s0", AdminState: "up", LinkState: "up"}
	bond := InterfaceState{Name: "bond-test", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.90.1/24"}}
	desired := Plan{
		Interfaces: []InterfaceState{physical, bond},
		Bonds:      []BondState{{Name: "bond-test", Mode: "xor", Members: []string{"lyroute-enp2s0", "lyroute-enp4s0"}}},
	}
	snapshot := Snapshot{Interfaces: []InterfaceState{physical, bond}}
	if !GatewaySnapshotMatchesResource("interfaces", snapshot, desired) {
		t.Fatal("the interfaces resource must compare physical ports only; the bond lifecycle owns bond-test")
	}
}

func TestDecodeInterfacesToleratesAbsentTarget(t *testing.T) {
	request := SnapshotRequest{
		AbsentInterfaces: []string{"bond-test"},
		Candidates:       SnapshotCandidates{Interfaces: []InterfaceState{{Name: "bond-test"}}},
	}
	results := []VPPCTLCommandResult{{
		Command: "show interface address",
		Stdout:  "lyroute-enp2s0 (up):\n  L3 192.0.2.1/24\n",
	}}
	if _, err := decodeVPPCTLInterfaces(request, results); err != nil {
		t.Fatalf("an absent deletion target must not require a live row: %v", err)
	}
}

func TestDecodeBondsToleratesAbsentBondWhenAllowed(t *testing.T) {
	request := SnapshotRequest{
		Bonds:        []string{"bond-test"},
		AllowMissing: true,
		Candidates:   SnapshotCandidates{Bonds: []BondState{{Name: "bond-test"}}},
	}
	results := []VPPCTLCommandResult{{Command: "show bond details", Stdout: ""}}
	if _, err := decodeVPPCTLBonds(request, results); err != nil {
		t.Fatalf("a missing bond must be observable as drift: %v", err)
	}
}
