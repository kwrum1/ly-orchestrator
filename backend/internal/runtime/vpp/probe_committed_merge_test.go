package vpp

import (
	"os"
	"path/filepath"
	"testing"
)

const committedStateFixture = `{
  "path": {
    "tier": "vpp_native",
    "attachments": [
      {"linux_interface": "ens161", "vpp_interface": "lyroute-ens161", "tier": "vpp_native", "hook": "af_packet", "mode": "linux_packet_socket", "mac_address": "00:50:56:3a:15:21"},
      {"linux_interface": "ens192", "vpp_interface": "lyroute-ens192", "tier": "vpp_native", "hook": "af_packet", "mode": "linux_packet_socket", "mac_address": "00:50:56:3a:15:22"}
    ]
  }
}`

func writeCommittedMergeState(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "active.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A WAN gateway edit only changes addresses and next hops. When a live probe
// cycle does not cover the WAN port, the committed attachment must be reused
// instead of turning the change into dataplane_locked.
func TestAugmentObservationWithCommittedProofsFillsMissingPorts(t *testing.T) {
	path := writeCommittedMergeState(t, committedStateFixture)
	observation := Observation{
		Report: CapabilityReport{Proofs: []InterfaceProof{{
			LinuxInterface: "ens192",
			Proof:          CapabilityProof{Hook: NativeHookAFPacket, Mode: NativeModeAFPacket, AcceptanceOnly: true},
		}}},
		Reason: "",
	}
	merged := AugmentObservationWithCommittedProofs(observation, path, []string{"ens192", "ens161"})
	if len(merged.Report.Proofs) != 2 {
		t.Fatalf("proofs = %#v", merged.Report.Proofs)
	}
	found := false
	for _, proof := range merged.Report.Proofs {
		if proof.LinuxInterface == "ens161" && proof.Proof.VPPInterface == "lyroute-ens161" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ens161 proof was not recovered from the committed generation: %#v", merged.Report.Proofs)
	}
	if merged.Reason == "" {
		t.Fatal("merge was not reported in the observation reason")
	}
}

// A live proof is authoritative. The committed generation must never hide a
// port whose live capability changed.
func TestAugmentObservationWithCommittedProofsKeepsLiveProof(t *testing.T) {
	path := writeCommittedMergeState(t, committedStateFixture)
	live := CapabilityProof{Hook: NativeHookAFXDP, Mode: NativeModeZeroCopy, PerformanceScore: 100}
	observation := Observation{Report: CapabilityReport{Proofs: []InterfaceProof{{
		LinuxInterface: "ens161",
		Proof:          live,
	}}}}
	merged := AugmentObservationWithCommittedProofs(observation, path, []string{"ens161"})
	if len(merged.Report.Proofs) != 1 || merged.Report.Proofs[0].Proof.Hook != NativeHookAFXDP {
		t.Fatalf("live proof was overridden: %#v", merged.Report.Proofs)
	}
}

func TestAugmentObservationWithCommittedProofsWithoutStateIsNoop(t *testing.T) {
	observation := Observation{Report: CapabilityReport{Proofs: []InterfaceProof{{LinuxInterface: "ens192"}}}}
	merged := AugmentObservationWithCommittedProofs(observation, filepath.Join(t.TempDir(), "absent.json"), []string{"ens192", "ens161"})
	if len(merged.Report.Proofs) != 1 {
		t.Fatalf("proofs = %#v", merged.Report.Proofs)
	}
}
