package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/runtime/vpp"
)

var qosCapabilityStoreSeq atomic.Uint64

const qosDropPolicy = `{"id":"qos-drop","rules":[{"id":"block-https","granularity":"rule",` +
	`"match":{"sources":["192.168.10.0/24"],"protocols":["tcp"],"dest_ports":["443"]},` +
	`"actions":[{"kind":"drop"}]}]}`

const qosRemarkPolicy = `{"id":"qos-remark","rules":[{"id":"mark-voice","granularity":"rule",` +
	`"match":{"sources":["192.168.10.0/24"],"protocols":["udp"],"dest_ports":["5060"]},` +
	`"actions":[{"kind":"remark","dscp":"EF"}]}]}`

func qosCapabilityWith(core, flowRate, policer bool) vpp.QoSCapability {
	return qosCapabilityWithShaping(core, flowRate, flowRate, policer)
}

func qosCapabilityWithShaping(core, flowRate, shaping, policer bool) vpp.QoSCapability {
	return vpp.QoSCapability{
		CoreQoS:  vpp.KnownFact(core),
		FlowRate: vpp.KnownFact(flowRate),
		Shaping:  vpp.KnownFact(shaping),
		Policer:  vpp.KnownFact(policer),
		Source:   "test",
	}
}

const qosShapePolicy = `{"id":"qos-shape","rules":[{"id":"shape-lan","granularity":"rule",` +
	`"match":{"direction":"downlink"},"actions":[{"kind":"shape","shape":` +
	`{"rate_bps":1000000,"burst_bps":100000,"host_isolation":true,"direction":"downlink"}}]}]}`

func qosCapabilityServer(t *testing.T, capability vpp.QoSCapability, probeErr error) (*Server, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	dsn := fmt.Sprintf("file:httpapi-qos-capability-test-%d?mode=memory&cache=shared", qosCapabilityStoreSeq.Add(1))
	store, err := persistence.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithClock(fixedClock()),
		withQoSCapabilityProbe(func(context.Context) (vpp.QoSCapability, error) {
			return capability, probeErr
		}))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	return server, login.Result().Cookies()[0]
}

// A rate rule compiles into vpp.behavior.rate, which only the product
// flow-rate plugin can enforce. Saving it must fail closed with the missing
// component named, even when VPP core QoS is available.
func TestTrafficControlWriteRejectsRateWithoutFlowRatePlugin(t *testing.T) {
	server, cookie := qosCapabilityServer(t, qosCapabilityWith(true, false, true), nil)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosPolicerPolicy, cookie)
	if created.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create rate policy = %d %s, want 422", created.Code, created.Body.String())
	}
	body := created.Body.String()
	if !strings.Contains(body, `"qos_unsupported"`) || !strings.Contains(body, "ly_route_smart_qos_plugin.so") {
		t.Fatalf("create error = %s, want qos_unsupported naming the flow-rate plugin", body)
	}
	if strings.Contains(body, "Core QoS") {
		t.Fatalf("create error = %s, must not blame the core QoS CLI", body)
	}
}

// Blocking is ACL-backed and stays available on a build with no QoS facility
// at all; rejecting it would be a fake capability limit.
func TestTrafficControlWriteAcceptsDropWithoutAnyQoSFacility(t *testing.T) {
	server, cookie := qosCapabilityServer(t, qosCapabilityWith(false, false, false), nil)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosDropPolicy, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create drop policy = %d %s, want 200", created.Code, created.Body.String())
	}
	// A blocking rule is ACL-backed, so it stays a first-class traffic-control
	// decision: the save was accepted and the resource is pending apply. It
	// must never be reported as applied without dataplane evidence.
	list := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/gateway/traffic-control", "", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list traffic control = %d %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"runtime_state":"desired_not_applied"`) {
		t.Fatalf("list = %s, want runtime_state desired_not_applied", list.Body.String())
	}
	if strings.Contains(list.Body.String(), `"runtime_state":"applied"`) {
		t.Fatalf("list = %s, must not report the rule applied", list.Body.String())
	}
}

// DSCP remark compiles into VPP core QoS (26.06: vnet core, no plugin), so a
// build without the flow-rate plugin can still accept it.
func TestTrafficControlWriteAcceptsRemarkWithCoreQoSOnly(t *testing.T) {
	server, cookie := qosCapabilityServer(t, qosCapabilityWith(true, false, false), nil)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosRemarkPolicy, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create remark policy = %d %s, want 200", created.Code, created.Body.String())
	}
}

func TestTrafficControlWriteRejectsRemarkWithoutCoreQoS(t *testing.T) {
	server, cookie := qosCapabilityServer(t, qosCapabilityWith(false, true, true), nil)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosRemarkPolicy, cookie)
	if created.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create remark policy = %d %s, want 422", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), "qos record/store/egress-map/mark") {
		t.Fatalf("create error = %s, want the core QoS CLI named", created.Body.String())
	}
}

