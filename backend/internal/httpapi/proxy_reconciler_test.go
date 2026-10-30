package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/proxy"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

type fakeProxyFailureActuator struct {
	active        bool
	configured    []proxyFailurePageConfig
	activations   int
	deactivations int
	disables      int
	activateErr   error
	deactivateErr error
}

func (actuator *fakeProxyFailureActuator) Configure(_ context.Context, config proxyFailurePageConfig) error {
	actuator.configured = append(actuator.configured, config)
	return nil
}

func (actuator *fakeProxyFailureActuator) Activate(_ context.Context, config proxyFailurePageConfig) error {
	if actuator.activateErr != nil {
		return actuator.activateErr
	}
	actuator.active = true
	actuator.activations++
	actuator.configured = append(actuator.configured, config)
	return nil
}

func (actuator *fakeProxyFailureActuator) Deactivate(context.Context) error {
	if actuator.deactivateErr != nil {
		return actuator.deactivateErr
	}
	actuator.active = false
	actuator.deactivations++
	return nil
}

func (actuator *fakeProxyFailureActuator) Disable(context.Context) error {
	actuator.active = false
	actuator.disables++
	return nil
}

type proxyFailureTestClock struct {
	current time.Time
}

func (clock *proxyFailureTestClock) Now() time.Time { return clock.current }
func (clock *proxyFailureTestClock) Advance(duration time.Duration) {
	clock.current = clock.current.Add(duration)
}

func openProxyFailureStore(t *testing.T, name string) *persistence.Store {
	t.Helper()
	store, err := persistence.Open(context.Background(), "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func saveProxyFailureNode(t *testing.T, store *persistence.Store, id, address string, port int) {
	t.Helper()
	payload := map[string]any{
		"id": id, "name": id, "enabled": true,
		"protocol": "trojan", "address": address, "port": port,
	}
	if err := store.SaveConfigWithSecrets(context.Background(), configDocument(t, "proxy_node", id, payload, fixedClock()()), map[string]string{"secret": "test-password"}); err != nil {
		t.Fatal(err)
	}
}

func saveProxyFailureEgress(t *testing.T, store *persistence.Store, egress proxy.Egress) {
	t.Helper()
	raw, err := json.Marshal(egress)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(context.Background(), configDocument(t, "proxy_egress", egress.ID, payload, fixedClock()())); err != nil {
		t.Fatal(err)
	}
}

func newProxyFailureServer(t *testing.T, store *persistence.Store, controller *httpServiceController, actuator *fakeProxyFailureActuator) (*Server, *proxyFailureTestClock) {
	t.Helper()
	clock := &proxyFailureTestClock{current: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	server := New(
		WithStore(store),
		WithClock(clock.Now),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithServiceRuntime(serviceRuntime.Runtime{Controller: controller}),
		withProxyFailureActuator(actuator),
	)
	return server, clock
}

func observeProxyFailure(t *testing.T, server *Server, controller *httpServiceController, observations []serviceRuntime.XrayObservatoryState) error {
	t.Helper()
	controller.xrayObservations = observations
	server.services.InvalidateHealth()
	return server.reconcileProxyHealth(context.Background(), controller)
}

func TestProxyFailurePageRequiresTwoConsecutiveDeadSamples(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-two-samples")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: false}}
	if err := observeProxyFailure(t, server, controller, dead); err != nil {
		t.Fatal(err)
	}
	if actuator.active {
		t.Fatal("failure page activated after a single dead sample")
	}
	status := server.proxyFailureSnapshot()
	if !status.AllNodesDown || status.ConsecutiveAllDown != 1 || status.FailurePageActive {
		t.Fatalf("first sample state = %#v", status)
	}

	if err := observeProxyFailure(t, server, controller, dead); err != nil {
		t.Fatal(err)
	}
	if !actuator.active || actuator.activations != 1 {
		t.Fatalf("failure page was not activated after two samples: %#v", actuator)
	}
	status = server.proxyFailureSnapshot()
	if !status.AllNodesDown || !status.FailurePageActive || status.ConsecutiveAllDown != 2 {
		t.Fatalf("activated state = %#v", status)
	}
	if status.FailureSince == nil || status.LastSampleAt == nil || status.FailureReason != "all_proxy_nodes_unreachable" {
		t.Fatalf("activated timestamps/reason = %#v", status)
	}
	if len(status.DeadNodeIDs) != 1 || status.DeadNodeIDs[0] != "node-proxy-node-test" {
		t.Fatalf("dead node ids = %#v", status.DeadNodeIDs)
	}
}

func TestProxyFailurePageRecoversOnFirstAliveSample(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-recovery")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: false}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page did not activate")
	}

	if err := observeProxyFailure(t, server, controller, []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: true, DelayMilliseconds: 12}}); err != nil {
		t.Fatal(err)
	}
	if actuator.active {
		t.Fatal("failure page stayed active after an alive sample")
	}
	if actuator.deactivations != 1 {
		t.Fatalf("deactivations = %d", actuator.deactivations)
	}
	if len(controller.applied) == 0 || controller.applied[len(controller.applied)-1] != string(serviceRuntime.Nftables) {
		t.Fatalf("capture was not restored before removing the page: %#v", controller.applied)
	}
	status := server.proxyFailureSnapshot()
	if status.FailurePageActive || status.AllNodesDown || status.ConsecutiveAllDown != 0 {
		t.Fatalf("recovered state = %#v", status)
	}
	if status.RecoveredAt == nil || status.FailureSince != nil {
		t.Fatalf("recovered timestamps = %#v", status)
	}
}

