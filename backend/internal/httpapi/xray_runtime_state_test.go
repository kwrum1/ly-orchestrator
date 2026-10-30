package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/proxy"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func TestXrayStatusReadsLiveFastestSubscriptionSelection(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:xray-live-selection?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "proxy_subscription", "main", map[string]any{"id": "main", "enabled": true, "selection": "fastest", "node_refs": []string{"a", "b"}}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	controller := &httpServiceController{
		health:     map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayStates: []serviceRuntime.XrayBalancerState{{Tag: "subscription-main-fastest", SelectedOutboundTags: []string{"subscription-main-node-b"}}},
	}
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/proxy/xray/status", "", login.Result().Cookies()[0])
	for _, required := range []string{`"available":true`, `"state":"available"`, `"subscription_id":"main"`, `"selected_node_ids":["b"]`, `"live_verified":true`} {
		if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), required) {
			t.Fatalf("Xray live status missing %q: %d %s", required, status.Code, status.Body.String())
		}
	}
}

func TestXrayStatusDegradesWhenBalancerHasNoLiveSelection(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:xray-missing-selection?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "proxy_subscription", "main", map[string]any{"id": "main", "enabled": true, "selection": "fastest", "node_refs": []string{"a"}}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	controller := &httpServiceController{
		health:       map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayStateErr: errors.New("Xray balancer main has no healthy selected outbound"),
	}
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/proxy/xray/status", "", login.Result().Cookies()[0])
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"available":false`) || !strings.Contains(status.Body.String(), `"state":"degraded"`) || !strings.Contains(status.Body.String(), "no healthy selected outbound") {
		t.Fatalf("Xray missing selection did not degrade explicitly: %d %s", status.Code, status.Body.String())
	}
}

func TestXrayStatusReportsAdaptiveFiveTuplePoolWithoutBalancerAPI(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:xray-adaptive-pool?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveConfig(ctx, configDocument(t, "proxy_subscription", "main", map[string]any{
		"id": "main", "enabled": true, "selection": "adaptive", "strategy": "adaptive_topn_weighted", "top_n": 2, "node_refs": []string{"a", "b", "c"},
	}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "subscription-main-node-a", Alive: true, DelayMilliseconds: 10},
			{OutboundTag: "subscription-main-node-b", Alive: true, DelayMilliseconds: 20},
			{OutboundTag: "subscription-main-node-c", Alive: true, DelayMilliseconds: 100},
		},
	}
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	selections, err := server.xrayRuntimeSelections(ctx)
	if err != nil || len(selections) != 1 {
		t.Fatalf("adaptive selections = %#v, err=%v", selections, err)
	}
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/proxy/xray/status", "", login.Result().Cookies()[0])
	for _, required := range []string{
		`"available":true`, `"algorithm":"rtt_weighted_five_tuple"`,
		`"candidate_node_ids":["a","b","c"]`, `"selected_node_ids":["a","b"]`,
		`"node_id":"a","rtt_ms":10,"weight":67`, `"node_id":"b","rtt_ms":20,"weight":33`,
		`"live_verified":true`,
	} {
		if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), required) {
			t.Fatalf("adaptive Xray status missing %q: %d %s", required, status.Code, status.Body.String())
		}
	}
}

func xrayGroupStatusTestServer(t *testing.T, controller *httpServiceController) (*Server, context.Context) {
	t.Helper()
	return xrayGroupStatusTestServerFor(t, controller, "loadbalance", "")
}

func xrayGroupStatusTestServerFor(t *testing.T, controller *httpServiceController, strategy, probeURL string) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := "file:xray-group-status-" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared"
	store, err := persistence.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	group := map[string]any{
		"id": "grp-main", "name": "Main", "enabled": true, "strategy": strategy, "members": []string{"node-kurumi", "node-kurumi-2"},
	}
	if probeURL != "" {
		group["probe_url"] = probeURL
	}
	if err := store.SaveConfig(ctx, configDocument(t, "proxy_group", "grp-main", group, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, configDocument(t, "proxy_egress", "egress-proxy", map[string]any{
		"id": "egress-proxy", "name": "Proxy", "enabled": true, "group_id": "grp-main",
	}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}))
	return server, ctx
}

func xrayGroupStatusPayload(t *testing.T, server *Server) string {
	t.Helper()
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/proxy/xray/status", "", login.Result().Cookies()[0])
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	return status.Body.String()
}

// A group egress (the normal latency/loadbalance configuration) must expose
// the balancer and per-node observatory latency. Before this regression the
// response had "balancers": [] and no node samples even while traffic flowed.
func TestXrayStatusReportsGroupBalancerAndNodeLatency(t *testing.T) {
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: true, DelayMilliseconds: 975, LastTryTime: time.Unix(1793349125, 0)},
			{OutboundTag: "node-node-kurumi-2", Alive: true, DelayMilliseconds: 1720, LastTryTime: time.Unix(1793349126, 0)},
		},
		xrayStates: []serviceRuntime.XrayBalancerState{{Tag: "group-grp-main", SelectedOutboundTags: []string{"node-node-kurumi"}}},
	}
	server, _ := xrayGroupStatusTestServer(t, controller)
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{
		`"available":true`,
		`"balancer_tag":"group-grp-main"`,
		`"group_id":"grp-main"`,
		`"strategy":"loadbalance"`,
		`"selector":["node-node-kurumi","node-node-kurumi-2"]`,
		`"node_id":"node-kurumi"`,
		`"outbound_tag":"node-node-kurumi"`,
		`"delay_ms":975`,
		`"node_id":"node-kurumi-2"`,
		`"delay_ms":1720`,
		`"selected_node_ids":["node-kurumi"]`,
		`"live_verified":true`,
		`"all_nodes_down":false`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("group Xray status missing %q: %s", required, body)
		}
	}
	if strings.Contains(body, `"failure_error"`) {
		t.Fatalf("healthy group status still reports failure_error: %s", body)
	}
}

func TestXrayStatusMarksAllGroupNodesDown(t *testing.T) {
	t.Setenv(proxy.ProxyProbeURLEnv, "http://127.0.0.1:1/unreachable")
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: false, DelayMilliseconds: 0},
			{OutboundTag: "node-node-kurumi-2", Alive: false, DelayMilliseconds: 0},
		},
		xrayStateErr: errors.New("Xray balancer group-grp-main has no healthy selected outbound"),
	}
	server, _ := xrayGroupStatusTestServer(t, controller)
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{`"all_nodes_down":true`, `"dead_node_ids":["node-node-kurumi","node-node-kurumi-2"]`, `"selection_error":"Xray balancer group-grp-main has no healthy selected outbound"`, `"available":true`} {
		if !strings.Contains(body, required) {
			t.Fatalf("all-down Xray status missing %q: %s", required, body)
		}
	}
}

// leastPing is selected inside Xray. When the routing API cannot be read, the
// status must still name the node the observatory would choose instead of
// returning null selected_node_ids/live_verified.
func TestXrayStatusLatencyFallbackShowsSelectedNode(t *testing.T) {
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: true, DelayMilliseconds: 30},
			{OutboundTag: "node-node-kurumi-2", Alive: true, DelayMilliseconds: 10},
		},
		xrayStateErr: errors.New("routing api unavailable"),
	}
	server, _ := xrayGroupStatusTestServerFor(t, controller, "latency", "")
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{
		`"strategy":"latency"`,
		`"selected_node_ids":["node-kurumi-2"]`,
		`"selected_outbound_tags":["node-node-kurumi-2"]`,
		`"live_verified":false`,
		`"selection_source":"observatory"`,
		`"selection_error":"routing api unavailable"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("latency fallback status missing %q: %s", required, body)
		}
	}
}

