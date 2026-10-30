package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func proxyDNSTestSpec(id string, port int, protocol ProxyDNSEndpointProtocol, outbound string) ProxyDNSEndpointSpec {
	return ProxyDNSEndpointSpec{
		ID:            id,
		EgressID:      "proxy-media",
		UpstreamID:    "dns-proxy",
		Protocol:      protocol,
		Port:          port,
		TargetAddress: "198.51.100.53",
		TargetPort:    53,
		ServerName:    "dns.example",
		OutboundTag:   outbound,
	}
}

func TestAttachProxyDNSEndpointsAddsLoopbackInboundAndRouting(t *testing.T) {
	config := XrayConfigPayload{
		Inbounds:  []XrayInbound{{Tag: "proxy-in", Listen: "0.0.0.0", Port: 23456, Protocol: "dokodemo-door"}},
		Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}},
	}
	// EnsureFailureFallback installs the terminal catch-all the DNS rules must
	// stay ahead of.
	if err := EnsureFailureFallback(&config, ProxyProbeURL); err != nil {
		t.Fatal(err)
	}
	spec := proxyDNSTestSpec("px-abc123", 15353, ProxyDNSEndpointUDP, "")
	spec.ServerName = ""
	if err := AttachProxyDNSEndpoints(&config, []ProxyDNSEndpointSpec{spec}); err != nil {
		t.Fatal(err)
	}

	var dnsInbound *XrayInbound
	for index := range config.Inbounds {
		if config.Inbounds[index].Tag == ProxyDNSEndpointTagPrefix+"px-abc123" {
			dnsInbound = &config.Inbounds[index]
		}
	}
	if dnsInbound == nil {
		t.Fatalf("DNS inbound missing: %#v", config.Inbounds)
	}
	if dnsInbound.Listen != ProxyDNSListenAddress || dnsInbound.Port != 15353 || dnsInbound.Protocol != "dokodemo-door" {
		t.Fatalf("DNS inbound = %#v, want loopback dokodemo-door on 15353", dnsInbound)
	}
	if dnsInbound.Settings.Address != "198.51.100.53" || dnsInbound.Settings.Port != 53 || dnsInbound.Settings.Network != "udp,tcp" || dnsInbound.Settings.FollowRedirect {
		t.Fatalf("DNS inbound settings = %#v", dnsInbound.Settings)
	}

	if config.Routing == nil || len(config.Routing.Rules) < 2 {
		t.Fatalf("routing rules = %#v", config.Routing)
	}
	last := config.Routing.Rules[len(config.Routing.Rules)-1]
	if last.OutboundTag != ProxyFailureOutboundTag {
		t.Fatalf("terminal catch-all moved: %#v", last)
	}
	found := false
	for _, rule := range config.Routing.Rules[:len(config.Routing.Rules)-1] {
		if len(rule.InboundTags) == 1 && rule.InboundTags[0] == ProxyDNSEndpointTagPrefix+"px-abc123" {
			found = true
			if rule.OutboundTag != "node-a" {
				t.Fatalf("DNS routing rule outbound = %q, want the only proxy outbound", rule.OutboundTag)
			}
		}
	}
	if !found {
		t.Fatalf("DNS routing rule missing: %#v", config.Routing.Rules)
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"followRedirect":false`) {
		t.Fatalf("DNS inbound must not use tproxy redirect: %s", encoded)
	}
}

func TestAttachProxyDNSEndpointsIsIdempotentAndReplacesStale(t *testing.T) {
	config := XrayConfigPayload{Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}}}
	first := proxyDNSTestSpec("px-one", 15353, ProxyDNSEndpointUDP, "")
	second := proxyDNSTestSpec("px-two", 15400, ProxyDNSEndpointDoH, "node-b")
	if err := AttachProxyDNSEndpoints(&config, []ProxyDNSEndpointSpec{first, second}); err != nil {
		t.Fatal(err)
	}
	once, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := AttachProxyDNSEndpoints(&config, []ProxyDNSEndpointSpec{first, second}); err != nil {
		t.Fatal(err)
	}
	twice, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("attach is not idempotent:\n%s\n%s", once, twice)
	}
	if err := AttachProxyDNSEndpoints(&config, []ProxyDNSEndpointSpec{second}); err != nil {
		t.Fatal(err)
	}
	for _, inbound := range config.Inbounds {
		if inbound.Tag == ProxyDNSEndpointTagPrefix+"px-one" {
			t.Fatalf("stale DNS inbound survived the reload: %#v", config.Inbounds)
		}
	}
	for _, rule := range config.Routing.Rules {
		for _, tag := range rule.InboundTags {
			if tag == ProxyDNSEndpointTagPrefix+"px-one" {
				t.Fatalf("stale DNS routing rule survived the reload: %#v", config.Routing.Rules)
			}
		}
	}
}

func TestAttachProxyDNSEndpointsRejectsUnsafeInput(t *testing.T) {
	base := XrayConfigPayload{Outbounds: []XrayOutbound{{Tag: "node-a", Protocol: "vless"}}}

	publicListen := proxyDNSTestSpec("px-public", 15353, ProxyDNSEndpointUDP, "")
	publicListen.TargetAddress = "127.0.0.1"
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{publicListen}); err == nil || !strings.Contains(err.Error(), "routable literal") {
		t.Fatalf("loopback target error = %v", err)
	}

	unsafeID := proxyDNSTestSpec("px bad\ninbound", 15353, ProxyDNSEndpointUDP, "")
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{unsafeID}); err == nil {
		t.Fatal("unsafe endpoint id was accepted")
	}

	privileged := proxyDNSTestSpec("px-port", 53, ProxyDNSEndpointUDP, "")
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{privileged}); err == nil || !strings.Contains(err.Error(), "1024-65535") {
		t.Fatalf("privileged port error = %v", err)
	}

	missingName := proxyDNSTestSpec("px-doh", 15353, ProxyDNSEndpointDoH, "")
	missingName.ServerName = ""
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{missingName}); err == nil || !strings.Contains(err.Error(), "server name") {
		t.Fatalf("DoH without server name error = %v", err)
	}

	unsupported := proxyDNSTestSpec("px-h3", 15353, ProxyDNSEndpointProtocol("h3"), "")
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{unsupported}); err == nil {
		t.Fatal("unsupported protocol was accepted")
	}

	first := proxyDNSTestSpec("px-one", 15353, ProxyDNSEndpointUDP, "")
	second := proxyDNSTestSpec("px-two", 15353, ProxyDNSEndpointUDP, "")
	if err := AttachProxyDNSEndpoints(&base, []ProxyDNSEndpointSpec{first, second}); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("port collision error = %v", err)
	}

	noOutbound := proxyDNSTestSpec("px-orphan", 15353, ProxyDNSEndpointUDP, "")
	if err := AttachProxyDNSEndpoints(&XrayConfigPayload{}, []ProxyDNSEndpointSpec{noOutbound}); err == nil || !strings.Contains(err.Error(), "no proxy outbound") {
		t.Fatalf("missing outbound error = %v", err)
	}
}

func TestProxyDNSEndpointTransportNetwork(t *testing.T) {
	cases := map[ProxyDNSEndpointProtocol]string{
		ProxyDNSEndpointUDP: "udp,tcp",
		ProxyDNSEndpointTCP: "tcp",
		ProxyDNSEndpointDoH: "tcp",
		ProxyDNSEndpointDoT: "tcp",
	}
	for protocol, want := range cases {
		spec := proxyDNSTestSpec("px-"+string(protocol), 15353, protocol, "node-a")
		if got := spec.transportNetwork(); got != want {
			t.Fatalf("%s transport network = %q, want %q", protocol, got, want)
		}
	}
}
