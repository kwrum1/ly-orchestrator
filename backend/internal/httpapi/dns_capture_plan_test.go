package httpapi

import (
	"net/netip"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

// Two current LANs must both appear in the capture generation, with UDP and TCP
// 53 redirects and explicit bypasses for the gateway addresses, the LAN
// prefixes and the proxy/DNS service peers.
func TestDNSCapturePlanCoversEveryLANAndBypassesServices(t *testing.T) {
	assignments := []vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", CIDR: "192.168.88.1/24"},
		{Role: "lan", LinuxInterface: "enp5s0", VPPInterface: "lyroute-enp5s0", CIDR: "192.168.89.1/24"},
		{Role: "wan", LinuxInterface: "enp6s0", VPPInterface: "lyroute-enp6s0", CIDR: "203.0.113.2/30"},
	}
	dnsNetwork, err := vpp.DNSServiceNetworkForUpstreamID("dns-upstream-a", "wan-a", []string{"8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	proxyNetwork := proxy.ProxyServiceNetwork{
		IngressCIDR: "198.18.11.0/30",
		EgressCIDR:  "198.18.11.4/30",
	}
	dnsPrefix, err := netip.ParsePrefix(dnsNetwork.CIDR)
	if err != nil {
		t.Fatal(err)
	}
	plan := dnsCapturePlan(assignments, []vpp.DNSServiceNetwork{dnsNetwork}, proxyNetwork, false)
	if !plan.LocalResolver || plan.ListenPort != 53 {
		t.Fatalf("capture plan = %#v", plan)
	}
	artifacts, err := service.RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, plan)
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		`iifname "enp4s0" udp dport 53 counter redirect to :53`,
		`iifname "enp4s0" tcp dport 53 counter redirect to :53`,
		`iifname "lylan-enp4s0" udp dport 53 counter redirect to :53`,
		`iifname "lylan-enp4s0" tcp dport 53 counter redirect to :53`,
		`iifname "enp5s0" udp dport 53 counter redirect to :53`,
		`iifname "enp5s0" tcp dport 53 counter redirect to :53`,
		`iifname "lylan-enp5s0" udp dport 53 counter redirect to :53`,
		`iifname "lylan-enp5s0" tcp dport 53 counter redirect to :53`,
		`ip daddr 192.168.88.1 udp dport 53 counter return`,
		`ip daddr 192.168.89.1 tcp dport 53 counter return`,
		`ip daddr 192.168.88.0/24 udp dport 53 counter return`,
		`ip daddr 192.168.89.0/24 tcp dport 53 counter return`,
		`ip daddr 198.18.11.0/30 udp dport 53 counter return`,
		`ip daddr ` + dnsPrefix.Masked().String() + ` udp dport 53 counter return`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("capture missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "enp3s0") || strings.Contains(content, "lylan-eth2") {
		t.Fatalf("capture references a LAN that is no longer configured:\n%s", content)
	}
}

// The single-LAN VPP path keeps the plugin (and its ICMP guard) but the nft
// capture layer must still own LAN DNS so a missing host-stack session cannot
// black-hole queries.
func TestDNSCapturePlanSingleLANKeepsTransparentGuards(t *testing.T) {
	assignments := []vpp.AddressAssignment{
		{Role: "lan", LinuxInterface: "enp4s0", VPPInterface: "lyroute-enp4s0", CIDR: "192.168.88.1/24"},
	}
	plan := dnsCapturePlan(assignments, nil, proxy.ProxyServiceNetwork{}, true)
	if len(plan.LCPGuardAddresses) != 1 || plan.LCPGuardAddresses[0] != "192.168.88.1" {
		t.Fatalf("guard addresses = %#v", plan.LCPGuardAddresses)
	}
	if !plan.LocalResolver {
		t.Fatal("the capture plan must know dnsmasq serves the LAN")
	}
	artifacts, err := service.RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, plan)
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, `iifname "lylan-enp4s0" udp dport 53 counter redirect to :53`) {
		t.Fatalf("single LAN is not captured:\n%s", content)
	}
	if !strings.Contains(content, "chain lan_icmp_guard") {
		t.Fatalf("ICMP duplicate guard is missing:\n%s", content)
	}
	if strings.Contains(content, "chain dns_lcp_guard") {
		t.Fatalf("DNS drop guard contradicts the local resolver:\n%s", content)
	}
}

func TestGatewayNftablesNeededForLANCapture(t *testing.T) {
	if !gatewayNftablesNeeded(proxy.NftablesCapturePlan{}, service.DNSInterceptionPlan{LANInterfaces: []string{"lylan-enp4s0"}}) {
		t.Fatal("a LAN capture plan must render the nftables artifact without proxy capture")
	}
}
