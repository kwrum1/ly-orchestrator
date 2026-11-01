package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
)

// Export must describe the operator's desired documents, not the product
// defaults: importing defaults (management network, built-in ACL/IP objects)
// as desired state changes semantics and can lock the dataplane.
func TestConfigExportOnlyIncludesStoredDesiredDocuments(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-config-export-stored-only?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	payload, hash, err := persistence.MarshalPayload(map[string]any{"id": "eth5", "name": "eth5", "mirror": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "interface", ResourceID: "eth5",
		Payload: payload, PayloadHash: hash, UpdatedAt: time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithClock(fixedClock()))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", login.Code, login.Body.String())
	}
	exported := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/config/export", "", login.Result().Cookies()[0])
	if exported.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200: %s", exported.Code, exported.Body.String())
	}
	var body struct {
		Payload ConfigPackagePayload `json:"payload"`
	}
	if err := json.Unmarshal(exported.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	resources := body.Payload.Resources
	if len(resources["management_network"]) != 0 {
		t.Fatalf("export leaked the management-network default: %s", resources["management_network"])
	}
	if len(resources["security_acl"]) != 0 {
		t.Fatalf("export leaked the default security ACL: %s", resources["security_acl"])
	}
	if len(resources["interface"]) != 1 {
		t.Fatalf("export interface items = %d, want the single stored document", len(resources["interface"]))
	}
}
