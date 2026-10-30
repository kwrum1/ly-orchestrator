package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

const qosPolicerPolicy = `{"id":"qos-failclosed","rules":[{"id":"limit-https","granularity":"rule",` +
	`"match":{"sources":["192.168.10.0/24"],"protocols":["tcp"],"dest_ports":["443"],"direction":"both"},` +
	`"actions":[{"kind":"policer","policer":{"rate_bps":20000000,"burst_bps":2000000}}]}]}`

func qosTestServer(t *testing.T, available bool) (*Server, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-qos-failclosed-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store),
		WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}),
		WithClock(fixedClock()),
		withQoSPluginProbe(func(context.Context) (bool, error) { return available, nil }))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	return server, login.Result().Cookies()[0]
}

// A build without any QoS dataplane plugin must reject the policy instead of
// persisting it and degrading every later apply.
func TestTrafficControlWriteFailsClosedWithoutQoSDataplane(t *testing.T) {
	server, cookie := qosTestServer(t, false)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosPolicerPolicy, cookie)
	if created.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create traffic control = %d %s, want 422", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `"qos_unsupported"`) {
		t.Fatalf("create error body = %s, want qos_unsupported", created.Body.String())
	}
	fetched := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/gateway/traffic-control/qos-failclosed", "", cookie)
	if fetched.Code != http.StatusNotFound {
		t.Fatalf("read back rejected policy = %d %s, want 404", fetched.Code, fetched.Body.String())
	}
}

// A build that can enforce policers still accepts the policy.
func TestTrafficControlWriteAcceptedWithQoSDataplane(t *testing.T) {
	server, cookie := qosTestServer(t, true)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/traffic-control", qosPolicerPolicy, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create traffic control = %d %s, want 200", created.Code, created.Body.String())
	}
}

func TestRequestBodyLimitRejectsOversizedPayloadWith413(t *testing.T) {
	server := New()
	body := bytes.Repeat([]byte("x"), int(maxRequestBodyBytes)+1)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d %s, want 413", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "payload_too_large") {
		t.Fatalf("oversized body error = %s, want payload_too_large", response.Body.String())
	}
}
