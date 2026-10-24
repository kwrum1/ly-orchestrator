package httpapi

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestProductionGatewayCommitsWithoutRetiredSmartQoS is the arch-v1 acceptance
// gate for requirement 1 and 5: a capability report that never mentions the
// smart-QoS plugin still commits a forwarding generation, and the VPP command
// trace contains no smart-QoS mutation. The same transaction still applies the
// ordinary traffic-control QoS objects, so retirement did not remove flow QoS.
func TestProductionGatewayCommitsWithoutRetiredSmartQoS(t *testing.T) {
	proofPath := os.Getenv("LY_ROUTE_VPP_CAPABILITY_PROOF")
	report, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), "smart_qos") {
		t.Fatalf("arch-v1 fixture must not advertise smart QoS: %s", report)
	}

	proof := newProductionVPPProof(t, false)
	response := proof.request(t, http.MethodPost, "/api/v1/config/apply", `{}`, proof.cookie)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"committed"`) {
		t.Fatalf("apply without smart QoS = %d %s", response.StatusCode, body)
	}

	traceBytes, err := os.ReadFile(proof.trace)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(traceBytes)
	if !strings.Contains(trace, "set interface state") {
		t.Fatalf("dataplane was never attached:\n%s", trace)
	}
	if strings.Contains(trace, "smart-qos") {
		t.Fatalf("retired smart-QoS command reached VPP:\n%s", trace)
	}
	// Flow QoS remains owned by the gateway: the production proof intent applies
	// a DSCP remark, which must still reach the dataplane.
	if !strings.Contains(trace, "qos") {
		t.Fatalf("ordinary traffic-control QoS was not applied:\n%s", trace)
	}

	snapshots, err := proof.store.RuntimeSnapshots(context.Background())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("committed snapshots = %d, error = %v", len(snapshots), err)
	}
	payload := decodeProofSnapshot(t, snapshots[0])
	for _, evidence := range payload.GatewayEvidence {
		if evidence.Resource == "qos" && evidence.Deleted {
			t.Fatalf("QoS evidence unexpectedly deleted: %#v", evidence)
		}
	}
}

// TestProductionGatewayFailedSecondGenerationRollsBackAndVPPStaysUsable pins
// requirement 5's rollback contract: a failed apply preserves the committed
// generation, leaves the fake VPP healthy, and a following apply still commits.
// A rollback that tore down shared state would fail the second apply.
func TestProductionGatewayFailedSecondGenerationRollsBackAndVPPStaysUsable(t *testing.T) {
	proof := newProductionVPPProof(t, true)
	t.Setenv("FAKE_VPPCTL_FAIL", "qos egress map id")

	response := proof.request(t, http.MethodPost, "/api/v1/config/apply", `{}`, proof.cookie)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted || strings.Contains(string(body), `"status":"committed"`) {
		t.Fatalf("failed second generation = %d %s", response.StatusCode, body)
	}
	snapshots, err := proof.store.RuntimeSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("failed apply persisted %d snapshots, want only the prior generation", len(snapshots))
	}

	// The rollback must not have destroyed the dataplane: clear the injected
	// failure and commit the same desired generation.
	t.Setenv("FAKE_VPPCTL_FAIL", "")
	recovered := proof.request(t, http.MethodPost, "/api/v1/config/apply", `{}`, proof.cookie)
	recoveredBody, err := io.ReadAll(recovered.Body)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.StatusCode != http.StatusOK || !strings.Contains(string(recoveredBody), `"status":"committed"`) {
		t.Fatalf("apply after rollback = %d %s", recovered.StatusCode, recoveredBody)
	}
	traceBytes, err := os.ReadFile(proof.trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(traceBytes), "smart-qos") {
		t.Fatalf("retired smart-QoS command reached VPP during rollback recovery:\n%s", traceBytes)
	}
}
