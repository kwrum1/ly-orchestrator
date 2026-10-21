package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/product"
)

func TestOrchestratorOperationProgressEndpoint(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	idle := authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.OperationsPath, "", cookie)
	if idle.Code != http.StatusOK || !strings.Contains(idle.Body.String(), `"state":"idle"`) {
		t.Fatalf("idle operation status=%d body=%s", idle.Code, idle.Body.String())
	}
	server.BeginOperation("transparent-apply", "orchestrator topology/policy")
	running := authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.OperationsPath, "", cookie)
	if running.Code != http.StatusOK || !strings.Contains(running.Body.String(), `"state":"running"`) || !strings.Contains(running.Body.String(), "transparent-apply") {
		t.Fatalf("running operation status=%d body=%s", running.Code, running.Body.String())
	}
	server.EndOperation()
	complete := authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.OperationsPath, "", cookie)
	if complete.Code != http.StatusOK || !strings.Contains(complete.Body.String(), `"state":"complete"`) {
		t.Fatalf("complete operation status=%d body=%s", complete.Code, complete.Body.String())
	}
}

func TestOrchestratorBadPackageReturnsDedicatedCode(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	server.orchestratorUpgradeDir = t.TempDir()
	pkg := testUpgradePackage(t, product.Orchestrator().ID(), true)
	response := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": pkg}, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad package status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "invalid_upgrade_package") {
		t.Fatalf("bad package code = %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "signal: killed") {
		t.Fatalf("bad package leaked process signal: %s", response.Body.String())
	}
	if got := sanitizeUpgradeError(errors.New("zstd: signal: killed")); strings.Contains(got, "signal: killed") {
		t.Fatalf("sanitized error = %q", got)
	}
}

func TestConfigImportAcceptsUnifiedConfirmationAliases(t *testing.T) {
	_, _, _ = productTestServer(t, product.Gateway())
	payload := ConfigPackagePayload{
		SchemaVersion: ConfigPackageSchemaVersion, ContentType: configContentType,
		Product: product.Gateway().ID(), DeviceMode: "gateway",
		Resources: map[string][]json.RawMessage{}, Excluded: []string{"rendered_config"},
	}
	body := map[string]any{
		"confirm": true, "confirmation_token": "token", "confirmation_actor": "admin",
		"confirmation_expires_at":   "2026-07-19T03:15:00Z",
		"confirmation_package_hash": "sha256:package", "confirmation_diff_hash": "sha256:diff",
		"package_manifest": manifestForPayload(payload), "payload": payload,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", strings.NewReader(string(raw)))
	var decoded ConfigImportRequest
	if err := decodeStrictJSON(request, &decoded); err != nil {
		t.Fatalf("decode aliases: %v", err)
	}
	if decoded.ConfirmationPackageHash != "sha256:package" || decoded.ConfirmationDiffHash != "sha256:diff" {
		t.Fatalf("decoded confirmation = %#v", decoded)
	}
	// Old names remain accepted for existing clients.
	raw = []byte(strings.ReplaceAll(string(raw), "confirmation_package_hash", "package_hash"))
	raw = []byte(strings.ReplaceAll(string(raw), "confirmation_diff_hash", "diff_hash"))
	request = httptest.NewRequest(http.MethodPost, "/api/v1/config/import", strings.NewReader(string(raw)))
	var legacy ConfigImportRequest
	if err := decodeStrictJSON(request, &legacy); err != nil || legacy.ConfirmationPackageHash != "sha256:package" {
		t.Fatalf("legacy confirmation = %#v err=%v", legacy, err)
	}
}

func TestSnapshotGetReturnsPayload(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/config/snapshots", `{"name":"readable"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create snapshot status=%d body=%s", created.Code, created.Body.String())
	}
	var body struct {
		Snapshot struct {
			ID string `json:"id"`
		} `json:"snapshot"`
	}
	decode(t, created, &body)
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/snapshots/"+body.Snapshot.ID, "", cookie)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"payload"`) || !strings.Contains(response.Body.String(), `"resources"`) {
		t.Fatalf("snapshot GET status=%d body=%s", response.Code, response.Body.String())
	}
	missing := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/snapshots/missing", "", cookie)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "snapshot_not_found") {
		t.Fatalf("missing snapshot status=%d body=%s", missing.Code, missing.Body.String())
	}
}