// A policy saved while the plugin was present must stop claiming to be
// applied once the capability is gone. This is the upgrade/rollback shape:
// the stored rate rule survives, the enforcing plugin does not.
func TestTrafficControlRatePolicyStopsClaimingAppliedWithoutPlugin(t *testing.T) {
	var current atomic.Value
	current.Store(qosCapabilityWith(true, true, true))
	ctx := context.Background()
	dsn := fmt.Sprintf("file:httpapi-qos-capability-test-%d?mode=memory&cache=shared", qosCapabilityStoreSeq.Add(1))
	store, err := persistence.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithClock(fixedClock()),
		withQoSCapabilityProbe(func(context.Context) (vpp.QoSCapability, error) {
			return current.Load().(vpp.QoSCapability), nil
		}))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	cookie := login.Result().Cookies()[0]

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosPolicerPolicy, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create rate policy = %d %s, want 200", created.Code, created.Body.String())
	}

	current.Store(qosCapabilityWith(true, false, true))
	list := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/gateway/traffic-control", "", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list traffic control = %d %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, `"runtime_state":"unsupported"`) {
		t.Fatalf("list = %s, want runtime_state unsupported", body)
	}
	if !strings.Contains(body, "ly_route_smart_qos_plugin.so") {
		t.Fatalf("list = %s, want the missing plugin named", body)
	}
	if strings.Contains(body, `"runtime_state":"applied"`) {
		t.Fatalf("list = %s, must not keep claiming the rate rule is applied", body)
	}
}

func TestQoSRuntimeComponentUsesRealCapabilityStates(t *testing.T) {

	tests := []struct {
		name       string
		capability vpp.QoSCapability
		probeErr   error
		wantState  string
		wantReason string
	}{
		{name: "supported", capability: qosCapabilityWith(true, true, true), wantState: "running"},
		{name: "unsupported", capability: qosCapabilityWith(true, false, true), wantState: "unsupported", wantReason: "ly_route_smart_qos_plugin.so"},
		{name: "probe failure", capability: vpp.QoSCapability{}, probeErr: context.DeadlineExceeded, wantState: "unknown", wantReason: "probe failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := New(withQoSCapabilityProbe(func(context.Context) (vpp.QoSCapability, error) {
				return test.capability, test.probeErr
			}))
			component := server.qosRuntimeComponent(context.Background())
			if component.Name != "qos" || component.State != test.wantState {
				t.Fatalf("component = %#v, want state %q", component, test.wantState)
			}
			if test.wantState != "running" && component.Available {
				t.Fatalf("component = %#v, unsupported/unknown must not be available", component)
			}
			if test.wantReason != "" && !strings.Contains(component.Reason, test.wantReason) {
				t.Fatalf("reason = %q, want %q", component.Reason, test.wantReason)
			}
		})
	}
}

func TestApplyQoSPlanStateMarksUnsupportedInsteadOfSkipped(t *testing.T) {
	server := New()
	components := []RuntimeComponentState{{Name: "qos", State: "running", Available: true}}
	components = server.applyQoSPlanState(components, true, "qos unsupported: missing flow-rate plugin")
	if components[0].State != "unsupported" || components[0].Available {
		t.Fatalf("component = %#v, want unsupported", components[0])
	}
	if !strings.Contains(components[0].Reason, "flow-rate") {
		t.Fatalf("reason = %q, want the plan verdict", components[0].Reason)
	}

	unchanged := server.applyQoSPlanState([]RuntimeComponentState{{Name: "qos", State: "running", Available: true}}, false, "")
	if unchanged[0].State != "running" {
		t.Fatalf("component = %#v, want running", unchanged[0])
	}
}

// Shaping is implemented by the same plugin as flow-rate but through a
// different CLI. A plugin that only provides flow-rate must reject a shape
// policy and name the missing fq-codel contract, not blame the rate rule.
func TestTrafficControlWriteRejectsShapeWithoutFqCodelCLI(t *testing.T) {
	server, cookie := qosCapabilityServer(t, qosCapabilityWithShaping(true, true, false, true), nil)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosShapePolicy, cookie)
	if created.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create shape policy = %d %s, want 422", created.Code, created.Body.String())
	}
	body := created.Body.String()
	if !strings.Contains(body, `"qos_unsupported"`) || !strings.Contains(body, "fq-codel shaping") {
		t.Fatalf("create error = %s, want qos_unsupported naming the fq-codel shaping CLI", body)
	}
	if strings.Contains(body, "set ly-route flow-rate") {
		t.Fatalf("create error = %s, must not blame the flow-rate rule", body)
	}
}
