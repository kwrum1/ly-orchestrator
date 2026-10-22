package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/orchestratorapi"
)

type healthTransparentRuntime struct {
	recordingOrchestratorRuntime
	evidence orchestratorapi.TransparentRuntimeEvidence
	err      error
}

func (runtime *healthTransparentRuntime) TransparentRuntimeEvidence(context.Context) (orchestratorapi.TransparentRuntimeEvidence, error) {
	if runtime.err != nil {
		return orchestratorapi.TransparentRuntimeEvidence{}, runtime.err
	}
	return runtime.evidence, nil
}

func TestOrchestratorHealthReportsLockedTransparentRuntime(t *testing.T) {
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	server.orchestratorRuntime = &healthTransparentRuntime{err: errors.New("transparent orchestrator state is locked")}

	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/health", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"degraded":true`) {
		t.Fatalf("health did not degrade for locked runtime: %s", body)
	}
	if !strings.Contains(body, `"name":"orchestrator_runtime"`) || !strings.Contains(body, `"available":false`) || !strings.Contains(body, "state is locked") {
		t.Fatalf("health missing locked orchestrator runtime dependency: %s", body)
	}
}

func TestOrchestratorHealthDoesNotRequireRuntimeWithoutTopology(t *testing.T) {
	server, _, _ := orchestratorPackageServer(t)
	server.orchestratorRuntime = &healthTransparentRuntime{err: errors.New("transparent orchestrator state is locked")}
	if capability := server.orchestratorRuntimeCapability(context.Background()); capability != nil {
		t.Fatalf("empty topology required a transparent runtime: %#v", capability)
	}
}

func TestOrchestratorHealthAcceptsRunningTransparentRuntime(t *testing.T) {
	server, repository, _ := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: orchestratorapi.TransparentRuntimeEvidence{
		TransactionID: "txn-health", Generation: "gen-health", State: "running",
	}}
	capability := server.orchestratorRuntimeCapability(context.Background())
	if capability == nil || !capability.Available || capability.State != "available" {
		t.Fatalf("running transparent runtime capability = %#v", capability)
	}
}

func TestOrchestratorHealthRejectsLockedEvidenceWithoutError(t *testing.T) {
	server, repository, _ := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: orchestratorapi.TransparentRuntimeEvidence{State: "locked"}}
	capability := server.orchestratorRuntimeCapability(context.Background())
	if capability == nil || capability.Available || capability.State != "degraded" || !strings.Contains(capability.Reason, "locked") {
		t.Fatalf("locked evidence capability = %#v", capability)
	}
}
