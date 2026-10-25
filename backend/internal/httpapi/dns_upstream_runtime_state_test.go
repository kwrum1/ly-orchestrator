package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func TestDNSUpstreamRuntimeStateFollowsCommittedServiceNetwork(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-dns-upstream-state?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		},
	}
	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithClock(fixedClock()),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	cookie := login.Result().Cookies()[0]

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/dns/upstreams",
		`{"id":"up-wan","name":"WAN upstream","enabled":true,"servers":["100.77.0.1"],"wan_egress_id":"wan-a"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create upstream = %d %s", created.Code, created.Body.String())
	}

	network, err := vpp.DNSServiceNetworkForUpstreamID("up-wan", "wan-a", []string{"100.77.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	transactionID := "txn-dns-upstream"
	plan := vpp.Plan{DNSServiceNetworks: []vpp.DNSServiceNetwork{network}}
	setDNSUpstreamRuntimeEvidence(server, transactionID, &plan, nil)

	item := getDNSUpstreamItem(t, server, cookie, "up-wan")
	if item["runtime_state"] != "applied" {
		t.Fatalf("WAN DNS upstream runtime_state = %v, want applied (%#v)", item["runtime_state"], item)
	}

	// A resolver change is a new desired generation. The committed evidence
	// still describes the old resolver set, so the item must stay pending.
	patched := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/dns/upstreams/up-wan",
		`{"servers":["100.77.0.9"]}`, cookie)
	if patched.Code != http.StatusOK {
		t.Fatalf("patch upstream = %d %s", patched.Code, patched.Body.String())
	}
	item = getDNSUpstreamItem(t, server, cookie, "up-wan")
	if item["runtime_state"] != "desired_not_applied" {
		t.Fatalf("changed resolver set runtime_state = %v, want desired_not_applied (%#v)", item["runtime_state"], item)
	}
}

func TestDNSUpstreamRuntimeStateUsesDurableEvidenceAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-dns-upstream-restart?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		},
	}
	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithClock(fixedClock()),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	cookie := login.Result().Cookies()[0]
	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/dns/upstreams",
		`{"id":"up-wan","name":"WAN upstream","enabled":true,"servers":["100.77.0.1"],"wan_egress_id":"wan-a"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create upstream = %d %s", created.Code, created.Body.String())
	}

	transactionID := "txn-dns-upstream-restart"
	evidence := []apply.GatewayResourceEvidence{{
		Resource:      "service-networks",
		Capability:    "service-networks",
		TransactionID: transactionID,
		ApplyReceipt:  apply.ApplyReceipt{TransactionID: transactionID, Capability: "service-networks", Status: apply.ReceiptApplied, AppliedAt: fixedClock()()},
		SupplementalReadback: []vpp.SupplementalOperationReadback{{
			Name:     "vpp.dns-service.network",
			Resource: "up-wan",
		}},
	}}
	setDNSUpstreamRuntimeEvidence(server, transactionID, nil, evidence)

	item := getDNSUpstreamItem(t, server, cookie, "up-wan")
	if item["runtime_state"] != "applied" {
		t.Fatalf("restarted WAN DNS upstream runtime_state = %v, want applied (%#v)", item["runtime_state"], item)
	}

	// Evidence from a different transaction must never promote the item.
	setDNSUpstreamRuntimeEvidence(server, "txn-other", nil, evidence)
	item = getDNSUpstreamItem(t, server, cookie, "up-wan")
	if item["runtime_state"] != "desired_not_applied" {
		t.Fatalf("foreign transaction runtime_state = %v, want desired_not_applied", item["runtime_state"])
	}
}

func TestDNSUpstreamResolverCoverage(t *testing.T) {
	if !dnsResolverServersCovered(nil, nil) {
		t.Fatal("empty desired resolver set must be covered")
	}
	if !dnsResolverServersCovered([]string{"1.1.1.1"}, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatal("applied superset must cover desired resolvers")
	}
	if dnsResolverServersCovered([]string{"1.1.1.1"}, []string{"8.8.8.8"}) {
		t.Fatal("missing resolver must not be reported as covered")
	}
}

func setDNSUpstreamRuntimeEvidence(server *Server, transactionID string, plan *vpp.Plan, evidence []apply.GatewayResourceEvidence) {
	now := fixedClock()()
	server.runtimeMu.Lock()
	defer server.runtimeMu.Unlock()
	server.lastRuntime = &RuntimeApplyResult{
		Status:          RuntimeStatusCommitted,
		RuntimeState:    RuntimeStateRunning,
		TransactionID:   transactionID,
		AppliedAt:       now,
		Receipt:         apply.ApplyReceipt{TransactionID: transactionID, Capability: "runtime", Status: apply.ReceiptApplied, AppliedAt: now},
		Readback:        apply.Readback{TransactionID: transactionID, Capability: "runtime", Timestamp: now, Fresh: true},
		GatewayPlan:     plan,
		GatewayEvidence: evidence,
	}
}

func getDNSUpstreamItem(t *testing.T, server *Server, cookie *http.Cookie, id string) map[string]any {
	t.Helper()
	response := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/dns/upstreams/"+id, "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("get upstream = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode upstream readback: %v (%s)", err, response.Body.String())
	}
	if body.Item == nil {
		t.Fatalf("get upstream returned no item: %s", response.Body.String())
	}
	return body.Item
}