func TestProxyFailurePageHoldsModeOnObservatoryError(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-observatory-error")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: false}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page did not activate")
	}

	controller.xrayObservationErr = errors.New("observatory endpoint unavailable")
	server.services.InvalidateHealth()
	if err := server.reconcileProxyHealth(context.Background(), controller); err == nil {
		t.Fatal("observatory error was not reported")
	}
	if !actuator.active {
		t.Fatal("failure page was removed on an observatory error")
	}
	status := server.proxyFailureSnapshot()
	if !status.FailurePageActive || status.FailureError == "" {
		t.Fatalf("error state = %#v", status)
	}

	// An observatory error must not fake a recovery either: the page stays.
	controller.xrayObservationErr = nil
	controller.xrayObservations = nil
	server.services.InvalidateHealth()
	if err := server.reconcileProxyHealth(context.Background(), controller); err == nil {
		t.Fatal("empty observatory sample was not reported")
	}
	if !actuator.active {
		t.Fatal("failure page was removed without an alive sample")
	}
}

func TestProxyFailurePageActivatesImmediatelyWhenXrayIsInactive(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-xray-inactive")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: false, Reason: "unit xray.service is inactive"},
		},
		xrayObservations: []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: true}},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	server.services.InvalidateHealth()
	if err := server.reconcileProxyHealth(context.Background(), controller); err != nil {
		t.Fatal(err)
	}
	if !actuator.active {
		t.Fatal("failure page was not activated for an inactive Xray process")
	}
	status := server.proxyFailureSnapshot()
	if !status.AllNodesDown || status.ConsecutiveAllDown != 1 || !strings.Contains(status.FailureReason, "xray_inactive") {
		t.Fatalf("inactive xray state = %#v", status)
	}
}