// A blocked probe URL makes every node look dead in Xray. Status must expose
// the probe target and label the failure separately from an unreachable node.
func TestXrayStatusDistinguishesProbeTargetFromNodeFailure(t *testing.T) {
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: false, DelayMilliseconds: 99999999},
			{OutboundTag: "node-node-kurumi-2", Alive: false, DelayMilliseconds: 99999999},
		},
		xrayStateErr: errors.New("Xray balancer group-grp-main has no healthy selected outbound"),
	}
	server, _ := xrayGroupStatusTestServerFor(t, controller, "latency", "http://127.0.0.1:1/unreachable")
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{
		`"probe_url":"http://127.0.0.1:1/unreachable"`,
		`"probe_target_reachable":false`,
		`"probe_error"`,
		`"health_reason":"probe_target_unreachable"`,
		`"selected_node_ids":[]`,
		`"live_verified":false`,
		`"all_nodes_down":true`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("probe-target status missing %q: %s", required, body)
		}
	}
}

// A successful probe sample must revive the node without restarting Xray or
// persisting the dead verdict in status.
func TestXrayStatusNodeRecoversAfterDeadSample(t *testing.T) {
	t.Setenv(proxy.ProxyProbeURLEnv, "http://127.0.0.1:1/unreachable")
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: false, DelayMilliseconds: 99999999},
			{OutboundTag: "node-node-kurumi-2", Alive: false, DelayMilliseconds: 99999999},
		},
		xrayStateErr: errors.New("no healthy selected outbound"),
	}
	server, _ := xrayGroupStatusTestServerFor(t, controller, "latency", "")
	if body := xrayGroupStatusPayload(t, server); !strings.Contains(body, `"all_nodes_down":true`) {
		t.Fatalf("dead sample not reported: %s", body)
	}
	controller.xrayObservations = []serviceRuntime.XrayObservatoryState{
		{OutboundTag: "node-node-kurumi", Alive: true, DelayMilliseconds: 45},
		{OutboundTag: "node-node-kurumi-2", Alive: true, DelayMilliseconds: 12},
	}
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{`"all_nodes_down":false`, `"dead_node_ids":[]`, `"delay_ms":45`, `"delay_ms":12`, `"selected_node_ids":["node-kurumi-2"]`} {
		if !strings.Contains(body, required) {
			t.Fatalf("recovered status missing %q: %s", required, body)
		}
	}
}

