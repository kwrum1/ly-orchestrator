package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/persistence"
)

func dnsProxyTestServer(t *testing.T, documents ...map[string]any) (*Server, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, document := range documents {
		resourceType := strings.TrimSpace(stringField(document, "resource_type"))
		id := strings.TrimSpace(stringField(document, "id"))
		payload := cloneObject(document)
		delete(payload, "resource_type")
		raw, hash, err := persistence.MarshalPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveConfig(ctx, persistence.ConfigDocument{ResourceType: resourceType, ResourceID: id, Payload: raw, PayloadHash: hash, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	server := New(WithStore(store), WithAuthConfig(AuthConfig{AdminUsername: "admin", AdminPassword: "secret"}), WithClock(fixedClock()))
	login := requestBody(t, server, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
	}
	return server, login.Result().Cookies()[0]
}

func postDNSPolicy(t *testing.T, server *Server, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/dns/policies", bytes.NewBufferString(body))
	req.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func TestDNSPolicyAcceptsProxyOutcomeWithLocalEndpoint(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-egress-default","upstream_id":"dns-direct-default"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200: %s", response.Code, response.Body.String())
	}
	// The save response is the operator's readback of the endpoint generation:
	// loopback only, port inside the reserved range, one endpoint per pair.
	var created struct {
		Item struct {
			ProxyDNSEndpoints *struct {
				Endpoints []struct {
					ID            string `json:"id"`
					ProxyEgressID string `json:"proxy_egress_id"`
					UpstreamID    string `json:"upstream_id"`
					Protocol      string `json:"protocol"`
					ListenAddress string `json:"listen_address"`
					Port          int    `json:"port"`
				} `json:"endpoints"`
				Problems []struct {
					Reason string `json:"reason"`
				} `json:"problems"`
			} `json:"proxy_dns_endpoints"`
		} `json:"item"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, response.Body.String())
	}
	plan := created.Item.ProxyDNSEndpoints
	if plan == nil || len(plan.Problems) != 0 || len(plan.Endpoints) != 1 {
		t.Fatalf("proxy endpoint readback = %#v: %s", plan, response.Body.String())
	}
	endpoint := plan.Endpoints[0]
	if endpoint.ListenAddress != "127.0.0.1" || endpoint.Port < 15353 || endpoint.Port > 15452 || endpoint.Protocol != "udp" || endpoint.UpstreamID != "dns-direct-default" {
		t.Fatalf("endpoint = %#v", endpoint)
	}

	list := request(t, server, http.MethodGet, "/api/v1/dns/policies")
	if !strings.Contains(list.Body.String(), "media.example") {
		t.Fatalf("accepted proxy policy was not persisted: %s", list.Body.String())
	}

	readback := request(t, server, http.MethodGet, "/api/v1/dns/proxy-endpoints?probe=0")
	if readback.Code != http.StatusOK {
		t.Fatalf("proxy endpoint readback status = %d: %s", readback.Code, readback.Body.String())
	}
	if !strings.Contains(readback.Body.String(), `"port_range":"15353-15452"`) {
		t.Fatalf("readback does not publish the port range: %s", readback.Body.String())
	}
}

func TestDNSPolicyRejectsProxyOutcomeWithoutUpstream(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-egress-default"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	for _, required := range []string{"upstream_id", "must select the resolver"} {
		if !strings.Contains(response.Body.String(), required) {
			t.Fatalf("response missing actionable %q: %s", required, response.Body.String())
		}
	}
}

func TestDNSPolicyRejectsProxyOutcomeWithUnsupportedProtocol(t *testing.T) {
	server, cookie := dnsProxyTestServer(t, map[string]any{
		"resource_type": "dns_upstream", "id": "dns-h3", "name": "HTTP/3 Resolver", "enabled": true,
		"servers": []string{"h3://dns.example/dns-query"},
	})
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-egress-default","upstream_id":"dns-h3"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "HTTP/3 and QUIC") {
		t.Fatalf("response does not explain the unsupported protocol: %s", response.Body.String())
	}
}

func TestDNSPolicyRejectsProxyOutcomeForMissingEgress(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-ghost","upstream_id":"dns-direct-default"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "missing proxy egress") && !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("response does not explain the missing egress: %s", response.Body.String())
	}
}

func TestDNSPolicyRejectsProxyOutcomeWithMissingUpstream(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-egress-default","upstream_id":"dns-missing"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "missing DNS upstream") {
		t.Fatalf("response does not explain the missing upstream: %s", response.Body.String())
	}
}

func TestDNSPolicyRejectsImplicitProxyFallback(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-media","domains":["media.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-egress-default","failure_mode":"fallback_direct"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "requires an explicit fallback_upstream_id") {
		t.Fatalf("response does not require explicit fallback: %s", response.Body.String())
	}
}

func TestDNSPolicyRejectsUnsafeRuleID(t *testing.T) {
	server, cookie := dnsProxyTestServer(t)
	body := `{"id":"unsafe","kind":"policy","name":"Unsafe","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"legit\naddress /injected.example/203.0.113.9","domains":["media.example"],"outcome":{"kind":"reject"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "rule id") {
		t.Fatalf("response does not explain the unsafe rule id: %s", response.Body.String())
	}
}

func TestDNSPolicyRejectsProxyDNSDependencyCycle(t *testing.T) {
	server, cookie := dnsProxyTestServer(t,
		map[string]any{
			"resource_type": "proxy_node", "id": "node-a", "name": "Node A", "enabled": true,
			"protocol": "vless", "address": "node.a.example", "port": 443,
		},
		map[string]any{
			"resource_type": "proxy_egress", "id": "proxy-a", "name": "Proxy A", "enabled": true,
			"semantic_type": "proxy_egress", "display_list": "wan", "runtime_profile": "xray-tproxy-outbound",
			"capture_path": "vpp_service_interface", "engine": "xray", "handoff": "vpp_to_service",
			"listener_mode": "vpp-service", "underlay_wan_id": "wan-a", "node_id": "node-a",
		},
	)
	body := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-node","domains":["node.a.example"],"outcome":{"kind":"proxy","proxy_egress_id":"proxy-a"}}]}}`
	response := postDNSPolicy(t, server, cookie, body)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "dependency cycle") || !strings.Contains(response.Body.String(), "node.a.example") {
		t.Fatalf("response does not explain the DNS/proxy cycle: %s", response.Body.String())
	}

	directBody := `{"id":"proxy-dns","kind":"policy","name":"Proxy DNS","enabled":true,"policy":{"engine":"smartdns","miss":{"kind":"reject"},"rules":[{"id":"proxy-node","domains":["node.a.example"],"outcome":{"kind":"direct","upstream_id":"dns-direct-default"}}]}}`
	if direct := postDNSPolicy(t, server, cookie, directBody); direct.Code != http.StatusOK {
		t.Fatalf("direct node resolution status = %d, want 200: %s", direct.Code, direct.Body.String())
	}
}
