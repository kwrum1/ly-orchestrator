package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/dns"
	"ly-route/backend/internal/runtime/proxy"
)

func proxyDNSRenderPlan(t *testing.T, rules []dns.Rule, miss dns.Outcome, upstreams []SmartDNSUpstream, outboundTag string) (dns.SmartDNSRender, []SmartDNSUpstream, ProxyDNSEndpointPlan) {
	t.Helper()
	ids := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		ids = append(ids, upstream.ID)
	}
	compiled, err := dns.CompilePolicyWithOptions(dns.NewPolicy(miss, rules), dns.CompileOptions{
		ProxyEgresses:              []proxy.Egress{proxy.NewProxyEgress("proxy-media", "xray-vpp-service")},
		UpstreamIDs:                ids,
		ValidateUpstreamReferences: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	render := compiled.RenderSmartDNS()
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, map[string]string{"proxy-media": outboundTag}, map[string]bool{"proxy-media": true})
	return render, upstreams, plan
}

func renderProxyDNSTestConfig(t *testing.T, render dns.SmartDNSRender, upstreams []SmartDNSUpstream, plan ProxyDNSEndpointPlan) string {
	t.Helper()
	artifacts, err := RenderSmartDNS(SmartDNSPlan{
		ID:                "active",
		Render:            render,
		Upstreams:         upstreams,
		ProxyDNSEndpoints: plan,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.Path == "/etc/smartdns/conf.d/ly-route-active.conf" {
			return artifact.Content
		}
	}
	t.Fatalf("active SmartDNS artifact missing: %#v", artifacts)
	return ""
}

func TestRenderSmartDNSProxyOutcomeUsesLocalEndpoint(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, plan := proxyDNSRenderPlan(t,
		[]dns.Rule{
			{ID: "proxy-media", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"}},
			{ID: "direct-local", Domains: []string{"lan.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeDirect, UpstreamID: "dns-proxy"}},
		},
		dns.Reject(), upstreams, "node-a")
	if len(plan.Problems) != 0 || len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	endpoint := plan.Endpoints[0]
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)

	for _, required := range []string{
		"server 127.0.0.1:" + itoa(endpoint.Port) + " -group " + endpoint.Group() + " -exclude-default-group",
		"nameserver /-.media.example/" + endpoint.Group(),
		"# endpoint " + endpoint.ID + " egress=proxy-media upstream=dns-proxy protocol=udp failure_mode=fail_closed",
		"# Proxy DNS endpoints: loopback listeners owned by Xray",
	} {
		if !strings.Contains(config, required) {
			t.Fatalf("SmartDNS config missing %q:\n%s", required, config)
		}
	}
	// Only the rule that names the proxy outcome may use the endpoint group.
	if strings.Count(config, "nameserver /-.media.example/"+endpoint.Group()) != 1 {
		t.Fatalf("proxy nameserver rendered more than once:\n%s", config)
	}
	if strings.Contains(config, "nameserver /-.lan.example/"+endpoint.Group()) {
		t.Fatalf("direct rule was routed through the proxy endpoint:\n%s", config)
	}
	// The endpoint group must never join the default group: an unrelated query
	// would otherwise be resolved through the proxy egress.
	serverLine := ""
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, "server 127.0.0.1:") {
			serverLine = line
		}
	}
	if !strings.Contains(serverLine, "-exclude-default-group") {
		t.Fatalf("endpoint server line = %q", serverLine)
	}
}

func TestRenderSmartDNSProxyOutcomeDoHUsesSmartDNSHTTPSClient(t *testing.T) {
	upstreams := []SmartDNSUpstream{{
		ID:              "dns-doh",
		Servers:         []string{"https://dns.example/dns-query"},
		ResolvedHostIPs: map[string][]string{"dns.example": {"203.0.113.53"}},
	}}
	render, upstreams, plan := proxyDNSRenderPlan(t,
		[]dns.Rule{{ID: "proxy-doh", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-doh"}}},
		dns.Reject(), upstreams, "node-a")
	if len(plan.Endpoints) != 1 || plan.Endpoints[0].Protocol != proxy.ProxyDNSEndpointDoH {
		t.Fatalf("plan = %#v", plan)
	}
	endpoint := plan.Endpoints[0]
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)
	want := "server-https https://" + endpoint.Listen() + "/dns-query -group " + endpoint.Group() + " -exclude-default-group -host-name dns.example -http-host dns.example -tls-host-verify dns.example"
	if !strings.Contains(config, want) {
		t.Fatalf("SmartDNS config missing DoH endpoint %q:\n%s", want, config)
	}
	if !strings.Contains(config, "nameserver /-.media.example/"+endpoint.Group()) {
		t.Fatalf("DoH proxy rule missing nameserver:\n%s", config)
	}
}

func TestRenderSmartDNSProxyRejectBlocksWhenEndpointIsUnavailable(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, _ := proxyDNSRenderPlan(t,
		[]dns.Rule{{
			ID:      "proxy-reject",
			Domains: []string{"blocked.example"},
			Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy", FailureMode: dns.FailureModeReject},
		}},
		dns.Reject(), upstreams, "node-a")

	// No endpoint: the egress runtime is not established in this generation.
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, nil, map[string]bool{})
	if len(plan.Problems) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)
	if !strings.Contains(config, "# proxy DNS endpoint unavailable; failure_mode=reject blocks this domain instead of resolving it directly") {
		t.Fatalf("reject fallback not rendered:\n%s", config)
	}
	if !strings.Contains(config, "address /-.blocked.example/#") {
		t.Fatalf("reject outcome did not block the domain:\n%s", config)
	}
	if strings.Contains(config, "nameserver /-.blocked.example/") {
		t.Fatalf("unavailable endpoint was rendered as a nameserver:\n%s", config)
	}
}

