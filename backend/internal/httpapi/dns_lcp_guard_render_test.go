package httpapi

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func TestGatewayNftablesRenderedForDNSLCPGuardWithoutProxyCapture(t *testing.T) {
	guard := dnsLCPGuardPlan([]vpp.AddressAssignment{{
		Role:           "lan",
		CIDR:           "192.168.77.2/24",
		LinuxInterface: "enp0s4",
		VPPInterface:   "lyroute-enp0s4",
	}}, nil)
	if !gatewayNftablesNeeded(proxy.NftablesCapturePlan{}, guard) {
		t.Fatal("DNS LCP guard without proxy capture must render the nftables artifact")
	}
	artifacts, err := service.RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, guard)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "/etc/nftables.conf" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	for _, want := range []string{
		"table inet ly_route_dns_capture",
		`iifname "enp0s4" ip daddr 192.168.77.2 udp dport 53 counter drop`,
		`iifname "enp0s4" ip daddr 192.168.77.2 tcp dport 53 counter drop`,
		`iifname "lylan-enp0s4" ip daddr 192.168.77.2 udp dport 53 counter drop`,
		`iifname "lylan-enp0s4" ip daddr 192.168.77.2 tcp dport 53 counter drop`,
	} {
		if !strings.Contains(artifacts[0].Content, want) {
			t.Fatalf("nftables artifact lacks %q:\n%s", want, artifacts[0].Content)
		}
	}
}

func TestGatewayNftablesSkippedWithoutProxyCaptureOrDNSGuard(t *testing.T) {
	if gatewayNftablesNeeded(proxy.NftablesCapturePlan{}, service.DNSInterceptionPlan{}) {
		t.Fatal("empty capture and guard must not render nftables")
	}
}
