package vpp

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The AF_PACKET consent is part of the apply decision, so it must survive the
// persisted plan. Without it a restart cannot rebuild the committed interface
// operations and every apply reconciles as af_fallback_required.
func TestNativePathRequestPersistsAFPacketAcceptance(t *testing.T) {
	request := NativePathRequest{
		ManagementInterface: "enp0s2",
		AcceptanceAFPacket:  true,
		Assignments:         []NativeAssignment{{LinuxInterface: "enp0s4", Explicit: true}},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded NativePathRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.AcceptanceAFPacket {
		t.Fatalf("AF_PACKET acceptance was not persisted: %s", encoded)
	}
}

func TestBuildOperationsReconcilesPersistedAFOnlyPlan(t *testing.T) {
	now := time.Now().UTC()
	proof := CapabilityProof{
		Tier:            DataplaneTierNative,
		Hook:            NativeHookAFPacket,
		Mode:            NativeModeAFPacket,
		Source:          ProofSourceRuntimeProbe,
		RuntimeVerified: true,
		Native:          true,
		AcceptanceOnly:  true,
		MACAddress:      "52:54:00:aa:bb:cc",
		ObservedAt:      now,
		ValidUntil:      now.Add(5 * time.Minute),
	}
	request := NativePathRequest{
		ManagementInterface: "enp0s2",
		Now:                 now,
		Assignments: []NativeAssignment{{
			LinuxInterface: "enp0s4",
			Explicit:       true,
			Candidates:     []CapabilityProof{proof},
		}},
	}
	if _, err := SelectNativePath(request); err == nil {
		t.Fatal("unconsented AF_PACKET candidate must require consent")
	} else {
		var afRequired *AFFallbackRequiredError
		if !errors.As(err, &afRequired) {
			t.Fatalf("selection error = %v, want AFFallbackRequiredError", err)
		}
	}

	// A persisted pre-consent plan is still reconcilable: it may only be used
	// to derive cleanup/readback operations, never to apply.
	operations, err := BuildOperations(Plan{
		RequestID:               "txn-persisted-af",
		AllowUnprovenOperations: true,
		DataplanePrepared:       true,
		NativePath:              request,
	})
	if err != nil {
		t.Fatalf("persisted AF plan could not be reconciled: %v", err)
	}
	if len(operations) == 0 {
		t.Fatal("persisted AF plan produced no interface operations")
	}
}
