package vpp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadNativePathRequestWrapsSingleLegacyProofForSmartQoS(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	proof := candidateProof(now, DataplaneTierNative, NativeHookAFXDP, NativeModeZeroCopy, 100)
	proof.SmartQoSPluginAvailable = true
	report := CapabilityReport{
		ManagementInterface: "eth0",
		Proofs: []InterfaceProof{{
			LinuxInterface: "eth1",
			Proof:          proof,
		}},
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capabilities.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	request := LoadNativePathRequestWithSharedManagement(path, "eth0", []string{"eth1"}, now, false)
	request.RequireSmartQoS = true
	selected, err := SelectNativePath(request)
	if err != nil {
		t.Fatalf("single legacy proof was rejected: %v", err)
	}
	if len(selected.Attachments) != 1 || selected.Attachments[0].LinuxInterface != "eth1" {
		t.Fatalf("selected path = %#v", selected)
	}
}