func TestXrayStatusMixedGroupDoesNotBlameProbeTarget(t *testing.T) {
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: true, DelayMilliseconds: 20},
			{OutboundTag: "node-node-kurumi-2", Alive: false, DelayMilliseconds: 99999999, LastErrorReason: "context deadline exceeded"},
		},
		xrayStates: []serviceRuntime.XrayBalancerState{{Tag: "group-grp-main", SelectedOutboundTags: []string{"node-node-kurumi"}}},
	}
	server, _ := xrayGroupStatusTestServerFor(t, controller, "latency", "http://127.0.0.1:1/unreachable")
	body := xrayGroupStatusPayload(t, server)
	for _, required := range []string{
		`"probe_target_reachable":true`,
		`"health_reason":"some_nodes_unhealthy"`,
		`"delay_ms":99999999`,
		`"health_reason":"node_endpoint_unreachable"`,
		`"last_error_reason":"context deadline exceeded"`,
		`"selected_node_ids":["node-kurumi"]`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("mixed-group status missing %q: %s", required, body)
		}
	}
}

func TestXrayStatusProbeURLFromEnvironment(t *testing.T) {
	t.Setenv(proxy.ProxyProbeURLEnv, "https://1.1.1.1")
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true}},
		xrayObservations: []serviceRuntime.XrayObservatoryState{
			{OutboundTag: "node-node-kurumi", Alive: true, DelayMilliseconds: 20},
			{OutboundTag: "node-node-kurumi-2", Alive: true, DelayMilliseconds: 30},
		},
	}
	server, _ := xrayGroupStatusTestServer(t, controller)
	body := xrayGroupStatusPayload(t, server)
	if !strings.Contains(body, `"probe_url":"https://1.1.1.1"`) {
		t.Fatalf("environment probe URL was not reflected in status: %s", body)
	}
}
