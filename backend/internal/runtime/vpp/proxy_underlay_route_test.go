package vpp

import (
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
)

func TestProxyServiceNetworkRoutesThroughEgressVRF(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-media")
	network.NATPoolAddress = "10.1.18.53"
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	network.LANRoutes = []string{"192.168.88.0/24"}
	steering := proxy.VPPSteeringInstruction{
		EgressID:       network.EgressID,
		TargetKind:     "vpp.proxy-service.network",
		ServiceNetwork: network,
		UnderlayRoute:  network.UnderlayRoute,
	}
	commands := strings.Join(proxySteeringCommands(steering, "endpoint_dependent"), "\n")
	for _, required := range []string{
		"?ip table add " + proxyTestInt(network.OutboundTableID),
		"?set interface ip table " + network.EgressVPPInterface + " " + proxyTestInt(network.OutboundTableID),
		"?ip route add table " + proxyTestInt(network.OutboundTableID) + " 0.0.0.0/0 via 10.1.18.1 lyroute-enp4s0",
		"?ip route add table " + proxyTestInt(network.OutboundTableID) + " " + network.EgressHostAddress + "/32 via " + network.EgressVPPInterface,
		"?ip route add table " + proxyTestInt(network.OutboundTableID) + " " + network.IngressHostAddress + "/32 via " + network.IngressVPPInterface,
		"?nat44 add address 10.1.18.53 tenant-vrf " + proxyTestInt(network.OutboundTableID),
		"set interface nat44 out lyroute-enp4s0 output-feature",
		"?set interface nat44 out lyroute-enp4s0 del",
		"show ip fib table " + proxyTestInt(network.OutboundTableID),
	} {
		if !strings.Contains(commands, required) {
			t.Fatalf("proxy egress VRF commands are missing %q:\n%s", required, commands)
		}
	}
	if strings.Contains(commands, "?set interface nat44 in "+network.EgressVPPInterface+" out ") {
		t.Fatalf("tenant proxy egress was attached as a NAT44 input, breaking the FIB 0 return path:\n%s", commands)
	}
	if strings.Contains(commands, "show ip fib table 0") {
		t.Fatalf("proxy egress still trusted the main FIB:\n%s", commands)
	}
}

func TestProxyServiceNetworkInstallsUnderlayRouteWithoutTenantPool(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-direct")
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	steering := proxy.VPPSteeringInstruction{
		EgressID:       network.EgressID,
		UnderlayWANID:  "j1900-pub",
		TargetKind:     "vpp.proxy-service.network",
		ServiceNetwork: network,
		UnderlayRoute:  network.UnderlayRoute,
	}
	commands := strings.Join(proxySteeringCommands(steering, "endpoint_dependent"), "\n")
	for _, required := range []string{
		"?ip table add " + strconv.Itoa(network.OutboundTableID),
		"?ip route add table " + strconv.Itoa(network.OutboundTableID) + " 0.0.0.0/0 via 10.1.18.1 lyroute-enp4s0",
	} {
		if !strings.Contains(commands, required) {
			t.Fatalf("proxy underlay route is missing %q:\n%s", required, commands)
		}
	}
	if strings.Contains(commands, "?set interface ip table "+network.EgressVPPInterface) {
		t.Fatalf("proxy egress moved into a tenant FIB without a NAT pool:\n%s", commands)
	}
}

func TestProxyUnderlayInterfacesFeedNAT44OutputFeature(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-media")
	network.NATPoolAddress = "10.1.18.53"
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	steering := proxy.VPPSteeringInstruction{
		EgressID:       network.EgressID,
		UnderlayWANID:  "j1900-pub",
		TargetKind:     "vpp.proxy-service.network",
		ServiceNetwork: network,
		UnderlayRoute:  network.UnderlayRoute,
	}
	plan := Plan{Proxy: proxy.CompiledEgress{VPPSteering: []proxy.VPPSteeringInstruction{steering}}}
	underlays := proxyUnderlayInterfaces(plan)
	if len(underlays) != 1 || underlays[0] != "lyroute-enp4s0" {
		t.Fatalf("proxy underlay interfaces = %#v, want [lyroute-enp4s0]", underlays)
	}
	// The resolved route may only live on the service network; both carriers
	// must feed the NAT44 role.
	steering.UnderlayRoute = ""
	plan.Proxy.VPPSteering = []proxy.VPPSteeringInstruction{steering}
	if underlays := proxyUnderlayInterfaces(plan); len(underlays) != 1 || underlays[0] != "lyroute-enp4s0" {
		t.Fatalf("service-network underlay was not used: %#v", underlays)
	}
	if underlays := proxyUnderlayInterfaces(Plan{}); len(underlays) != 0 {
		t.Fatalf("a plan without proxy egress produced underlays: %#v", underlays)
	}
}

func TestBuildOperationsRejectsProxyServiceWithoutUnderlay(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-media")
	steering := proxy.VPPSteeringInstruction{
		EgressID:       network.EgressID,
		UnderlayWANID:  "j1900-pub",
		TargetKind:     "vpp.proxy-service.network",
		ServiceNetwork: network,
	}
	plan := provenPlan(Plan{RequestID: "req-proxy-no-underlay", Proxy: proxy.CompiledEgress{VPPSteering: []proxy.VPPSteeringInstruction{steering}}}, "enp0s3")
	if _, err := BuildOperations(plan); err == nil || !strings.Contains(err.Error(), "underlay") {
		t.Fatalf("proxy service without an underlay must fail the plan, got %v", err)
	}
}

func proxyTestInt(value int) string {
	return strconv.Itoa(value)
}
