package service

import (
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
)

func TestRenderGatewayNftablesCaptureFailOpenFlushesWithoutTPROXY(t *testing.T) {
	artifacts, err := RenderGatewayNftablesCapture(proxy.FailOpenNftablesCapture("proxy-media"), DNSInterceptionPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	for _, wanted := range []string{
		"add table inet ly_route_proxy_capture",
		"flush table inet ly_route_proxy_capture",
		"# ly-route-proxy-fail-open:",
	} {
		if !strings.Contains(content, wanted) {
			t.Fatalf("fail-open nftables content missing %q:\n%s", wanted, content)
		}
	}
	if strings.Contains(content, "tproxy") || strings.Contains(content, "meta mark set") {
		t.Fatalf("fail-open capture still contains interception rules:\n%s", content)
	}
	family, table, rules, err := expectedNftablesRules(content)
	if err != nil {
		t.Fatal(err)
	}
	if family != "inet" || table != "ly_route_proxy_capture" || len(rules) != 0 {
		t.Fatalf("fail-open readback expectation = %s %s %#v", family, table, rules)
	}
}

func TestGeneratedCaptureReadbackMatchesLiveNftShape(t *testing.T) {
	compiled, err := proxy.CompileEgress(proxy.NewProxyEgress("proxy-media", "xray-vpp-service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.BindServiceNetwork(&compiled, "192.0.2.1 lyroute-wan0", []string{"192.168.88.0/24"}, 1500); err != nil {
		t.Fatal(err)
	}
	artifacts, err := RenderNftablesCapture(compiled.NftablesCapture)
	if err != nil {
		t.Fatal(err)
	}
	family, table, rules, err := expectedNftablesRules(artifacts[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if family != "inet" || table != "ly_route_proxy_capture" || len(rules) < 4 {
		t.Fatalf("generated readback expectation = %s %s %#v", family, table, rules)
	}
	// This is the exact shape `nft list table` prints for the generated file
	// (priority is rendered as -150, live prints mangle; counter gains stats).
	live := `table inet ly_route_proxy_capture {
  chain proxy_prerouting {
    type filter hook prerouting priority mangle; policy accept;
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" meta mark ` + compiled.NftablesCapture.Mark + ` counter packets 0 bytes 0 return
    iifname != "` + compiled.ServiceNetwork.IngressHostInterface + `" counter packets 0 bytes 0 return
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" meta nfproto ipv4 tcp dport 53 counter packets 0 bytes 0 return
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" meta nfproto ipv4 udp dport 53 counter packets 0 bytes 0 return
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" ip daddr 192.168.88.0/24 counter packets 0 bytes 0 return
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" meta nfproto ipv4 meta l4proto tcp counter packets 0 bytes 0 meta mark set ` + compiled.NftablesCapture.Mark + ` tproxy ip to :` + strconv.Itoa(compiled.NftablesCapture.TargetPort) + ` accept
    iifname "` + compiled.ServiceNetwork.IngressHostInterface + `" meta nfproto ipv4 meta l4proto udp counter packets 0 bytes 0 meta mark set ` + compiled.NftablesCapture.Mark + ` tproxy ip to :` + strconv.Itoa(compiled.NftablesCapture.TargetPort) + ` accept
  }
}`
	normalized := normalizedNftables(live)
	position := -1
	for _, rule := range rules {
		next := nftablesRulePosition(normalized, rule, position)
		if next < 0 {
			t.Fatalf("generated rule %q is not found in live nft output", rule)
		}
		position = next
	}
}

func TestExpectedNftablesRulesRejectsFailOpenOutsideManagedTable(t *testing.T) {
	content := "add table inet other_capture\nflush table inet other_capture\n# ly-route-proxy-fail-open: empty\ntable inet other_capture {\n}\n"
	if _, _, _, err := expectedNftablesRules(content); err == nil {
		t.Fatal("fail-open marker was accepted for an unmanaged table")
	}
}
