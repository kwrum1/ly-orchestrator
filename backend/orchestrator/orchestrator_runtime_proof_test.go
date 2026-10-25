package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshRuntimeProofRunsProbeWithCandidateInterfaces(t *testing.T) {
	dir := t.TempDir()
	proofPath := filepath.Join(dir, "proof.json")
	scriptPath := filepath.Join(dir, "probe.sh")
	script := "#!/bin/sh\nprintf '%s' \"$LY_ROUTE_VPP_DATA_INTERFACES|$LY_ROUTE_MANAGEMENT_INTERFACE|$LY_ROUTE_MANAGEMENT_SHARED\" > \"$LY_ROUTE_VPP_CAPABILITY_PROOF\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := &productionOrchestratorRuntime{
		proofCommand:        scriptPath,
		managementInterface: "eth0",
		capabilityReport:    proofPath,
	}
	if err := runtime.refreshRuntimeProof(context.Background(), []string{"eth1", "eth2"}, true); err != nil {
		t.Fatalf("refresh proof: %v", err)
	}
	body, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "eth1,eth2|eth0|true" {
		t.Fatalf("proof probe env = %q", body)
	}
}

func TestRefreshRuntimeProofSkipsMissingProbeAndEmptyInterfaces(t *testing.T) {
	runtime := &productionOrchestratorRuntime{
		proofCommand:     filepath.Join(t.TempDir(), "missing"),
		capabilityReport: filepath.Join(t.TempDir(), "proof.json"),
	}
	if err := runtime.refreshRuntimeProof(context.Background(), []string{"eth1"}, false); err != nil {
		t.Fatalf("missing probe must be a no-op for fixtures: %v", err)
	}
	if err := runtime.refreshRuntimeProof(context.Background(), nil, false); err != nil {
		t.Fatalf("empty interfaces must be a no-op: %v", err)
	}
}
