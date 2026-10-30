package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
)

// Every current LAN must be redirected for both UDP and TCP port 53, and the
// bypass rules must precede the redirects so a query to the gateway itself, a
// LAN host, a proxy/DNS service peer or loopback is never captured.
func TestRenderGatewayNftablesCaptureRedirectsLAN53WithBypasses(t *testing.T) {
	artifacts, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LANInterfaces:   []string{"enp4s0", "lylan-enp4s0", "enp5s0", "lylan-enp5s0"},
		BypassAddresses: []string{"192.168.88.1", "192.168.89.1"},
		BypassPrefixes:  []string{"192.168.88.0/24", "192.168.89.0/24", "198.18.11.0/30"},
		LocalResolver:   true,
		ListenPort:      53,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "/etc/nftables.conf" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	for _, iface := range []string{"enp4s0", "lylan-enp4s0", "enp5s0", "lylan-enp5s0"} {
		for _, protocol := range []string{"udp", "tcp"} {
			want := `iifname "` + iface + `" ` + protocol + ` dport 53 counter redirect to :53`
			if !strings.Contains(content, want) {
				t.Fatalf("capture missing %q:\n%s", want, content)
			}
		}
	}
	for _, want := range []string{
		`iifname "lo" udp dport 53 counter return`,
		`ip daddr 192.168.88.1 udp dport 53 counter return`,
		`ip daddr 192.168.89.1 tcp dport 53 counter return`,
		`ip daddr 192.168.88.0/24 udp dport 53 counter return`,
		`ip daddr 192.168.89.0/24 tcp dport 53 counter return`,
		`ip daddr 198.18.11.0/30 udp dport 53 counter return`,
		`ip daddr 127.0.0.0/8 udp dport 53 counter return`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("capture missing bypass %q:\n%s", want, content)
		}
	}
	bypass := strings.Index(content, `ip daddr 198.18.11.0/30 udp dport 53 counter return`)
	redirect := strings.Index(content, `iifname "enp4s0" udp dport 53 counter redirect to :53`)
	if bypass < 0 || redirect < 0 || bypass >= redirect {
		t.Fatalf("bypass rules do not precede the redirects:\n%s", content)
	}
	if strings.Contains(content, "chain dns_lcp_guard") {
		t.Fatalf("a local resolver must not install the DNS drop guard:\n%s", content)
	}
	if _, _, _, err := expectedNftablesRules(content); err != nil {
		t.Fatalf("rendered capture is not readable: %v", err)
	}
}

// The single-LAN VPP path keeps the ICMP duplicate guard but must not drop the
// DNS copy now that dnsmasq listens on the LAN address.
func TestRenderGatewayNftablesCaptureKeepsICMPGuardWithLocalResolver(t *testing.T) {
	artifacts, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LANInterfaces:         []string{"lylan-enp4s0"},
		LCPGuardAddresses:     []string{"192.168.88.1"},
		LCPGuardInterfaces:    []string{"enp4s0", "lylan-enp4s0"},
		LCPGuardTapInterfaces: []string{"lylan-enp4s0"},
		LocalResolver:         true,
		ListenPort:            53,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, "chain lan_icmp_guard") {
		t.Fatalf("ICMP duplicate guard is missing:\n%s", content)
	}
	if strings.Contains(content, `ip daddr 192.168.88.1 udp dport 53 counter drop`) {
		t.Fatalf("LAN DNS is still dropped instead of answered locally:\n%s", content)
	}
}

func TestRenderGatewayNftablesCaptureRejectsInvalidBypass(t *testing.T) {
	if _, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LANInterfaces:  []string{"lylan-enp4s0"},
		BypassPrefixes: []string{"not-a-prefix"},
	}); err == nil {
		t.Fatal("invalid bypass prefix was accepted")
	}
	if _, err := RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, DNSInterceptionPlan{
		LANInterfaces:   []string{"lylan-enp4s0"},
		BypassAddresses: []string{"0.0.0.0"},
	}); err == nil {
		t.Fatal("unspecified bypass address was accepted")
	}
}
