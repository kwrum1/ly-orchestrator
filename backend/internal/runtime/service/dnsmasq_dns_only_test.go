package service

import (
	"strings"
	"testing"
)

// A multi-LAN plan needs a dnsmasq listener on every LAN gateway address: the
// nftables redirect targets the gateway address and SmartDNS only binds the
// loopback stub. A DNS-only plan must not require a DHCP range.
func TestRenderDnsmasqConfigDNSOnlyPlanListensOnLAN(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:          "lan-dns-0",
		InterfaceID: "lylan-ens192",
		ServeDNS:    true,
		DNSOnly:     true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		"port=53\n",
		"bind-dynamic\n",
		"server=127.0.0.1#1053\n",
		"interface=lylan-ens192\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq DNS-only config missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "dhcp-range=") {
		t.Fatalf("DNS-only plan rendered a DHCP range:\n%s", content)
	}
}
