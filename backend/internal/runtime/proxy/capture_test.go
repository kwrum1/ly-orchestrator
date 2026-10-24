package proxy

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNftablesCaptureGuardsBypassBeforeTPROXY(t *testing.T) {
	egress := NewProxyEgress("proxy-media", "xray-vpp-service")
	compiled, err := CompileEgress(egress)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindServiceNetwork(&compiled, "192.0.2.1 lyroute-wan0", []string{"192.168.88.0/24", "10.10.0.0/16"}, 1460); err != nil {
		t.Fatal(err)
	}

	plan := compiled.NftablesCapture
	net := compiled.ServiceNetwork
	if plan.IngressInterface != net.IngressHostInterface {
		t.Fatalf("capture interface = %q, want %q", plan.IngressInterface, net.IngressHostInterface)
	}
	for _, wanted := range []string{maskedPrefix(t, net.IngressCIDR), maskedPrefix(t, net.EgressCIDR), "192.168.88.0/24", "10.10.0.0/16", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"} {
		found := false
		for _, bypass := range plan.BypassDestinations {
			if bypass == wanted {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("capture bypass destinations %#v missing %q", plan.BypassDestinations, wanted)
		}
	}

	var dnsReturn, lanReturn, tcpProxy, udpProxy int = -1, -1, -1, -1
	for index, rule := range plan.Rules {
		switch {
		case strings.Contains(rule.Expression, "dport 53") && rule.Action == "counter return":
			if dnsReturn < 0 {
				dnsReturn = index
			}
		case strings.Contains(rule.Expression, "ip daddr 192.168.88.0/24") && rule.Action == "counter return":
			lanReturn = index
		case strings.Contains(rule.Expression, "meta nfproto ipv4 meta l4proto tcp") && strings.Contains(rule.Action, "tproxy ip to"):
			tcpProxy = index
		case strings.Contains(rule.Expression, "meta nfproto ipv4 meta l4proto udp") && strings.Contains(rule.Action, "tproxy ip to"):
			udpProxy = index
		}
	}
	if dnsReturn < 0 || lanReturn < 0 || tcpProxy < 0 || udpProxy < 0 {
		t.Fatalf("capture rules are incomplete: dns=%d lan=%d tcp=%d udp=%d rules=%#v", dnsReturn, lanReturn, tcpProxy, udpProxy, plan.Rules)
	}
	if !(dnsReturn < tcpProxy && lanReturn < tcpProxy && tcpProxy < udpProxy) {
		t.Fatalf("guard rules must precede TPROXY: dns=%d lan=%d tcp=%d udp=%d", dnsReturn, lanReturn, tcpProxy, udpProxy)
	}
	if !strings.Contains(plan.Rules[tcpProxy].Action, "meta mark set "+plan.Mark) {
		t.Fatalf("TPROXY rule does not apply the ingress mark: %#v", plan.Rules[tcpProxy])
	}
	for index, rule := range plan.Rules {
		if rule.Order != index+1 {
			t.Fatalf("rule order is not contiguous at %d: %#v", index, rule)
		}
	}
}

func TestNftablesCaptureKeepsAdaptiveLanesAfterRebind(t *testing.T) {
	network := ServiceNetworkForEgressID("proxy-media")
	plan := buildNftablesCapturePlan("proxy-media", network, nil)
	for _, port := range []int{21000, 21001} {
		for _, protocol := range []string{"tcp", "udp"} {
			plan.Rules = append(plan.Rules, NftablesRule{
				Chain:      ProxyCaptureChain,
				Expression: `iifname "` + network.IngressHostInterface + `" meta nfproto ipv4 meta l4proto ` + protocol,
				Action:     "counter meta mark set 0x100001 tproxy ip to :" + strconv.Itoa(port) + " accept",
			})
		}
	}
	refreshed := refreshCaptureRules(plan, network, []string{"192.168.1.0/24"})

	proxies := 0
	for _, rule := range refreshed.Rules {
		if strings.Contains(rule.Action, "tproxy") {
			proxies++
			if !strings.Contains(rule.Expression, network.IngressHostInterface) {
				t.Fatalf("preserved lane rule has the wrong interface: %#v", rule)
			}
		}
	}
	if proxies != 6 {
		t.Fatalf("terminal lane rules were not preserved: %#v", refreshed.Rules)
	}
	if !strings.Contains(strings.Join(refreshed.BypassDestinations, ","), "192.168.1.0/24") {
		t.Fatalf("rebind did not add the resolved LAN return route: %#v", refreshed.BypassDestinations)
	}
}

func maskedPrefix(t *testing.T, raw string) string {
	t.Helper()
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		t.Fatalf("invalid test prefix %q: %v", raw, err)
	}
	return prefix.Masked().String()
}