func TestRenderSmartDNSProxyFailClosedWithoutEndpointRefusesToRender(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, _ := proxyDNSRenderPlan(t,
		[]dns.Rule{{ID: "proxy-media", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"}}},
		dns.Reject(), upstreams, "node-a")
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, nil, map[string]bool{})

	artifacts, err := RenderSmartDNS(SmartDNSPlan{ID: "active", Render: render, Upstreams: upstreams, ProxyDNSEndpoints: plan})
	if err == nil {
		t.Fatalf("render succeeded without an endpoint: %#v", artifacts)
	}
	if !strings.Contains(err.Error(), "requires proxy DNS endpoint") || !strings.Contains(err.Error(), "no compiled runtime") {
		t.Fatalf("error is not actionable: %v", err)
	}
	if artifacts != nil {
		t.Fatalf("render returned artifacts while failing closed: %#v", artifacts)
	}
}

func TestRenderSmartDNSProxyFallbackDirectNamesExplicitResolver(t *testing.T) {
	upstreams := []SmartDNSUpstream{
		{ID: "dns-proxy", Servers: []string{"198.51.100.53"}},
		{ID: "dns-fallback", Servers: []string{"192.0.2.53"}},
	}
	render, upstreams, plan := proxyDNSRenderPlan(t,
		[]dns.Rule{{
			ID:      "proxy-fallback",
			Domains: []string{"media.example"},
			Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy", FailureMode: dns.FailureModeFallbackDirect, FallbackUpstreamID: "dns-fallback"},
		}},
		dns.Reject(), upstreams, "node-a")
	if len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)
	endpoint := plan.Endpoints[0]
	if !strings.Contains(config, "nameserver /-.media.example/"+endpoint.Group()) {
		t.Fatalf("missing proxy nameserver:\n%s", config)
	}
	if !strings.Contains(config, "# explicit direct fallback (failure_mode=fallback_direct): SmartDNS queries both resolvers") {
		t.Fatalf("fallback comment missing:\n%s", config)
	}
	if !strings.Contains(config, "nameserver /-.media.example/dns-fallback") {
		t.Fatalf("explicit fallback upstream not rendered:\n%s", config)
	}
}

