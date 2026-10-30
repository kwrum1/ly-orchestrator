package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestObservatoryProbeURLPrefersEnvironmentThenDefault(t *testing.T) {
	t.Setenv(ProxyProbeURLEnv, "https://1.1.1.1")
	if got := ObservatoryProbeURL(); got != "https://1.1.1.1" {
		t.Fatalf("probe URL = %q, want environment override", got)
	}
	t.Setenv(ProxyProbeURLEnv, " ftp://not-http.example/probe ")
	if got := ObservatoryProbeURL(); got != ProxyProbeURL {
		t.Fatalf("invalid environment override was accepted: %q", got)
	}
	t.Setenv(ProxyProbeURLEnv, "")
	if got := ObservatoryProbeURL(); got != ProxyProbeURL {
		t.Fatalf("empty environment override = %q, want default %q", got, ProxyProbeURL)
	}
}

func TestObservatoryProbeURLGroupOverrideWinsOverEnvironment(t *testing.T) {
	t.Setenv(ProxyProbeURLEnv, "https://env.example/probe")
	group := NodeGroup{ProbeURL: "http://group.example/probe"}
	if got := ObservatoryProbeURLForGroup(group); got != "http://group.example/probe" {
		t.Fatalf("group override = %q", got)
	}
	group.ProbeURL = "ftp://group.example/probe"
	if got := ObservatoryProbeURLForGroup(group); got != "https://env.example/probe" {
		t.Fatalf("invalid group override did not fall back to env: %q", got)
	}
}

func TestValidateObservatoryProbeURL(t *testing.T) {
	for _, raw := range []string{"http://www.baidu.com", "https://1.1.1.1", "https://example.com/generate_204"} {
		if err := ValidateObservatoryProbeURL(raw); err != nil {
			t.Fatalf("valid probe %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "ftp://example.com", "//example.com", "https://user:pass@example.com"} {
		if err := ValidateObservatoryProbeURL(raw); err == nil {
			t.Fatalf("invalid probe %q accepted", raw)
		}
	}
	if !strings.Contains(ValidateObservatoryProbeURL("ftp://example.com").Error(), "http") {
		t.Fatal("validation error does not explain the accepted schemes")
	}
}

func TestEnsureFailureFallbackUsesProbeEnvironmentByDefault(t *testing.T) {
	t.Setenv(ProxyProbeURLEnv, "https://1.1.1.1")
	config := XrayConfigPayload{
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}},
		Routing:   &XrayRouting{Rules: []XrayRoutingRule{{Type: "field", InboundTags: []string{"capture"}, OutboundTag: "node-a"}}},
	}
	if err := EnsureFailureFallback(&config, ""); err != nil {
		t.Fatal(err)
	}
	if config.Observatory == nil || config.Observatory.ProbeURL != "https://1.1.1.1" {
		t.Fatalf("observatory = %#v, want environment probe URL", config.Observatory)
	}
}

func TestAdaptiveObservatoryUsesProbeEnvironment(t *testing.T) {
	t.Setenv(ProxyProbeURLEnv, "http://www.baidu.com")
	nodes := []Node{
		{ID: "a", Protocol: "vless", Address: "127.0.0.1", Port: 21001, Secret: "11111111-1111-1111-1111-111111111111"},
		{ID: "b", Protocol: "vless", Address: "127.0.0.1", Port: 21002, Secret: "22222222-2222-2222-2222-222222222222"},
	}
	now := time.Now().UTC()
	runtime, err := CompileAdaptiveSubscriptionLanes(
		Subscription{ID: "main", URL: "https://subscription.invalid/list", Enabled: true, NodeRefs: []string{"a", "b"}, TopN: 2},
		nodes,
		[]NodeProbe{{NodeID: "a", Reachable: true, RTT: 10 * time.Millisecond, ObservedAt: now}, {NodeID: "b", Reachable: true, RTT: 20 * time.Millisecond, ObservedAt: now}},
		23080,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Observatory.ProbeURL != "http://www.baidu.com" {
		t.Fatalf("adaptive probe URL = %q", runtime.Observatory.ProbeURL)
	}
}
