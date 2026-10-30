package proxy

import "testing"

func groupTestNodes() []Node {
	return []Node{
		{ID: "a", Protocol: "vless", Address: "198.51.100.10", Port: 443, Secret: "11111111-1111-1111-1111-111111111111"},
		{ID: "b", Protocol: "vless", Address: "198.51.100.11", Port: 443, Secret: "22222222-2222-2222-2222-222222222222"},
	}
}

func TestCompileNodeGroupLatencyKeepsLeastPingAndFailClosedFallback(t *testing.T) {
	outbounds, balancer, err := CompileNodeGroup(NodeGroup{ID: "hk", Members: []string{"a", "b"}, Strategy: GroupStrategyLatency, Enabled: true}, groupTestNodes())
	if err != nil {
		t.Fatalf("compile group: %v", err)
	}
	if len(outbounds) != 2 || outbounds[0].Tag != "node-a" || outbounds[1].Tag != "node-b" {
		t.Fatalf("outbounds = %#v", outbounds)
	}
	if balancer.Strategy.Type != "leastPing" {
		t.Fatalf("latency balancer strategy = %q, want leastPing", balancer.Strategy.Type)
	}
	config := XrayConfigPayload{
		Outbounds: outbounds,
		Routing: &XrayRouting{
			Balancers: []XrayBalancer{balancer},
			Rules:     []XrayRoutingRule{{Type: "field", InboundTags: []string{"capture"}, BalancerTag: balancer.Tag}},
		},
	}
	if err := EnsureFailureFallback(&config, ProxyProbeURL); err != nil {
		t.Fatalf("failure fallback: %v", err)
	}
	if len(config.Routing.Balancers) != 1 {
		t.Fatalf("balancers after fallback = %#v", config.Routing.Balancers)
	}
	kept := config.Routing.Balancers[0]
	if kept.Tag != balancer.Tag || kept.FallbackTag != ProxyFailureOutboundTag {
		t.Fatalf("balancer not fail-closed: %#v", kept)
	}
	if len(kept.Selector) != 2 {
		t.Fatalf("selector = %#v", kept.Selector)
	}
	if config.Observatory == nil {
		t.Fatal("latency group lost the observatory")
	}
	last := config.Routing.Rules[len(config.Routing.Rules)-1]
	if last.OutboundTag != ProxyFailureOutboundTag {
		t.Fatalf("terminal rule = %#v", last)
	}
}

func TestCompileNodeGroupLoadBalanceUsesRoundRobin(t *testing.T) {
	_, balancer, err := CompileNodeGroup(NodeGroup{ID: "lb", Members: []string{"b", "a"}, Strategy: GroupStrategyLoadBalance, Enabled: true}, groupTestNodes())
	if err != nil {
		t.Fatalf("compile group: %v", err)
	}
	if balancer.Strategy.Type != "roundRobin" {
		t.Fatalf("loadbalance strategy = %q, want roundRobin", balancer.Strategy.Type)
	}
	if len(balancer.Selector) != 2 || balancer.Selector[0] != "node-b" || balancer.Selector[1] != "node-a" {
		t.Fatalf("member order not preserved: %#v", balancer.Selector)
	}
}

func TestCompileNodeGroupSingleMemberDegradesToDirectOutbound(t *testing.T) {
	outbounds, balancer, err := CompileNodeGroup(NodeGroup{ID: "one", Members: []string{"a"}, Strategy: GroupStrategyLatency, Enabled: true}, groupTestNodes())
	if err != nil {
		t.Fatalf("compile group: %v", err)
	}
	config := XrayConfigPayload{
		Outbounds: outbounds,
		Routing: &XrayRouting{
			Balancers: []XrayBalancer{balancer},
			Rules:     []XrayRoutingRule{{Type: "field", InboundTags: []string{"capture"}, BalancerTag: balancer.Tag}},
		},
	}
	if err := EnsureFailureFallback(&config, ProxyProbeURL); err != nil {
		t.Fatalf("failure fallback: %v", err)
	}
	if len(config.Routing.Balancers) != 0 {
		t.Fatalf("single member kept a health gate: balancers=%#v", config.Routing.Balancers)
	}
	if config.Observatory == nil || config.Metrics == nil || config.API == nil {
		t.Fatalf("single member lost observability: observatory=%#v metrics=%#v api=%#v", config.Observatory, config.Metrics, config.API)
	}
	if config.Metrics.Listen != DefaultXrayMetricsListen {
		t.Fatalf("metrics listener = %q, want loopback %q", config.Metrics.Listen, DefaultXrayMetricsListen)
	}
	captured := 0
	for _, rule := range config.Routing.Rules {
		if len(rule.InboundTags) > 0 && containsString(rule.InboundTags, "capture") {
			captured++
			if rule.OutboundTag != "node-a" {
				t.Fatalf("single member rule = %#v", rule)
			}
		}
	}
	if captured != 1 {
		t.Fatalf("capture inbound was not routed to the node: %#v", config.Routing.Rules)
	}
}

