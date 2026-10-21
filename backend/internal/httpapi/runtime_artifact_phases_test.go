package httpapi

import (
	"testing"

	"ly-route/backend/internal/runtime/proxy"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/vpp"
)

func TestSplitRuntimeArtifactsStartsPPPoEBeforeGatewayTransaction(t *testing.T) {
	artifacts := []serviceRuntime.RenderedArtifact{
		serviceRuntime.NewArtifact(serviceRuntime.SmartDNS, "/etc/smartdns/conf.d/ly-route-active.conf", "server 1.1.1.1\n", "restart"),
		serviceRuntime.NewArtifact(serviceRuntime.PPPoE, "/etc/ly-route/pppoe/ly-route-wan.json", "{}\n", "restart"),
		serviceRuntime.NewArtifact(serviceRuntime.LinuxRouting, "/var/lib/ly-route/policy-routing/apply.sh", "#!/bin/sh\n", "restart"),
		serviceRuntime.NewArtifact(serviceRuntime.Kea, "/etc/kea/kea-dhcp4.conf", "{}\n", "restart"),
	}
	pre, post := splitRuntimeArtifactsByPhase(artifacts, vpp.Plan{})

	if len(pre) != 2 {
		t.Fatalf("pre-gateway artifacts = %#v, want PPPoE and LinuxRouting", serviceNames(pre))
	}
	if pre[0].Service != serviceRuntime.PPPoE || pre[1].Service != serviceRuntime.LinuxRouting {
		t.Fatalf("pre-gateway order = %v, want pppoe before linux-routing", serviceNames(pre))
	}
	if len(post) != len(artifacts) {
		t.Fatalf("post-gateway artifacts = %v, want the authoritative full set", serviceNames(post))
	}
}

func TestSplitRuntimeArtifactsDefersLinuxRoutingUntilPPPoESessionExists(t *testing.T) {
	artifacts := []serviceRuntime.RenderedArtifact{
		serviceRuntime.NewArtifact(serviceRuntime.PPPoE, "/etc/ly-route/pppoe/ly-route-wan.json", "{}\n", "restart"),
		serviceRuntime.NewArtifact(serviceRuntime.LinuxRouting, "/var/lib/ly-route/policy-routing/apply.sh", "#!/bin/sh\n", "restart"),
	}
	network, err := vpp.DNSServiceNetworkForUpstreamID("dns-lab", "wan-eth1-ipv4", []string{"1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := vpp.BindDNSServiceNetwork(&network, "pppoe-runtime:wan-eth1-ipv4"); err != nil {
		t.Fatal(err)
	}
	gateway := vpp.Plan{
		DNSServiceNetworks: []vpp.DNSServiceNetwork{network},
		Proxy:              proxy.CompiledEgress{VPPSteering: []proxy.VPPSteeringInstruction{{TargetKind: "vpp.proxy-service.network", EgressID: "proxy", UnderlayRoute: "pppoe-runtime:wan-eth1-ipv4"}}},
	}
	pre, post := splitRuntimeArtifactsByPhase(artifacts, gateway)
	if len(pre) != 1 || pre[0].Service != serviceRuntime.PPPoE {
		t.Fatalf("pre-gateway artifacts = %v, want PPPoE only", serviceNames(pre))
	}
	if len(post) != len(artifacts) {
		t.Fatalf("post-gateway artifacts = %v, want the full authoritative set", serviceNames(post))
	}
}

func serviceNames(artifacts []serviceRuntime.RenderedArtifact) []serviceRuntime.ServiceName {
	names := make([]serviceRuntime.ServiceName, 0, len(artifacts))
	for _, artifact := range artifacts {
		names = append(names, artifact.Service)
	}
	return names
}
