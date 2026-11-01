package vpp

import (
	"errors"
	"testing"
	"time"
)

func TestBuildOperationsBondAddressInheritsMemberProofs(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	proof := CapabilityProof{
		Hook:            NativeHookAFXDP,
		Mode:            NativeModeZeroCopy,
		Source:          ProofSourceRuntimeProbe,
		RuntimeVerified: true,
		Native:          true,
		HighPerformance: true,
		ObservedAt:      now.Add(-time.Minute),
		ValidUntil:      now.Add(time.Hour),
	}
	plan := Plan{
		NativePath: NativePathRequest{
			ManagementInterface: "mgmt0",
			Assignments: []NativeAssignment{
				{LinuxInterface: "eth1", Explicit: true, Proof: proof},
				{LinuxInterface: "eth2", Explicit: true, Proof: proof},
			},
			AllowUnproven: true,
			Now:           now,
		},
		AddressAssignments: []AddressAssignment{{LinuxInterface: "bond-test", VPPInterface: "bond-test", CIDR: "192.168.90.1/24", Role: "lan"}},
		Bonds:              []BondState{{Name: "bond-test", Mode: "xor", Members: []string{"lyroute-eth1", "lyroute-eth2"}}},
	}
	if _, err := BuildOperations(plan); err != nil {
		t.Fatalf("bond with all member attachments: %v", err)
	}

	// One missing member keeps the bond address locked, but the failure names
	// the bond address assignment, not a synthetic bond capability proof.
	plan.NativePath.Assignments = plan.NativePath.Assignments[:1]
	_, err := BuildOperations(plan)
	var locked *DataplaneLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("missing member error = %v, want DataplaneLockedError", err)
	}
	foundAddressProof := false
	for _, result := range locked.Prerequisites {
		if result.Name == "runtime_capability_proof" && result.Interface == "bond-test" {
			t.Fatalf("bond was checked as an independent interface: %#v", result)
		}
		if result.Name == "address_assignment_proven" && result.Interface == "bond-test" && !result.Passed {
			foundAddressProof = true
		}
	}
	if !foundAddressProof {
		t.Fatalf("prerequisites = %#v, want failed address_assignment_proven for bond-test", locked.Prerequisites)
	}
}
