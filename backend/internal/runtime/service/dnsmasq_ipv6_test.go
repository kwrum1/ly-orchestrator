package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderDnsmasqConfigPPPoEDelegatedPrefixAddsRAAndDHCPv6(t *testing.T) {
	lifetime := 0
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:               "ipv6-lan0",
		InterfaceID:      "lylan-eth2",
		IPv6Prefix:       "2001:db8:100::/56",
		IPv6NameServers:  []string{"2001:db8:100::53"},
		RAInterval:       30,
		RARouterLifetime: &lifetime,
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		"port=0\n",
		"enable-ra\n",
		"interface=lylan-eth2\n",
		"dhcp-range=set:lyroute-ipv6-lan0,2001:db8:100::,ra-stateless,64,12h\n",
		"dhcp-option=tag:lyroute-ipv6-lan0,option6:dns-server,[2001:db8:100::53]\n",
		"ra-param=lylan-eth2,30,0\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq IPv6 config missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "radvd") {
		t.Fatalf("dnsmasq config must not depend on radvd:\n%s", content)
	}
}

func TestRenderDnsmasqConfigStaticIPv6Prefix(t *testing.T) {
	lifetime := 1800
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:               "lan1",
		InterfaceID:      "lylan-eth3",
		Subnet:           "192.168.50.0/24",
		Pools:            []string{"192.168.50.100-192.168.50.199"},
		Routers:          []string{"192.168.50.1"},
		NameServers:      []string{"192.168.50.1"},
		IPv6Prefix:       "2001:db8:50::1/64",
		RAInterval:       60,
		RARouterLifetime: &lifetime,
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		"dhcp-range=set:lyroute-lan1,192.168.50.100,192.168.50.199,255.255.255.0,12h",
		"dhcp-range=set:lyroute-lan1,2001:db8:50::,ra-stateless,64,12h",
		"dhcp-option=tag:lyroute-lan1,option6:dns-server,[2001:db8:50::1]",
		"ra-param=lylan-eth3,60,1800",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dnsmasq static IPv6 config missing %q:\n%s", want, content)
		}
	}
}

func TestRenderDnsmasqConfigStatefulIPv6UsesDeterministicPool(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:          "lan-stateful",
		InterfaceID: "lylan-eth4",
		IPv6Prefix:  "2001:db8:60::/64",
		IPv6Mode:    "stateful",
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, "dhcp-range=set:lyroute-lan-stateful,2001:db8:60::100,2001:db8:60::1ff,64,12h") {
		t.Fatalf("stateful DHCPv6 pool is wrong:\n%s", content)
	}
	if !strings.Contains(content, "option6:dns-server,[2001:db8:60::1]") {
		t.Fatalf("stateful DHCPv6 DNS option is wrong:\n%s", content)
	}
}

