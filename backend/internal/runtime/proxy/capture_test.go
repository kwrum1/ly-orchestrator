package proxy

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"testing"
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

func TestEnsureDirectFallbackIsIdempotentAndMarked(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds:  []XrayInbound{{Tag: "proxy-in", Protocol: "dokodemo-door", Settings: XrayDokodemoSettings{Network: "tcp,udp"}}},
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := EnsureDirectFallback(&config, ProxyDirectProbeURL); err != nil {
			t.Fatal(err)
		}
	}
	if len(config.Outbounds) != 2 {
		t.Fatalf("outbounds = %#v, want proxy node plus one direct fallback", config.Outbounds)
	}
	if config.Outbounds[1].Tag != ProxyDirectOutboundTag || config.Outbounds[1].Protocol != "freedom" {
		t.Fatalf("direct fallback outbound = %#v", config.Outbounds[1])
	}
	if config.Routing == nil || len(config.Routing.Balancers) != 1 {
		t.Fatalf("routing balancers = %#v", config.Routing)
	}
	balancer := config.Routing.Balancers[0]
	if balancer.FallbackTag != ProxyDirectOutboundTag || len(balancer.Selector) != 1 || balancer.Selector[0] != "node-a" {
		t.Fatalf("fallback balancer = %#v", balancer)
	}
	routed := 0
	directCatchAll := 0
	for _, rule := range config.Routing.Rules {
		if rule.BalancerTag == balancer.Tag {
			routed++
		}
		if isDirectCatchAll(rule) {
			directCatchAll++
		}
	}
	if routed != 1 || directCatchAll != 1 {
		t.Fatalf("routing rules = %#v", config.Routing.Rules)
	}
	if config.Observatory == nil || !containsString(config.Observatory.SubjectSelector, "node-a") || config.Observatory.ProbeURL != ProxyDirectProbeURL {
		t.Fatalf("observatory = %#v", config.Observatory)
	}

	ApplyOutboundSocketMark(&config, 0x200001)
	for _, index := range []int{0, 1} {
		stream := config.Outbounds[index].StreamSettings
		sockopt, _ := stream["sockopt"].(map[string]any)
		if sockopt["mark"] != uint32(0x200001) {
			// VPP owns the WAN interface. Without the proxy egress mark the
			// direct fallback would use the management/default table and fail.
			t.Fatalf("outbound %q lost the egress mark: %#v", config.Outbounds[index].Tag, sockopt)
		}
	}

	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"fallbackTag":"direct"`) {
		t.Fatalf("fallbackTag missing from rendered Xray config: %s", payload)
	}
}

func TestEnsureDirectFallbackRewritesAdaptiveLaneRules(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds: []XrayInbound{
			{Tag: "lane-a", Protocol: "dokodemo-door"},
			{Tag: "lane-b", Protocol: "dokodemo-door"},
		},
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}, {Tag: "node-b", Protocol: "vless"}},
		Routing:   &XrayRouting{Rules: []XrayRoutingRule{{Type: "field", InboundTags: []string{"lane-a"}, OutboundTag: "node-a"}, {Type: "field", InboundTags: []string{"lane-b"}, OutboundTag: "node-b"}}},
	}
	if err := EnsureDirectFallback(&config, ""); err != nil {
		t.Fatal(err)
	}
	if len(config.Routing.Balancers) != 2 {
		t.Fatalf("adaptive fallback balancers = %#v", config.Routing.Balancers)
	}
	for _, rule := range config.Routing.Rules {
		if len(rule.InboundTags) > 0 && rule.BalancerTag == "" {
			t.Fatalf("lane rule was not rewritten to a fallback balancer: %#v", rule)
		}
	}
}

func TestEnsureDirectFallbackRejectsEmptyRuntime(t *testing.T) {
	if err := EnsureDirectFallback(&XrayConfigPayload{}, ""); err == nil {
		t.Fatal("direct fallback accepted a runtime without proxy outbounds")
	}
}
