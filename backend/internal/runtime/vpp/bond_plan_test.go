package vpp

import "testing"

func TestSplitBondInterfaceStatesMovesBondAddressToBondPlan(t *testing.T) {
	desired := Plan{
		Interfaces: []InterfaceState{
			{Name: "lyroute-enp2s0", AdminState: "up", LinkState: "up"},
			{Name: "bond-home-lan", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.99.1/24"}},
		},
		Bonds: []BondState{{Name: "bond-home-lan", Mode: "xor", Members: []string{"lyroute-enp2s0", "lyroute-enp4s0"}}},
	}
	interfaces := InterfaceBondPlan{TransactionID: "txn", Interfaces: append([]InterfaceState(nil), desired.Interfaces...)}
	bonds := InterfaceBondPlan{TransactionID: "txn", Bonds: append([]BondState(nil), desired.Bonds...)}

	physical, bondPlan := splitBondInterfaceStates(interfaces, bonds, desired, Plan{})
	if len(physical.Interfaces) != 1 || physical.Interfaces[0].Name != "lyroute-enp2s0" {
		t.Fatalf("physical interfaces = %#v, want only the member port", physical.Interfaces)
	}
	if len(bondPlan.Interfaces) != 1 || bondPlan.Interfaces[0].Name != "bond-home-lan" {
		t.Fatalf("bond interfaces = %#v, want the bond address state", bondPlan.Interfaces)
	}
	if len(bondPlan.Bonds) != 1 || bondPlan.Bonds[0].Name != "bond-home-lan" {
		t.Fatalf("bonds = %#v, want the bond object", bondPlan.Bonds)
	}
}

func TestBuildInterfaceBondOperationsCreatesBondBeforeAddress(t *testing.T) {
	plan := InterfaceBondPlan{
		TransactionID: "txn",
		Interfaces:    []InterfaceState{{Name: "bond-home-lan", AdminState: "up", LinkState: "up", Addresses: []string{"192.168.99.1/24"}}},
		Bonds:         []BondState{{Name: "bond-home-lan", Mode: "xor", Members: []string{"lyroute-enp2s0", "lyroute-enp4s0"}}},
	}
	operations, err := BuildInterfaceBondOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	bondIndex, addressIndex := -1, -1
	for index, operation := range operations {
		switch operation.Name {
		case "vpp.interface-bond":
			if bondIndex < 0 {
				bondIndex = index
			}
		case "vpp.interface.address":
			if addressIndex < 0 {
				addressIndex = index
			}
		}
	}
	if bondIndex < 0 || addressIndex < 0 {
		t.Fatalf("operations = %#v, want a bond and an address operation", operations)
	}
	if bondIndex > addressIndex {
		t.Fatalf("bond operation at %d runs after address at %d; the bond must exist before its address", bondIndex, addressIndex)
	}
}
