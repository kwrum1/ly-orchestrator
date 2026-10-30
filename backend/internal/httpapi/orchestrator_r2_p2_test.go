package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	orchestratortelemetry "ly-route/backend/internal/orchestrator/telemetry"
	"ly-route/backend/internal/orchestratorapi"
)

func TestOrchestratorPasswordChangeRequiredMessage(t *testing.T) {
	server, _, _ := orchestratorPackageServer(t)
	session := server.sessions.create("admin", "admin", true)
	request := httptest.NewRequest(http.MethodPut, orchestratorapi.TopologyPath, strings.NewReader(`{}`))
	request.AddCookie(sessionCookie(session.ID, false))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("PUT before password change = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":"password_change_required"`) || !strings.Contains(body, "password change is required") {
		t.Fatalf("PUT before password change message = %s", body)
	}
	if strings.Contains(body, "admin role required") {
		t.Fatalf("PUT before password change reported a role problem: %s", body)
	}
}

func TestOrchestratorTopologyAndPolicyPutsAudit(t *testing.T) {
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	topology, _, err := repository.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	topologyBody, err := json.Marshal(topology.View())
	if err != nil {
		t.Fatal(err)
	}
	if response := authenticatedJSONRequest(t, server, http.MethodPut, orchestratorapi.TopologyPath, string(topologyBody), cookie); response.Code != http.StatusOK {
		t.Fatalf("PUT topology = %d: %s", response.Code, response.Body.String())
	}
	policy, _, err := repository.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := json.Marshal(policy.View())
	if err != nil {
		t.Fatal(err)
	}
	if response := authenticatedJSONRequest(t, server, http.MethodPut, orchestratorapi.PolicyPath, string(policyBody), cookie); response.Code != http.StatusOK {
		t.Fatalf("PUT policy = %d: %s", response.Code, response.Body.String())
	}
	events, err := server.store.AuditEvents(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, event := range events {
		if event.Actor == "admin" && event.Status == "success" && event.Action == "update" {
			found[event.Resource] = true
		}
	}
	if !found[orchestratorapi.TopologyPath] || !found[orchestratorapi.PolicyPath] {
		t.Fatalf("missing topology/policy audit rows: %#v", events)
	}
}

func TestOrchestratorDashboardUsesFreshTransparentRuntimeEvidence(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	server, repository, _ := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: orchestratorapi.TransparentRuntimeEvidence{
		TransactionID: "txn-dashboard", Generation: "gen-dashboard", State: "running", AppliedAt: now, ObservedAt: now,
	}}
	server.orchestratorTelemetry = fixedOrchestratorTelemetry{snapshot: orchestratortelemetry.Snapshot{
		Status: orchestratortelemetry.Status{State: orchestratortelemetry.StateAvailable, Fresh: true, CollectedAt: now, ObservedAt: now},
		Components: orchestratortelemetry.ComponentStatuses{
			Interfaces:  orchestratortelemetry.ComponentStatus{State: orchestratortelemetry.StateAvailable},
			PolicyHits:  orchestratortelemetry.ComponentStatus{State: orchestratortelemetry.StateAvailable},
			Neighbors:   orchestratortelemetry.ComponentStatus{State: orchestratortelemetry.StateAvailable},
			Connections: orchestratortelemetry.ComponentStatus{State: orchestratortelemetry.StateAvailable},
		},
	}}
	server.lastRuntime = &RuntimeApplyResult{Status: "degraded", RuntimeState: "degraded"}
	payload, _ := server.telemetryPayload(context.Background(), "dashboard")
	data, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("dashboard payload = %#v", payload)
	}
	if data["runtime_state"] != "running" || data["degraded"] != false {
		t.Fatalf("dashboard used stale runtime evidence: %#v", data)
	}
}
