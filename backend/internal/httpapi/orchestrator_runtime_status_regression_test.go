package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/runtime/apply"
)

// These tests pin the Orchestrator runtime/status contract that regressed on
// the merged release line: the transparent datapath has its own component
// inventory (vpp + persistence), and that inventory must not oscillate between
// a plan rebuild and the Gateway cached-component reprobe.

type runtimeStatusResponse struct {
	Status     string                  `json:"status"`
	Components []RuntimeComponentState `json:"components"`
}

func orchestratorStatusTestServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	server, repository, cookie := orchestratorPackageServer(t)
	seedOrchestratorPackageState(t, repository)
	return server, cookie
}

func transparentOrchestratorEvidence(now time.Time) orchestratorapi.TransparentRuntimeEvidence {
	return orchestratorapi.TransparentRuntimeEvidence{
		TransactionID: "txn-orch-status",
		Generation:    "fd3afd6b8f0e4d5c",
		State:         "running",
		AppliedAt:     now,
		ObservedAt:    now,
	}
}

func committedOrchestratorApply(now time.Time, components []RuntimeComponentState) RuntimeApplyResult {
	const transactionID = "txn-orch-status"
	return RuntimeApplyResult{
		Status:        RuntimeStatusCommitted,
		RuntimeState:  RuntimeStateRunning,
		TransactionID: transactionID,
		AppliedAt:     now,
		Receipt: apply.ApplyReceipt{
			TransactionID: transactionID,
			Capability:    "vpp.transparent-orchestrator",
			Status:        apply.ReceiptApplied,
			AppliedAt:     now,
		},
		Readback: apply.Readback{
			TransactionID: transactionID,
			Capability:    "vpp.transparent-orchestrator",
			Timestamp:     now,
			Fresh:         true,
		},
		ReconciliationReceipt: apply.ReconciliationReceipt{
			TransactionID: transactionID,
			Capability:    "runtime.reconciliation",
			Status:        apply.ReceiptApplied,
			Timestamp:     now,
		},
		Components: components,
	}
}

func getRuntimeStatus(t *testing.T, server *Server, cookie *http.Cookie) runtimeStatusResponse {
	t.Helper()
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/runtime/status", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("runtime status = %d: %s", response.Code, response.Body.String())
	}
	var body runtimeStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode runtime status: %v: %s", err, response.Body.String())
	}
	return body
}

func runtimeComponentNames(components []RuntimeComponentState) []string {
	names := make([]string, 0, len(components))
	for _, component := range components {
		names = append(names, component.Name)
	}
	return names
}

func requireOrchestratorTransparentInventory(t *testing.T, poll int, body runtimeStatusResponse) {
	t.Helper()
	if body.Status != RuntimeStateRunning {
		t.Fatalf("poll %d: status = %q, want %q; components = %#v", poll, body.Status, RuntimeStateRunning, body.Components)
	}
	if got, want := runtimeComponentNames(body.Components), []string{"vpp", "persistence"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("poll %d: components = %v, want %v (gateway service inventory leaked into the Orchestrator profile)", poll, got, want)
	}
	vpp := body.Components[0]
	if vpp.State != "running" || !vpp.Available {
		t.Fatalf("poll %d: vpp = %#v, want running/available from the committed transparent generation", poll, vpp)
	}
}

// Symptom 1: immediately after a committed Orchestrator apply the response
// listed the Gateway service inventory (smartdns/dnsmasq/xray/pppoe) instead
// of the transparent vpp + persistence pair.
func TestOrchestratorRuntimeStatusUsesTransparentComponentSet(t *testing.T) {
	server, cookie := orchestratorStatusTestServer(t)
	now := server.now().UTC()
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: transparentOrchestratorEvidence(now)}
	// Post-apply state: the receipt is committed but the Orchestrator apply
	// path never cached a Gateway component inventory.
	server.setRuntimeEvidence(committedOrchestratorApply(now, nil))

	requireOrchestratorTransparentInventory(t, 1, getRuntimeStatus(t, server, cookie))
}

// Symptom 2: after a control-api restart the first poll was correct, but the
// second poll reprobed the cached component through the Gateway VPP evidence
// path and degraded with "no VPP operations rendered".
func TestOrchestratorRuntimeStatusStaysRunningAcrossPollsAfterRestart(t *testing.T) {
	server, cookie := orchestratorStatusTestServer(t)
	now := server.now().UTC()
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: transparentOrchestratorEvidence(now)}
	// A restart restores a committed reconciliation receipt without a cached
	// inventory, exactly like the observed first poll after systemctl restart.
	server.setRuntimeEvidence(committedOrchestratorApply(now, nil))

	for poll := 1; poll <= 5; poll++ {
		requireOrchestratorTransparentInventory(t, poll, getRuntimeStatus(t, server, cookie))
	}
}

// The first poll after a restart rebuilds the plan and caches its component
// list. Later polls take the cached path; that path must not require Gateway
// plan operations for the transparent Orchestrator datapath.
func TestOrchestratorRuntimeStatusCachedReprobeStaysRunning(t *testing.T) {
	server, cookie := orchestratorStatusTestServer(t)
	now := server.now().UTC()
	server.orchestratorRuntime = &healthTransparentRuntime{evidence: transparentOrchestratorEvidence(now)}
	server.setRuntimeEvidence(committedOrchestratorApply(now, []RuntimeComponentState{
		{Name: "vpp", State: "running", Available: true, Reason: "transparent VPP generation fd3afd6b8f0e4d5c verified"},
		{Name: "persistence", State: "running", Available: true, Reason: persistenceRuntimeReason(true)},
	}))

	for poll := 1; poll <= 3; poll++ {
		requireOrchestratorTransparentInventory(t, poll, getRuntimeStatus(t, server, cookie))
	}
}

// The Gateway profile keeps its Gateway service inventory; the Orchestrator
// fix must not reroute it through the transparent evidence path.
func TestGatewayRuntimeStatusKeepsGatewayServiceInventory(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	now := server.now().UTC()
	server.setRuntimeEvidence(committedOrchestratorApply(now, nil))

	body := getRuntimeStatus(t, server, cookie)
	names := runtimeComponentNames(body.Components)
	for _, want := range []string{"smartdns", "dnsmasq", "xray", "pppoe"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("gateway components = %v, missing %q", names, want)
		}
	}
	for _, name := range names {
		if name == "vpp" {
			t.Fatalf("gateway components = %v, transparent vpp component leaked into the Gateway profile", names)
		}
	}
}
