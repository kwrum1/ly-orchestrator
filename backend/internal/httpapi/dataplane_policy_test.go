package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

func TestDataplanePolicyPersistsAFConsent(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-dataplane-policy-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}))
	cookie := loginAdmin(t, server)

	initial := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/dataplane/policy", "", cookie)
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"af_packet_fallback":false`) || !strings.Contains(initial.Body.String(), `"ports":[]`) {
		t.Fatalf("initial policy = %d %s, want disabled with empty ports", initial.Code, initial.Body.String())
	}

	enabled := authenticatedJSONRequest(t, server, http.MethodPut, "/api/v1/dataplane/policy", `{"af_packet_fallback":true,"ports":["eth2","eth5"]}`, cookie)
	if enabled.Code != http.StatusOK || !strings.Contains(enabled.Body.String(), `"af_packet_fallback":true`) || !strings.Contains(enabled.Body.String(), `"eth2"`) {
		t.Fatalf("enable policy = %d %s", enabled.Code, enabled.Body.String())
	}
	if policy := server.dataplanePolicy(ctx); !policy.AFPacketFallback || len(policy.Ports) != 2 {
		t.Fatalf("persisted policy = %#v", policy)
	}

	disabled := authenticatedJSONRequest(t, server, http.MethodPut, "/api/v1/dataplane/policy", `{"af_packet_fallback":false,"ports":[]}`, cookie)
	if disabled.Code != http.StatusOK || !strings.Contains(disabled.Body.String(), `"af_packet_fallback":false`) {
		t.Fatalf("disable policy = %d %s", disabled.Code, disabled.Body.String())
	}
	if policy := server.dataplanePolicy(ctx); policy.AFPacketFallback {
		t.Fatal("persisted policy is still enabled")
	}
}

func TestDataplanePolicyPortScoping(t *testing.T) {
	policy := DataplanePolicy{AFPacketFallback: true, Ports: []string{"eth2"}}
	if !policyAllowsAFPort(policy, "eth2") {
		t.Fatal("listed port was not allowed")
	}
	if policyAllowsAFPort(policy, "eth3") {
		t.Fatal("unlisted port was allowed")
	}
	policy.Ports = nil
	if !policyAllowsAFPort(policy, "eth3") {
		t.Fatal("empty port list must mean every port that needs AF")
	}
	policy.AFPacketFallback = false
	if policyAllowsAFPort(policy, "eth2") {
		t.Fatal("disabled policy allowed AF")
	}
}

func TestDataplanePolicyParticipatesInRuntimePlanHash(t *testing.T) {
	disabled := RuntimePlan{DataplanePolicy: DataplanePolicy{AFPacketFallback: false, Ports: []string{}}}
	enabled := RuntimePlan{DataplanePolicy: DataplanePolicy{AFPacketFallback: true, Ports: []string{"eth2"}}}
	if runtimePlanHash(disabled) == runtimePlanHash(enabled) {
		t.Fatal("AF consent does not change the runtime plan hash")
	}
}