func TestRenderSmartDNSProxyMissPolicyUsesEndpoint(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, plan := proxyDNSRenderPlan(t,
		nil,
		dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"},
		upstreams, "node-a")
	if len(plan.Endpoints) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)
	if !strings.Contains(config, "nameserver /./"+plan.Endpoints[0].Group()) {
		t.Fatalf("miss policy did not use the endpoint group:\n%s", config)
	}
}

func TestValidateProxyDNSRenderMatchesRendererDecision(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, plan := proxyDNSRenderPlan(t,
		[]dns.Rule{{ID: "ok", Domains: []string{"media.example"}, Outcome: dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy"}}},
		dns.Reject(), upstreams, "node-a")
	if err := ValidateProxyDNSRender(render, ProxyDNSEndpointPlan{}); err == nil {
		t.Fatal("a proxy rule without an endpoint passed validation")
	}
	if err := ValidateProxyDNSRender(render, plan); err != nil {
		t.Fatalf("planned proxy render failed validation: %v", err)
	}

	empty := ProxyDNSEndpointPlan{}
	if err := ValidateProxyDNSRender(dns.SmartDNSRender{Rules: []dns.SmartDNSRule{{RuleID: "direct", OutcomeKind: dns.ResolverOutcomeDirect}}}, empty); err != nil {
		t.Fatalf("direct-only render failed validation: %v", err)
	}
}

func TestValidateProxyDNSEndpointRejectsNonLoopbackListen(t *testing.T) {
	endpoint := ProxyDNSEndpoint{
		ID:            "px-test",
		ProxyEgressID: "proxy-media",
		UpstreamID:    "dns-proxy",
		Protocol:      proxy.ProxyDNSEndpointUDP,
		ListenAddress: "0.0.0.0",
		Port:          ProxyDNSEndpointPortBase,
		ServerAddress: "198.51.100.53",
		ServerPort:    53,
	}
	if err := validateProxyDNSEndpoint(endpoint); err == nil {
		t.Fatal("non-loopback endpoint was accepted")
	}
	endpoint.ListenAddress = ProxyDNSEndpointListenAddress
	endpoint.ServerAddress = "127.0.0.1"
	if err := validateProxyDNSEndpoint(endpoint); err == nil {
		t.Fatal("loopback target was accepted")
	}
}

// itoa keeps the test free of strconv noise in the required substrings.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func TestRenderSmartDNSProxyMissRejectWithoutEndpointFailsClosed(t *testing.T) {
	upstreams := []SmartDNSUpstream{{ID: "dns-proxy", Servers: []string{"198.51.100.53"}}}
	render, upstreams, _ := proxyDNSRenderPlan(t,
		nil,
		dns.Outcome{Kind: dns.OutcomeProxy, ProxyEgressID: "proxy-media", UpstreamID: "dns-proxy", FailureMode: dns.FailureModeReject},
		upstreams, "node-a")
	plan := PlanProxyDNSEndpoints(dns.ProxyBindingsFromRender(render), upstreams, nil, map[string]bool{})
	config := renderProxyDNSTestConfig(t, render, upstreams, plan)
	// The fail-closed default is an explicit `/./` rule for the empty reject
	// group, never a global `address #` (which would also suppress explicit
	// rules on the shipped SmartDNS).
	for _, line := range strings.Split(config, "\n") {
		directive := strings.TrimSpace(line)
		if strings.HasPrefix(directive, "#") {
			continue
		}
		if strings.HasPrefix(directive, "address #") {
			t.Fatalf("reject miss rendered a global address rule:\n%s", config)
		}
		if strings.HasPrefix(directive, "nameserver /./") && !strings.Contains(directive, SmartDNSRejectGroup) {
			t.Fatalf("unavailable miss endpoint was rendered as a default nameserver:\n%s", config)
		}
	}
	if !strings.Contains(config, "nameserver /./"+SmartDNSRejectGroup) {
		t.Fatalf("reject miss does not select the empty reject group:\n%s", config)
	}
	if !strings.Contains(config, "reject leaves unmatched domains unresolved (SERVFAIL)") {
		t.Fatalf("reject miss does not explain its fail-closed behavior:\n%s", config)
	}
}
