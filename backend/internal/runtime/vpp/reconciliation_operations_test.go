package vpp

import (
	"errors"
	"testing"
	"time"
)

// Persisted generations carry capability proofs with a short validity window.
// Reconciliation and committed-evidence validation still need the operation
// graph after that window expires; only an apply must fail closed.
func TestBuildOperationsAllowsUnprovenPathForReconciliation(t *testing.T) {
	expired := time.Now().UTC().Add(-2 * time.Hour)
	plan := Plan{
		RequestID: "txn-expired-proof",
		NativePath: NativePathRequest{
			ManagementInterface: "eth0",
			Assignments: []NativeAssignment{{
				LinuxInterface: "eth1",
				Explicit:       true,
				Proof: CapabilityProof{
					Hook:            NativeHookAFXDP,
					Mode:            NativeModeZeroCopy,
					VPPInterface:    "lyroute-eth1",
					Source:          ProofSourceRuntimeProbe,
					RuntimeVerified: true,
					Native:          true,
					HighPerformance: true,
					ObservedAt:      expired,
					ValidUntil:      expired.Add(time.Minute),
				},
			}},
		},
		AddressAssignments: []AddressAssignment{{ID: "lan", LinuxInterface: "eth1", VPPInterface: "lyroute-eth1", Role: "lan", CIDR: "192.0.2.2/24"}},
	}

	if _, err := BuildOperations(plan); err == nil {
		t.Fatal("apply path accepted an expired dataplane proof")
	} else {
		var locked *DataplaneLockedError
		if !errors.As(err, &locked) {
			t.Fatalf("expired proof error = %v, want dataplane_locked", err)
		}
	}

	plan.AllowUnprovenOperations = true
	operations, err := BuildOperations(plan)
	if err != nil {
		t.Fatalf("reconciliation operations failed: %v", err)
	}
	found := false
	for _, operation := range operations {
		if operation.Name == "vpp.dataplane.attach" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reconciliation operations lack the declarative dataplane attach: %#v", operations)
	}
}
