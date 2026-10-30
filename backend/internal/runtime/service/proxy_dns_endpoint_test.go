package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/dns"
	"ly-route/backend/internal/runtime/proxy"
)

func proxyDNSPlannerUpstreams() []SmartDNSUpstream {
	return []SmartDNSUpstream{
		{ID: "dns-udp", Servers: []string{"198.51.100.53"}},
		{ID: "dns-tcp", Servers: []string{"tcp://198.51.100.54:5353"}},
		{ID: "dns-doh", Servers: []string{"https://dns.example/dns-query"}, ResolvedHostIPs: map[string][]string{"dns.example": {"203.0.113.53"}}},
		{ID: "dns-dot", Servers: []string{"tls://dot.example:853"}, ResolvedHostIPs: map[string][]string{"dot.example": {"203.0.113.54"}}},
	}
}

func proxyDNSPlannerBindings() []dns.ProxyDNSBinding {
	return []dns.ProxyDNSBinding{
		{RuleID: "media", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp", FailureMode: dns.FailureModeFailClosed},
		{RuleID: "video", ProxyEgressID: "proxy-media", UpstreamID: "dns-tcp", FailureMode: dns.FailureModeReject},
		{RuleID: "doh", ProxyEgressID: "proxy-media", UpstreamID: "dns-doh"},
		{RuleID: "dot", ProxyEgressID: "proxy-media", UpstreamID: "dns-dot"},
	}
}

func TestPlanProxyDNSEndpointsAllocatesStableLoopbackEndpoints(t *testing.T) {
	first := PlanProxyDNSEndpoints(proxyDNSPlannerBindings(), proxyDNSPlannerUpstreams(), map[string]string{"proxy-media": "node-a"}, map[string]bool{"proxy-media": true})
	if len(first.Problems) != 0 {
		t.Fatalf("unexpected problems: %#v", first.Problems)
	}
	if len(first.Endpoints) != 4 {
		t.Fatalf("endpoints = %#v", first.Endpoints)
	}
	ports := map[int]string{}
	byUpstream := map[string]ProxyDNSEndpoint{}
	for _, endpoint := range first.Endpoints {
		if endpoint.ListenAddress != ProxyDNSEndpointListenAddress {
			t.Fatalf("endpoint %q listens on %q, want loopback only", endpoint.ID, endpoint.ListenAddress)
		}
		if endpoint.Port < ProxyDNSEndpointPortBase || endpoint.Port >= ProxyDNSEndpointPortBase+ProxyDNSEndpointPortSpan {
			t.Fatalf("endpoint %q port %d outside the reserved range", endpoint.ID, endpoint.Port)
		}
		if other, exists := ports[endpoint.Port]; exists {
			t.Fatalf("endpoints %q and %q share port %d", other, endpoint.ID, endpoint.Port)
		}
		ports[endpoint.Port] = endpoint.ID
		if endpoint.OutboundTag != "node-a" {
			t.Fatalf("endpoint %q outbound = %q", endpoint.ID, endpoint.OutboundTag)
		}
		byUpstream[endpoint.UpstreamID] = endpoint
	}
	if byUpstream["dns-udp"].Protocol != proxy.ProxyDNSEndpointUDP || byUpstream["dns-udp"].ServerPort != 53 || byUpstream["dns-udp"].ServerAddress != "198.51.100.53" {
		t.Fatalf("udp endpoint = %#v", byUpstream["dns-udp"])
	}
	if byUpstream["dns-tcp"].Protocol != proxy.ProxyDNSEndpointTCP || byUpstream["dns-tcp"].ServerPort != 5353 {
		t.Fatalf("tcp endpoint = %#v", byUpstream["dns-tcp"])
	}
	if byUpstream["dns-doh"].Protocol != proxy.ProxyDNSEndpointDoH || byUpstream["dns-doh"].ServerAddress != "203.0.113.53" || byUpstream["dns-doh"].ServerName != "dns.example" || byUpstream["dns-doh"].ServerPort != 443 {
		t.Fatalf("doh endpoint = %#v", byUpstream["dns-doh"])
	}
	if byUpstream["dns-dot"].Protocol != proxy.ProxyDNSEndpointDoT || byUpstream["dns-dot"].ServerPort != 853 || byUpstream["dns-dot"].ServerName != "dot.example" {
		t.Fatalf("dot endpoint = %#v", byUpstream["dns-dot"])
	}

	// Reversing the input order must not move a port: the allocation is keyed
	// by the pair identity, not by rule order.
	reversed := proxyDNSPlannerBindings()
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	second := PlanProxyDNSEndpoints(reversed, proxyDNSPlannerUpstreams(), map[string]string{"proxy-media": "node-a"}, map[string]bool{"proxy-media": true})
	if len(second.Endpoints) != len(first.Endpoints) {
		t.Fatalf("second plan = %#v", second.Endpoints)
	}
	for _, endpoint := range second.Endpoints {
		previous, exists := first.Endpoint(endpoint.ProxyEgressID, endpoint.UpstreamID)
		if !exists || previous.Port != endpoint.Port || previous.ID != endpoint.ID {
			t.Fatalf("endpoint moved between plans: %#v vs %#v", previous, endpoint)
		}
	}
}

func TestPlanProxyDNSEndpointsReportsUnavailableEndpoints(t *testing.T) {
	bindings := []dns.ProxyDNSBinding{
		{RuleID: "no-upstream", ProxyEgressID: "proxy-media"},
		{RuleID: "missing-upstream", ProxyEgressID: "proxy-media", UpstreamID: "dns-missing"},
		{RuleID: "h3", ProxyEgressID: "proxy-media", UpstreamID: "dns-h3"},
		{RuleID: "unresolved-doh", ProxyEgressID: "proxy-media", UpstreamID: "dns-doh-unresolved"},
		{RuleID: "plain-hostname", ProxyEgressID: "proxy-media", UpstreamID: "dns-hostname"},
		{RuleID: "degraded-egress", ProxyEgressID: "proxy-degraded", UpstreamID: "dns-udp"},
		{RuleID: "wan-pinned", ProxyEgressID: "proxy-media", UpstreamID: "dns-wan"},
	}
	upstreams := []SmartDNSUpstream{
		{ID: "dns-h3", Servers: []string{"h3://dns.example/dns-query"}},
		{ID: "dns-doh-unresolved", Servers: []string{"https://dns.example/dns-query"}},
		{ID: "dns-hostname", Servers: []string{"dns.example"}},
		{ID: "dns-udp", Servers: []string{"198.51.100.53"}},
		{ID: "dns-wan", Servers: []string{"198.51.100.55"}, WANEgressID: "wan-primary"},
	}
	plan := PlanProxyDNSEndpoints(bindings, upstreams, map[string]string{"proxy-media": "node-a"}, map[string]bool{"proxy-media": true})
	if len(plan.Endpoints) != 0 {
		t.Fatalf("endpoints = %#v", plan.Endpoints)
	}
	reasons := map[string]string{}
	for _, problem := range plan.Problems {
		reasons[problem.UpstreamID] = problem.Reason
	}
	for _, want := range []struct {
		upstream string
		contains []string
	}{
		{"", []string{"must select the resolver", "upstream_id"}},
		{"dns-missing", []string{"not configured or is disabled"}},
		{"dns-h3", []string{"HTTP/3 and QUIC"}},
		{"dns-doh-unresolved", []string{"no resolved IPv4 address", "bootstrap"}},
		{"dns-hostname", []string{"literal IP address"}},
		{"dns-udp", []string{"no compiled runtime"}},
		{"dns-wan", []string{"pinned to WAN egress"}},
	} {
		reason, exists := reasons[want.upstream]
		if !exists {
			t.Fatalf("missing problem for %q: %#v", want.upstream, plan.Problems)
		}
		for _, fragment := range want.contains {
			if !strings.Contains(reason, fragment) {
				t.Fatalf("problem %q reason = %q, missing %q", want.upstream, reason, fragment)
			}
		}
	}
}

func TestPlanProxyDNSEndpointsDeduplicatesSharedPairs(t *testing.T) {
	bindings := []dns.ProxyDNSBinding{
		{RuleID: "a", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"},
		{RuleID: "b", ProxyEgressID: "proxy-media", UpstreamID: "dns-udp"},
	}
	plan := PlanProxyDNSEndpoints(bindings, proxyDNSPlannerUpstreams(), nil, map[string]bool{"proxy-media": true})
	if len(plan.Endpoints) != 1 {
		t.Fatalf("endpoints = %#v, want one shared endpoint", plan.Endpoints)
	}
	if plan.Endpoints[0].OutboundTag != "proxy-media" {
		t.Fatalf("fallback outbound tag = %q, want the egress id", plan.Endpoints[0].OutboundTag)
	}
	if !strings.HasPrefix(plan.Endpoints[0].Group(), "lyroute-px-") {
		t.Fatalf("group = %q", plan.Endpoints[0].Group())
	}
	if dns.ProxyDNSEndpointGroup("proxy-media", "dns-udp") != dns.ProxyDNSEndpointGroup(" proxy-media ", " DNS-UDP ") {
		t.Fatal("endpoint group identity must be case/space insensitive")
	}
}

func TestProxyDNSEndpointPortAllocationExhaustion(t *testing.T) {
	used := map[int]string{}
	for port := ProxyDNSEndpointPortBase; port < ProxyDNSEndpointPortBase+ProxyDNSEndpointPortSpan; port++ {
		used[port] = "taken"
	}
	if port := allocateProxyDNSEndpointPort("proxy-media", "dns-udp", used); port != 0 {
		t.Fatalf("exhausted range returned port %d", port)
	}
}
