package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

func newPolicyServer(t *testing.T, name string) (*Server, *http.Cookie) {
	t.Helper()
	store, err := persistence.Open(context.Background(), "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	return server, loginAdmin(t, server)
}

func TestDataplanePolicyRejectsManualTakeoverSwitch(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "switch list", body: `{"af_packet_fallback":false,"ports":[],"takeover_switches":[{"port":"eth9","mode":"dpdk_uio"}]}`},
		{name: "confirmed", body: `{"af_packet_fallback":false,"ports":[],"takeover_switches":[{"port":"eth9","mode":"dpdk_uio","confirmed":true}]}`},
		{name: "top level confirmed", body: `{"af_packet_fallback":false,"ports":[],"confirmed":true}`},
		{name: "takeover mode", body: `{"af_packet_fallback":false,"ports":[],"takeover_mode":"dpdk_uio"}`},
		{name: "per port mode", body: `{"af_packet_fallback":false,"ports":[],"mode":"af_packet"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, cookie := newPolicyServer(t, "httpapi-manual-takeover-"+strings.ReplaceAll(testCase.name, " ", "-"))
			response := authenticatedJSONRequest(t, server, http.MethodPut, "/api/v1/dataplane/policy", testCase.body, cookie)
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), takeoverNotSelectableCode) {
				t.Fatalf("manual takeover = %d %s, want 422 %s", response.Code, response.Body.String(), takeoverNotSelectableCode)
			}
			if policy := server.dataplanePolicy(context.Background()); policy.AFPacketFallback || len(policy.Ports) != 0 {
				t.Fatalf("rejected request was persisted: %#v", policy)
			}
		})
	}
}

func TestDataplanePolicyIsReadOnlyForTakeoverSelection(t *testing.T) {
	server, cookie := newPolicyServer(t, "httpapi-policy-readonly-takeover")
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/dataplane/policy", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("GET policy = %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"takeover_automatic":true`, `"manual_takeover":false`, `"takeover_selection":"backend"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET policy is missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "takeover_switches") {
		t.Fatalf("GET policy still exposes takeover_switches: %s", body)
	}
}

func TestDataplanePolicyStillAcceptsLegacyAuditFields(t *testing.T) {
	server, cookie := newPolicyServer(t, "httpapi-policy-legacy-fields")
	response := authenticatedJSONRequest(t, server, http.MethodPut, "/api/v1/dataplane/policy", `{"af_packet_fallback":true,"ports":["eth2"]}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy policy update = %d %s", response.Code, response.Body.String())
	}
	policy := server.dataplanePolicy(context.Background())
	if !policy.AFPacketFallback || len(policy.Ports) != 1 {
		t.Fatalf("persisted policy = %#v", policy)
	}
}

func TestLegacyStoredTakeoverSwitchesCannotSelectAMode(t *testing.T) {
	server, _ := newPolicyServer(t, "httpapi-policy-legacy-document")
	ctx := context.Background()
	// Simulate a document written by the previous build: it contains a switch
	// list. Reading it must succeed, but the switch must not surface or be
	// applied.
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: dataplanePolicyResource,
		ResourceID:   dataplanePolicyID,
		Payload:      []byte(`{"af_packet_fallback":true,"ports":[],"takeover_switches":[{"port":"eth9","mode":"dpdk_uio","confirmed":true}]}`),
		PayloadHash:  "legacy",
	}); err != nil {
		t.Fatal(err)
	}
	policy := server.dataplanePolicy(ctx)
	if !policy.AFPacketFallback {
		t.Fatalf("legacy document was not read: %#v", policy)
	}
	// The switch in the document is not part of the model anymore: there is no
	// field to read it back into, and no automatic upgrade can be driven by it.
	plan := server.takeoverPlanForPort(ctx, "eth9")
	if plan.RuleID != "" || len(plan.Candidates) != 0 {
		t.Fatalf("legacy switch produced a plan for a non-physical port: %#v", plan)
	}
}