func TestEnsureFailureFallbackObservabilityIsIdempotent(t *testing.T) {
	config := XrayConfigPayload{
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}},
		Routing:   &XrayRouting{Rules: []XrayRoutingRule{{Type: "field", InboundTags: []string{"capture"}, OutboundTag: "node-a"}}},
	}
	for iteration := 0; iteration < 2; iteration++ {
		if err := EnsureFailureFallback(&config, ProxyProbeURL); err != nil {
			t.Fatalf("fallback iteration %d: %v", iteration, err)
		}
	}
	if config.Metrics == nil || config.Metrics.Listen != DefaultXrayMetricsListen {
		t.Fatalf("metrics = %#v, want loopback %q", config.Metrics, DefaultXrayMetricsListen)
	}
	if config.Observatory == nil || len(config.Observatory.SubjectSelector) != 1 || config.Observatory.SubjectSelector[0] != "node-a" {
		t.Fatalf("observatory = %#v", config.Observatory)
	}
	if config.API == nil || config.API.Tag != XrayRoutingAPITag {
		t.Fatalf("routing API = %#v", config.API)
	}
	inboundCount := 0
	for _, inbound := range config.Inbounds {
		if inbound.Tag == XrayRoutingAPIInbound {
			inboundCount++
			if inbound.Listen != "127.0.0.1" || inbound.Port != 10085 {
				t.Fatalf("routing API inbound is not loopback-only: %#v", inbound)
			}
		}
	}
	if inboundCount != 1 {
		t.Fatalf("routing API inbounds = %d, want 1", inboundCount)
	}
	apiRules := 0
	for _, rule := range config.Routing.Rules {
		if len(rule.InboundTags) == 1 && rule.InboundTags[0] == XrayRoutingAPIInbound {
			apiRules++
		}
	}
	if apiRules != 1 {
		t.Fatalf("routing API rules = %d, want 1", apiRules)
	}
}

func TestCompileNodeGroupRejectsMissingMemberAndBadStrategy(t *testing.T) {
	if _, _, err := CompileNodeGroup(NodeGroup{ID: "x", Members: []string{"missing"}, Strategy: GroupStrategyLatency, Enabled: true}, groupTestNodes()); err == nil {
		t.Fatal("missing member was accepted")
	}
	if _, _, err := CompileNodeGroup(NodeGroup{ID: "x", Members: []string{"a"}, Strategy: "fastest", Enabled: true}, groupTestNodes()); err == nil {
		t.Fatal("unsupported strategy was accepted")
	}
	if _, _, err := CompileNodeGroup(NodeGroup{ID: "x", Members: nil, Strategy: GroupStrategyLatency, Enabled: true}, groupTestNodes()); err == nil {
		t.Fatal("empty group was accepted")
	}
}

func TestCompileNodeGroupSkipsRetiredMembers(t *testing.T) {
	outbounds, balancer, err := CompileNodeGroup(NodeGroup{ID: "hk", Members: []string{"a", "retired", "b"}, Strategy: GroupStrategyLatency, Enabled: true}, groupTestNodes())
	if err != nil {
		t.Fatalf("compile group with retired member: %v", err)
	}
	if len(outbounds) != 2 || len(balancer.Selector) != 2 {
		t.Fatalf("retired member was not skipped: outbounds=%#v selector=%#v", outbounds, balancer.Selector)
	}
}
