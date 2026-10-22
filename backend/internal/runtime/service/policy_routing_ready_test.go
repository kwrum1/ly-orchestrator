package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/proxy"
)

func TestRenderProxyServiceRoutingMarksReadyForBootRetry(t *testing.T) {
	compiled, err := proxy.CompileEgress(proxy.NewProxyEgress("proxy-media", "xray-vpp-service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.BindServiceNetwork(&compiled, "203.0.113.1 lyroute-wan0", []string{"192.168.88.0/24"}, 1500); err != nil {
		t.Fatal(err)
	}
	artifacts, err := RenderLinuxPolicyRouting(compiled.LinuxPolicyRouting, nat.BehaviorEndpointDependent)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	for _, wanted := range []string{
		"printf 'ready\\n' > /run/ly-route/proxy-policy-ready",
		"rm -f /run/ly-route/proxy-policy-ready",
	} {
		if !strings.Contains(content, wanted) {
			t.Fatalf("policy routing script is missing %q:\n%s", wanted, content)
		}
	}
	// The ready marker must only be written after the TPROXY policy rules and
	// the marked egress table are proven, otherwise systemd would stop retrying
	// a half-installed boot generation.
	marker := strings.Index(content, "> /run/ly-route/proxy-policy-ready")
	rule := strings.Index(content, "ip rule add fwmark")
	egressRoute := strings.Index(content, "ip route replace default via")
	if marker < 0 || rule < 0 || egressRoute < 0 || marker < rule || marker < egressRoute {
		t.Fatalf("ready marker is not after the policy rules:\n%s", content)
	}
}
