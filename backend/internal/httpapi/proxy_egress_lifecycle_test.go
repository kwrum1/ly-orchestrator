package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/proxy"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// TestProxyEgressDeleteLeavesApplyCommittable is the F2 regression: deleting
// the last proxy egress leaves a soft-deleted tombstone in the config store,
// but the next apply must still compile a valid no-proxy generation, commit,
// and remove the previous steering. Re-creating an egress must also commit.
func TestProxyEgressDeleteLeavesApplyCommittable(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-proxy-delete-datapath?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "eth0")
	saveGatewayApplyInterface(t, store)
	useVPPProof(t, "eth0")
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.SmartDNS: {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:  {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Xray:     {Service: serviceRuntime.Xray, Available: true},
		serviceRuntime.PPPoE:    {Service: serviceRuntime.PPPoE, Available: true},
		serviceRuntime.VPP:      {Service: serviceRuntime.VPP, Available: true},
	}}
	transaction := &capturingGatewayTransaction{}
	server := New(
		WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
		WithGatewayTransaction(transaction),
		WithClock(fixedClock()),
	)
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]

	// Delete the built-in default. There is no stored row, so the API records a
	// tombstone to remember that the operator removed the default egress.
	deleted := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/proxy/egresses/proxy-egress-default", ``, cookie)
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete default status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	apply := func(step string) string {
		t.Helper()
		response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/runtime/apply", `{}`, cookie)
		if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
			t.Fatalf("%s apply status=%d body=%s", step, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), `"status":"apply_failed"`) || strings.Contains(response.Body.String(), "invalid proxy egress") {
			t.Fatalf("%s apply failed: status=%d body=%s", step, response.Code, response.Body.String())
		}
		if transaction.plan == nil {
			t.Fatalf("%s apply did not reach the gateway transaction", step)
		}
		return response.Body.String()
	}

	// The tombstone must not resurrect the hidden default egress in the apply
	// snapshot; the no-proxy generation has no steering.
	apply("after tombstone")
	if transaction.plan.Request.ProxyEgress.ID != "" || len(transaction.plan.CompiledProxy.VPPSteering) != 0 || len(transaction.plan.GatewayPlan.Proxy.VPPSteering) != 0 {
		t.Fatalf("tombstone compiled into apply: egress=%#v compiled=%#v gateway=%#v",
			transaction.plan.Request.ProxyEgress, transaction.plan.CompiledProxy.VPPSteering, transaction.plan.GatewayPlan.Proxy.VPPSteering)
	}

	// Build a real proxy generation.
	underlay := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		`{"id":"wan-underlay","name":"Underlay","enabled":true,"type":"static"}`, cookie)
	if underlay.Code != http.StatusOK {
		t.Fatalf("underlay status=%d body=%s", underlay.Code, underlay.Body.String())
	}
	saveTestProxyNode(t, store)
	egress := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/proxy/egresses",
		`{"id":"proxy-media","kind":"egress","name":"Media Proxy","enabled":true,"semantic_type":"proxy_egress","display_list":"wan","proxy_profile_id":"xray-tproxy-outbound","underlay_wan_id":"wan-underlay","node_id":"proxy-node-test"}`, cookie)
	if egress.Code != http.StatusOK {
		t.Fatalf("egress status=%d body=%s", egress.Code, egress.Body.String())
	}
	apply("with proxy")
	if transaction.plan.Request.ProxyEgress.ID != "proxy-media" || len(transaction.plan.GatewayPlan.Proxy.VPPSteering) == 0 {
		t.Fatalf("proxy generation was not compiled: egress=%#v steering=%#v",
			transaction.plan.Request.ProxyEgress, transaction.plan.GatewayPlan.Proxy.VPPSteering)
	}
	// Deleting the only active egress leaves the default tombstone behind. The
	// next apply must commit with no proxy and the transaction must no longer
	// contain a steering/service-network generation.
	removed := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/proxy/egresses/proxy-media", ``, cookie)
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"deleted":true`) {
		t.Fatalf("delete egress status=%d body=%s", removed.Code, removed.Body.String())
	}
	apply("after deleting last egress")
	if transaction.plan.Request.ProxyEgress.ID != "" || len(transaction.plan.CompiledProxy.VPPSteering) != 0 || len(transaction.plan.GatewayPlan.Proxy.VPPSteering) != 0 {
		t.Fatalf("deleted egress still compiled: egress=%#v compiled=%#v gateway=%#v",
			transaction.plan.Request.ProxyEgress, transaction.plan.CompiledProxy.VPPSteering, transaction.plan.GatewayPlan.Proxy.VPPSteering)
	}

	// Creating a new egress must become committable again even with the default
	// tombstone still in the store.
	recreated := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/proxy/egresses",
		`{"id":"proxy-media-2","kind":"egress","name":"Media Proxy 2","enabled":true,"semantic_type":"proxy_egress","display_list":"wan","proxy_profile_id":"xray-tproxy-outbound","underlay_wan_id":"wan-underlay","node_id":"proxy-node-test"}`, cookie)
	if recreated.Code != http.StatusOK {
		t.Fatalf("recreate status=%d body=%s", recreated.Code, recreated.Body.String())
	}
	apply("after recreate")
	if transaction.plan.Request.ProxyEgress.ID != "proxy-media-2" || len(transaction.plan.GatewayPlan.Proxy.VPPSteering) == 0 {
		t.Fatalf("recreated egress was not compiled: egress=%#v steering=%#v",
			transaction.plan.Request.ProxyEgress, transaction.plan.GatewayPlan.Proxy.VPPSteering)
	}
}

func TestProxyRuntimeRemovedOnlyOnSteeringTransition(t *testing.T) {
	previous := vpp.Plan{Proxy: proxy.CompiledEgress{VPPSteering: []proxy.VPPSteeringInstruction{{EgressID: "old"}}}}
	if !proxyRuntimeRemoved(apply.Plan{
		Previous:    apply.PreviousState{Available: true, GatewayPlan: &previous},
		GatewayPlan: vpp.Plan{},
	}) {
		t.Fatal("steering -> no proxy transition was not recognized")
	}
	if proxyRuntimeRemoved(apply.Plan{
		Previous:    apply.PreviousState{Available: true, GatewayPlan: &previous},
		GatewayPlan: previous,
	}) {
		t.Fatal("unchanged proxy generation was treated as removed")
	}
	if proxyRuntimeRemoved(apply.Plan{GatewayPlan: vpp.Plan{}}) {
		t.Fatal("first generation was treated as proxy removal")
	}
}
func TestHealthDoesNotDegradeXrayWithoutProxyEgress(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-proxy-health-no-egress?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controller := &httpServiceController{health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
		serviceRuntime.VPP:          {Service: serviceRuntime.VPP, Available: true},
		serviceRuntime.SmartDNS:     {Service: serviceRuntime.SmartDNS, Available: true},
		serviceRuntime.Dnsmasq:      {Service: serviceRuntime.Dnsmasq, Available: true},
		serviceRuntime.Nftables:     {Service: serviceRuntime.Nftables, Available: true},
		serviceRuntime.LinuxRouting: {Service: serviceRuntime.LinuxRouting, Available: true},
		serviceRuntime.Xray:         {Service: serviceRuntime.Xray, Available: false, Reason: "inactive"},
	}}
	server := New(WithStore(store), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}), WithClock(fixedClock()))
	response := request(t, server, http.MethodGet, "/api/v1/health")
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"degraded":false`) {
		t.Fatalf("no-proxy health degraded: %s", body)
	}
	if !strings.Contains(body, `"name":"xray","available":false,"state":"available"`) {
		t.Fatalf("xray was not published as optional: %s", body)
	}
}