func TestRenderDnsmasqConfigWithoutIPv6PrefixHasNoRAOrDHCPv6(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{{
		ID:          "lan",
		InterfaceID: "lylan-eth2",
		Subnet:      "192.168.88.0/24",
		Pools:       []string{"192.168.88.100-192.168.88.199"},
		Routers:     []string{"192.168.88.1"},
		NameServers: []string{"192.168.88.1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, forbidden := range []string{"enable-ra", "dhcp-range=set:lyroute-lan,2001:", "option6:", "ra-param="} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("no-prefix dnsmasq config contains %q:\n%s", forbidden, content)
		}
	}
	if !strings.Contains(content, "dhcp-range=set:lyroute-lan,192.168.88.100,192.168.88.199,255.255.255.0,12h") {
		t.Fatalf("IPv4 DHCP changed when IPv6 is absent:\n%s", content)
	}
}

func TestRenderDnsmasqConfigMultipleLANs(t *testing.T) {
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{
		{ID: "lan-a", InterfaceID: "lylan-eth2", IPv6Prefix: "2001:db8:100::/64"},
		{ID: "lan-b", InterfaceID: "lylan-eth3", IPv6Prefix: "2001:db8:101::/64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	for _, want := range []string{
		"interface=lylan-eth2\n",
		"interface=lylan-eth3\n",
		"dhcp-range=set:lyroute-lan-a,2001:db8:100::,ra-stateless,64,12h",
		"dhcp-range=set:lyroute-lan-b,2001:db8:101::,ra-stateless,64,12h",
		"ra-param=lylan-eth2,30,1800",
		"ra-param=lylan-eth3,30,1800",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("multi-LAN config missing %q:\n%s", want, content)
		}
	}
	if strings.Count(content, "enable-ra\n") != 1 {
		t.Fatalf("enable-ra must be emitted exactly once:\n%s", content)
	}
}

func TestRenderDnsmasqConfigRejectsInvalidIPv6State(t *testing.T) {
	bad := []struct {
		name string
		plan DnsmasqPlan
	}{
		{name: "prefix too small", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "2001:db8:1::/65"}},
		{name: "malformed prefix", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "not-a-prefix"}},
		{name: "ipv4 prefix", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "192.0.2.0/24"}},
		{name: "unspecified prefix", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "::/64"}},
		{name: "bad mode", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "2001:db8:1::/64", IPv6Mode: "automatic"}},
		{name: "ipv4 DNS", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2", IPv6Prefix: "2001:db8:1::/64", IPv6NameServers: []string{"192.0.2.1"}}},
		{name: "unsafe interface", plan: DnsmasqPlan{ID: "x", InterfaceID: "lylan-eth2\nrestart", IPv6Prefix: "2001:db8:1::/64"}},
	}
	for _, test := range bad {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RenderDnsmasqConfig([]DnsmasqPlan{test.plan}); err == nil {
				t.Fatalf("invalid IPv6 plan was accepted: %#v", test.plan)
			}
		})
	}
}

func TestDelegatedLANPrefixAllocatesDeterministic64(t *testing.T) {
	tests := []struct {
		delegated string
		index     int
		want      string
		wantErr   bool
	}{
		{delegated: "2001:db8:100::/56", index: 0, want: "2001:db8:100::/64"},
		{delegated: "2001:db8:100::/56", index: 1, want: "2001:db8:100:1::/64"},
		{delegated: "2001:db8:100::/56", index: 255, want: "2001:db8:100:ff::/64"},
		{delegated: "2001:db8:100::/56", index: 256, wantErr: true},
		{delegated: "2001:db8:100::/64", index: 0, want: "2001:db8:100::/64"},
		{delegated: "2001:db8:100::/64", index: 1, wantErr: true},
		{delegated: "2001:db8:100::/60", index: 15, want: "2001:db8:100:f::/64"},
		{delegated: "2001:db8:100::/65", index: 0, wantErr: true},
		{delegated: "not-a-prefix", index: 0, wantErr: true},
	}
	for _, test := range tests {
		got, err := DelegatedLANPrefix(test.delegated, test.index)
		if test.wantErr {
			if err == nil {
				t.Fatalf("DelegatedLANPrefix(%q, %d) = %s, want error", test.delegated, test.index, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("DelegatedLANPrefix(%q, %d): %v", test.delegated, test.index, err)
		}
		if got.String() != test.want {
			t.Fatalf("DelegatedLANPrefix(%q, %d) = %s, want %s", test.delegated, test.index, got, test.want)
		}
	}
}

func TestRenderDnsmasqIPv6ConfigPassesDnsmasqSyntaxCheck(t *testing.T) {
	binary, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Skip("dnsmasq is not installed")
	}
	lifetime := 0
	artifacts, err := RenderDnsmasqConfig([]DnsmasqPlan{
		{
			ID: "lan-a", InterfaceID: "lylan-eth2",
			Subnet: "192.168.88.0/24", Pools: []string{"192.168.88.100-192.168.88.199"},
			Routers:    []string{"192.168.88.1"},
			IPv6Prefix: "2001:db8:100::/56", RAInterval: 30, RARouterLifetime: &lifetime,
		},
		{ID: "lan-b", InterfaceID: "lylan-eth3", IPv6Prefix: "2001:db8:101::/64", IPv6Mode: "stateful"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	if err := os.WriteFile(path, []byte(artifacts[0].Content), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary, "--test", "-C", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dnsmasq rejected rendered config: %v\n%s\n%s", err, output, artifacts[0].Content)
	}
}
