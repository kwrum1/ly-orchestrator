package service

import (
	"strings"
	"testing"
)

// The runtime owns /etc/dnsmasq.d/ly-route-dhcp.conf. Its interface must be the
// control-plane host interface of the current LAN, never a build-time name.
func TestRenderDnsmasqConfigUsesLiveLANControlInterface(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:          "lan",
		InterfaceID: "lylan-enp4s0",
		Subnet:      "192.168.88.0/24",
		Pools:       []string{"192.168.88.100-192.168.88.199"},
		Routers:     []string{"192.168.88.1"},
		NameServers: []string{"192.168.88.1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "/etc/dnsmasq.d/ly-route-dhcp.conf" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		"interface=lylan-enp4s0\n",
		"dhcp-range=set:lyroute-lan,192.168.88.100,192.168.88.199,255.255.255.0,12h\n",
		"dhcp-option=tag:lyroute-lan,3,192.168.88.1\n",
		"dhcp-option=tag:lyroute-lan,6,192.168.88.1\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq drop-in missing %q:\n%s", want, content)
		}
	}
	for _, stale := range []string{"lylan-eth2", "lylan-eth3"} {
		if strings.Contains(content, stale) {
			t.Fatalf("dnsmasq drop-in still references build-time interface %q:\n%s", stale, content)
		}
	}
}

// Moving the LAN to another port must move the rendered interface and the DHCP
// range with it. A stale file is what made dnsmasq log
// "interface lylan-eth2 does not currently exist" and answer no DHCPDISCOVER.
func TestRenderDnsmasqConfigFollowsLANPortChange(t *testing.T) {
	render := func(iface, subnet, pool, router string) string {
		t.Helper()
		artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
			ID:          "lan",
			InterfaceID: iface,
			Subnet:      subnet,
			Pools:       []string{pool},
			Routers:     []string{router},
			NameServers: []string{router},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return artifacts[0].Content
	}
	first := render("lylan-enp4s0", "192.168.88.0/24", "192.168.88.100-192.168.88.199", "192.168.88.1")
	second := render("lylan-enp5s0", "192.168.89.0/24", "192.168.89.100-192.168.89.199", "192.168.89.1")
	if !strings.Contains(first, "interface=lylan-enp4s0") || !strings.Contains(first, "192.168.88.100,192.168.88.199") {
		t.Fatalf("first LAN render is wrong:\n%s", first)
	}
	if !strings.Contains(second, "interface=lylan-enp5s0") || !strings.Contains(second, "192.168.89.100,192.168.89.199") {
		t.Fatalf("moved LAN render did not follow the live LAN:\n%s", second)
	}
	if strings.Contains(second, "lylan-enp4s0") || strings.Contains(second, "192.168.88.") {
		t.Fatalf("moved LAN render kept the previous LAN:\n%s", second)
	}
}

// No DHCP plan means the runtime must still own the drop-in and explicitly stop
// serving, instead of leaving the install-time interface/range in place.
func TestRenderDnsmasqConfigDisablesDHCPWithoutPlans(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "/etc/dnsmasq.d/ly-route-dhcp.conf" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, "# ly-route-dhcp-disabled") || !strings.Contains(content, "port=0\n") {
		t.Fatalf("disabled dnsmasq drop-in is not explicit:\n%s", content)
	}
	for _, forbidden := range []string{"interface=", "dhcp-range=", "dhcp-option=", "enable-ra"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("disabled dnsmasq drop-in still contains %q:\n%s", forbidden, content)
		}
	}
}

// A DNS-only generation listens on the LAN but must say explicitly that it
// serves no DHCP range.
func TestRenderDnsmasqConfigMarksDNSOnlyGenerationAsNoDHCP(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:          "lan-dns-0",
		InterfaceID: "lylan-enp4s0",
		ServeDNS:    true,
		DNSOnly:     true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, "# ly-route-dhcp-disabled") {
		t.Fatalf("DNS-only generation does not mark DHCP as disabled:\n%s", content)
	}
	if !strings.Contains(content, "interface=lylan-enp4s0\n") {
		t.Fatalf("DNS listener interface is missing:\n%s", content)
	}
	if strings.Contains(content, "dhcp-range=") {
		t.Fatalf("DNS-only generation still serves DHCP:\n%s", content)
	}
}