func TestProxyFailurePageActivatesForAdaptiveSubscription(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-adaptive")
	saveProxyFailureNode(t, store, "a", "192.0.2.10", 443)
	saveProxyFailureNode(t, store, "b", "192.0.2.11", 443)
	subscription := map[string]any{
		"id": "main", "enabled": true, "selection": "adaptive",
		"strategy": "adaptive_topn_weighted", "top_n": 2, "node_refs": []string{"a", "b"},
	}
	if err := store.SaveConfig(context.Background(), configDocument(t, "proxy_subscription", "main", subscription, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	egress := proxy.NewProxyEgress("proxy-adaptive", "xray-tproxy-outbound")
	egress.SubscriptionID = "main"
	saveProxyFailureEgress(t, store, egress)

	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{
		{OutboundTag: "subscription-main-node-a", Alive: false},
		{OutboundTag: "subscription-main-node-b", Alive: false},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page was not activated for an all-dead adaptive subscription")
	}
	status := server.proxyFailureSnapshot()
	if len(status.DeadNodeIDs) != 2 {
		t.Fatalf("adaptive dead ids = %#v", status.DeadNodeIDs)
	}
}

func TestProxyFailurePageActivatesForFastestSubscription(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-fastest")
	saveProxyFailureNode(t, store, "a", "192.0.2.10", 443)
	subscription := map[string]any{
		"id": "main", "enabled": true, "selection": "fastest", "top_n": 1, "node_refs": []string{"a"},
	}
	if err := store.SaveConfig(context.Background(), configDocument(t, "proxy_subscription", "main", subscription, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	egress := proxy.NewProxyEgress("proxy-fastest", "xray-tproxy-outbound")
	egress.SubscriptionID = "main"
	saveProxyFailureEgress(t, store, egress)

	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-a", Alive: false}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page was not activated for an all-dead fastest subscription")
	}
}

func TestProxyFailurePageClearsStaleGenerationWhenProxyIsHealthy(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-stale-generation")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
		xrayObservations: []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: true, DelayMilliseconds: 9}},
	}
	// Simulate a page left behind by the systemd crash path while the control
	// API was down.
	actuator := &fakeProxyFailureActuator{active: true}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	if err := observeProxyFailure(t, server, controller, controller.xrayObservations); err != nil {
		t.Fatal(err)
	}
	if actuator.active || actuator.deactivations != 1 {
		t.Fatalf("stale failure generation was not cleared: %#v", actuator)
	}
	if !server.proxyFailure.failureCleared() {
		t.Fatal("tracker did not record the healthy clear")
	}
	if err := observeProxyFailure(t, server, controller, controller.xrayObservations); err != nil {
		t.Fatal(err)
	}
	if actuator.deactivations != 1 {
		t.Fatalf("healthy clear is not idempotent: %#v", actuator)
	}
}

func TestProxyFailurePageIsDisabledWhenProxyEgressIsRemoved(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-egress-removed")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: false}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page did not activate")
	}

	if err := store.SaveConfig(context.Background(), configDocument(t, "proxy_egress", "proxy-egress-default", map[string]any{"id": "proxy-egress-default", "deleted": true}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	server.services.InvalidateHealth()
	if err := server.reconcileProxyHealth(context.Background(), controller); err != nil {
		t.Fatal(err)
	}
	if actuator.active || actuator.disables != 1 {
		t.Fatalf("failure page was not disabled with the proxy egress: %#v", actuator)
	}
	if status := server.proxyFailureSnapshot(); status.FailurePageActive || status.AllNodesDown {
		t.Fatalf("state after egress removal = %#v", status)
	}
}

func TestXrayStatusExposesFailurePageState(t *testing.T) {
	store := openProxyFailureStore(t, "proxy-failure-status")
	saveTestProxyNode(t, store)
	saveProxyFailureEgress(t, store, testProxyEgressWithNode())
	controller := &httpServiceController{
		health: map[serviceRuntime.ServiceName]serviceRuntime.Health{
			serviceRuntime.Xray: {Service: serviceRuntime.Xray, Available: true},
		},
	}
	actuator := &fakeProxyFailureActuator{}
	server, _ := newProxyFailureServer(t, store, controller, actuator)

	dead := []serviceRuntime.XrayObservatoryState{{OutboundTag: "node-proxy-node-test", Alive: false}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := observeProxyFailure(t, server, controller, dead); err != nil {
			t.Fatal(err)
		}
	}
	if !actuator.active {
		t.Fatal("failure page did not activate")
	}

	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("login status = %d body=%s", login.Code, login.Body.String())
	}
	server.services.InvalidateHealth()
	status := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/proxy/xray/status", "", login.Result().Cookies()[0])
	body := status.Body.String()
	for _, required := range []string{
		`"all_nodes_down":true`,
		`"failure_page_active":true`,
		`"failure_reason":"all_proxy_nodes_unreachable"`,
		`"consecutive_all_down_samples":2`,
		`"dead_node_ids":["node-proxy-node-test"]`,
		`"failure_since":"2026-10-24T12:00:00Z"`,
		`"last_health_sample_at":"2026-10-24T12:00:00Z"`,
	} {
		if status.Code != http.StatusOK || !strings.Contains(body, required) {
			t.Fatalf("Xray status missing %q: %d %s", required, status.Code, body)
		}
	}
}