func TestFailOpenNftablesCaptureHasNoTPROXYRules(t *testing.T) {
	plan := FailOpenNftablesCapture("proxy-media")
	if !plan.FailOpen || plan.Family != ProxyCaptureFamily || plan.Table != ProxyCaptureTable {
		t.Fatalf("fail-open capture plan = %#v", plan)
	}
	if len(plan.Rules) != 0 || len(plan.Chains) != 0 {
		t.Fatalf("fail-open capture must stay empty: %#v", plan)
	}
}

func TestEnsureFailureFallbackIsIdempotentBlackholedAndMarked(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds:  []XrayInbound{{Tag: "proxy-in", Protocol: "dokodemo-door", Settings: XrayDokodemoSettings{Network: "tcp,udp"}}},
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := EnsureFailureFallback(&config, ProxyProbeURL); err != nil {
			t.Fatal(err)
		}
	}
	if len(config.Outbounds) != 2 {
		t.Fatalf("outbounds = %#v, want one proxy node plus one failure blackhole", config.Outbounds)
	}
	if config.Outbounds[1].Tag != ProxyFailureOutboundTag || config.Outbounds[1].Protocol != "blackhole" {
		t.Fatalf("failure fallback outbound = %#v", config.Outbounds[1])
	}
	for _, outbound := range config.Outbounds {
		if outbound.Tag == ProxyDirectOutboundTag || strings.EqualFold(outbound.Protocol, "freedom") {
			t.Fatalf("fail-closed config still contains a direct outbound: %#v", outbound)
		}
	}
	if config.Routing == nil || len(config.Routing.Balancers) != 1 {
		t.Fatalf("routing balancers = %#v", config.Routing)
	}
	balancer := config.Routing.Balancers[0]
	if balancer.FallbackTag != ProxyFailureOutboundTag || len(balancer.Selector) != 1 || balancer.Selector[0] != "node-a" {
		t.Fatalf("failure balancer = %#v", balancer)
	}
	routed := 0
	catchAll := 0
	for index, rule := range config.Routing.Rules {
		if rule.BalancerTag == balancer.Tag {
			routed++
		}
		if isFailureCatchAll(rule) {
			catchAll++
			if index != len(config.Routing.Rules)-1 {
				t.Fatalf("failure catch-all is not terminal: %#v", config.Routing.Rules)
			}
		}
	}
	if routed != 1 || catchAll != 1 {
		t.Fatalf("routing rules = %#v", config.Routing.Rules)
	}
	if config.Observatory == nil || !containsString(config.Observatory.SubjectSelector, "node-a") || config.Observatory.ProbeURL != ProxyProbeURL {
		t.Fatalf("observatory = %#v", config.Observatory)
	}
	if config.Metrics == nil || config.Metrics.Listen != DefaultXrayMetricsListen {
		t.Fatalf("observatory metrics are not enabled: %#v", config.Metrics)
	}

	ApplyOutboundSocketMark(&config, 0x200001)
	stream := config.Outbounds[0].StreamSettings
	sockopt, _ := stream["sockopt"].(map[string]any)
	if sockopt["mark"] != uint32(0x200001) {
		t.Fatalf("proxy outbound lost the egress mark: %#v", sockopt)
	}
	if stream := config.Outbounds[1].StreamSettings; len(stream) != 0 {
		t.Fatalf("blackhole outbound must not be socket-marked: %#v", stream)
	}

	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"fallbackTag":"failure"`, `"protocol":"blackhole"`, `"metrics":{"listen":"127.0.0.1:11111"}`} {
		if !strings.Contains(string(payload), required) {
			t.Fatalf("rendered Xray config is missing %s: %s", required, payload)
		}
	}
	if strings.Contains(string(payload), `"fallbackTag":"direct"`) || strings.Contains(string(payload), `"tag":"direct"`) {
		t.Fatalf("rendered Xray config still contains a direct path: %s", payload)
	}
	assertNoCapturedPathReachesDirect(t, config)
}

func TestEnsureFailureFallbackDropsLegacyDirectOutboundAndCatchAll(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds: []XrayInbound{{Tag: "proxy-in", Protocol: "dokodemo-door", Settings: XrayDokodemoSettings{Network: "tcp,udp"}}},
		Outbounds: []XrayOutbound{
			{Tag: "direct", Protocol: "freedom"},
			{Tag: "node-a", Protocol: "vless"},
		},
		Routing: &XrayRouting{Rules: []XrayRoutingRule{
			{Type: "field", InboundTags: []string{"proxy-in"}, OutboundTag: "node-a"},
			{Type: "field", Network: "tcp,udp", OutboundTag: "direct"},
		}},
		Observatory: &XrayObservatory{SubjectSelector: []string{"node-a", "direct"}, ProbeURL: ProxyProbeURL, ProbeInterval: "10s"},
	}
	if err := EnsureFailureFallback(&config, ""); err != nil {
		t.Fatal(err)
	}
	for _, outbound := range config.Outbounds {
		if outbound.Tag == "direct" || strings.EqualFold(outbound.Protocol, "freedom") {
			t.Fatalf("legacy direct outbound survived: %#v", config.Outbounds)
		}
	}
	for _, rule := range config.Routing.Rules {
		if rule.OutboundTag == "direct" || rule.OutboundTag == ProxyDirectOutboundTag {
			t.Fatalf("legacy direct rule survived: %#v", config.Routing.Rules)
		}
	}
	if last := config.Routing.Rules[len(config.Routing.Rules)-1]; !isFailureCatchAll(last) {
		t.Fatalf("terminal rule is not a failure catch-all: %#v", config.Routing.Rules)
	}
	for _, balancer := range config.Routing.Balancers {
		if balancer.FallbackTag != ProxyFailureOutboundTag {
			t.Fatalf("balancer %q keeps a non-failure fallback: %#v", balancer.Tag, balancer)
		}
		for _, selected := range balancer.Selector {
			if selected == "direct" {
				t.Fatalf("balancer %q still selects direct: %#v", balancer.Tag, balancer)
			}
		}
	}
}

func TestEnsureFailureFallbackRewritesAdaptiveLaneRules(t *testing.T) {
	now := time.Now().UTC()
	nodes := []Node{
		{ID: "a", Protocol: "vless", Address: "127.0.0.1", Port: 21001, Secret: "11111111-1111-1111-1111-111111111111"},
		{ID: "b", Protocol: "vless", Address: "127.0.0.1", Port: 21002, Secret: "11111111-1111-1111-1111-111111111111"},
	}
	subscription := Subscription{ID: "main", URL: "https://subscription.invalid/list", Enabled: true, NodeRefs: []string{"a", "b"}, TopN: 2}
	runtime, err := CompileAdaptiveSubscriptionLanes(subscription, nodes, []NodeProbe{
		{NodeID: "a", Reachable: true, RTT: 10 * time.Millisecond, ObservedAt: now},
		{NodeID: "b", Reachable: true, RTT: 20 * time.Millisecond, ObservedAt: now},
	}, 23080)
	if err != nil {
		t.Fatal(err)
	}
	config := XrayConfigPayload{
		Inbounds: runtime.Inbounds, Outbounds: runtime.Outbounds, Routing: &runtime.Routing,
		Observatory: &runtime.Observatory, Metrics: runtime.Metrics,
	}
	if err := EnsureFailureFallback(&config, ""); err != nil {
		t.Fatal(err)
	}
	if len(config.Routing.Balancers) != 2 {
		t.Fatalf("adaptive failure balancers = %#v", config.Routing.Balancers)
	}
	for _, rule := range config.Routing.Rules {
		if len(rule.InboundTags) > 0 && rule.BalancerTag == "" {
			t.Fatalf("lane rule was not rewritten to a failure balancer: %#v", rule)
		}
	}
	assertNoCapturedPathReachesDirect(t, config)
}

func TestEnsureFailureFallbackRejectsEmptyRuntime(t *testing.T) {
	if err := EnsureFailureFallback(&XrayConfigPayload{}, ""); err == nil {
		t.Fatal("failure fallback accepted a runtime without proxy outbounds")
	}
}

func TestEnsureFailureFallbackRejectsFreedomProxyOutbound(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds:  []XrayInbound{{Tag: "proxy-in", Protocol: "dokodemo-door"}},
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "freedom"}},
	}
	if err := EnsureFailureFallback(&config, ""); err == nil {
		t.Fatal("failure fallback accepted a freedom proxy outbound that would leak to direct")
	}
}

// assertNoCapturedPathReachesDirect walks the routing decision for every
// dokodemo-door inbound and both transports. A captured packet must either be
// black-holed directly or select a balancer whose selector only contains proxy
// outbounds and whose fallback is the failure blackhole.
func assertNoCapturedPathReachesDirect(t *testing.T, config XrayConfigPayload) {
	t.Helper()
	outboundByTag := make(map[string]XrayOutbound, len(config.Outbounds))
	hasFailure := false
	for _, outbound := range config.Outbounds {
		outboundByTag[outbound.Tag] = outbound
		if outbound.Tag == ProxyDirectOutboundTag || strings.EqualFold(outbound.Protocol, "freedom") {
			t.Fatalf("outbound %q provides a direct path", outbound.Tag)
		}
		if outbound.Tag == ProxyFailureOutboundTag && strings.EqualFold(outbound.Protocol, "blackhole") {
			hasFailure = true
		}
	}
	if !hasFailure {
		t.Fatal("config has no failure blackhole outbound")
	}
	if config.Routing == nil {
		t.Fatal("config has no routing rules")
	}
	balancers := make(map[string]XrayBalancer, len(config.Routing.Balancers))
	for _, balancer := range config.Routing.Balancers {
		balancers[balancer.Tag] = balancer
		if balancer.FallbackTag != ProxyFailureOutboundTag {
			t.Fatalf("balancer %q fallback = %q", balancer.Tag, balancer.FallbackTag)
		}
	}
	captured := 0
	for _, inbound := range config.Inbounds {
		if inbound.Protocol != "dokodemo-door" {
			continue
		}
		captured++
		for _, transport := range []string{"tcp", "udp"} {
			matched := false
			for _, rule := range config.Routing.Rules {
				if len(rule.InboundTags) > 0 && !containsString(rule.InboundTags, inbound.Tag) {
					continue
				}
				if rule.Network != "" && !strings.Contains(rule.Network, transport) {
					continue
				}
				matched = true
				if rule.BalancerTag != "" {
					balancer, exists := balancers[rule.BalancerTag]
					if !exists || len(balancer.Selector) == 0 {
						t.Fatalf("inbound %q matches unresolved balancer %q", inbound.Tag, rule.BalancerTag)
					}
					for _, tag := range balancer.Selector {
						outbound, exists := outboundByTag[tag]
						if !exists || outbound.Tag == ProxyDirectOutboundTag || strings.EqualFold(outbound.Protocol, "freedom") {
							t.Fatalf("balancer %q selects non-proxy outbound %q", balancer.Tag, tag)
						}
					}
					break
				}
				if rule.OutboundTag == ProxyFailureOutboundTag {
					break
				}
				outbound, exists := outboundByTag[rule.OutboundTag]
				if !exists || outbound.Tag == ProxyDirectOutboundTag || strings.EqualFold(outbound.Protocol, "freedom") {
					t.Fatalf("inbound %q routes to non-proxy outbound %q", inbound.Tag, rule.OutboundTag)
				}
				break
			}
			if !matched {
				t.Fatalf("captured %s traffic for inbound %q has no terminal rule", transport, inbound.Tag)
			}
		}
	}
	if captured == 0 {
		t.Fatal("test config has no capture inbound")
	}
}
