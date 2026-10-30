package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
)

func TestRenderGatewayNftablesCaptureDropsDuplicateKernelICMP(t *testing.T) {
	artifacts, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LCPGuardAddresses:     []string{"192.168.10.1", "2001:db8:10::1"},
		LCPGuardInterfaces:    []string{"ens256", "lylan-ens256"},
		LCPGuardTapInterfaces: []string{"lylan-ens256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, required := range []string{
		"chain lan_icmp_guard {",
		`iifname "ens256" ip daddr 192.168.10.1 icmp type echo-request counter drop`,
		`iifname "lylan-ens256" ip daddr 192.168.10.1 icmp type echo-request counter drop`,
		`iifname "ens256" ip daddr 192.168.10.1 icmp type echo-reply counter drop`,
		`iifname "ens256" ip6 daddr 2001:db8:10::1 icmpv6 type echo-request counter drop`,
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("ICMP guard missing %q:\n%s", required, content)
		}
	}
	// Never drop the only reply copy on the LCP tap: DPDK/AF_XDP bind the NIC
	// away from the kernel and the tap is then the only path to the host.
	for _, forbidden := range []string{
		`iifname "lylan-ens256" ip daddr 192.168.10.1 icmp type echo-reply`,
		`iifname "lylan-ens256" ip6 daddr 2001:db8:10::1 icmpv6 type echo-reply`,
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("ICMP guard drops the DPDK-only reply copy %q:\n%s", forbidden, content)
		}
	}
}

func TestRenderGatewayNftablesCaptureFailsOpenWithoutGuardAddresses(t *testing.T) {
	// A tap name without an address must not install a drop: the guard is
	// deliberately fail-open so a partial plan cannot blackhole LAN ICMP.
	artifacts, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LCPGuardTapInterfaces: []string{"lylan-ens256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if strings.Contains(content, "counter drop") || strings.Contains(content, "lan_icmp_guard") {
		t.Fatalf("tap without an address rendered a drop:\n%s", content)
	}
}
