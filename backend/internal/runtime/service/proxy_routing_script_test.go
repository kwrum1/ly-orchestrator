package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/nat"
	"ly-route/backend/internal/runtime/proxy"
)

func TestRenderLinuxPolicyRoutingProxyTenantScript(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-egress-kurumi")
	network.MTU = 1500
	network.NATPoolAddress = "10.1.18.187"
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	network.LANRoutes = []string{"192.168.89.0/24"}
	plan := proxy.LinuxPolicyRoutingPlan{
		EgressID:      network.EgressID,
		Mark:          "0x1002c3",
		MarkValue:     0x1002c3,
		MarkMask:      "0xffffffff",
		TableID:       network.OutboundTableID,
		TableName:     "ly_route_proxy_egress",
		RulePriority:  network.OutboundRulePriority,
		DefaultRoute:  proxy.LinuxDefaultRoute{Destination: "default", Table: network.OutboundTableID, Via: network.EgressVPPAddress, Device: network.EgressHostInterface, Scope: "link"},
		Network:       network,
		UnderlayRoute: network.UnderlayRoute,
	}
	artifacts, err := RenderLinuxPolicyRouting(plan, nat.BehaviorEndpointDependent)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Service != LinuxRouting {
		t.Fatalf("linux-routing artifacts = %#v", artifacts)
	}
	script := artifacts[0].Content
	for _, required := range []string{
		"vpp_require \"egress TAP tenant table\" set interface ip table " + network.EgressVPPInterface,
		"vpp_require \"proxy underlay default route\" ip route add table " + strconv.Itoa(network.OutboundTableID) + " 0.0.0.0/0 via 10.1.18.1 lyroute-enp4s0",
		"vpp_wait_pattern \"proxy underlay route\" lyroute-enp4s0",
		"nat44 add address 10.1.18.187 tenant-vrf " + strconv.Itoa(network.OutboundTableID),
		"proxy_ensure_wan_output_feature()",
		"\"$VPPCTL\" set interface nat44 out \"$wan_if\" del",
		"\"$VPPCTL\" set interface nat44 out \"$wan_if\" output-feature",
		"proxy_ensure_wan_output_feature lyroute-enp4s0",
		"nat44_wait_vrf \"tenant NAT pool\" 10.1.18.187",
		"nat44_pool_has_vrf 10.1.18.187 " + strconv.Itoa(network.OutboundTableID),
		"policy_routing_fail()",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("proxy linux-routing script is missing %q:\n%s", required, script)
		}
	}
	for _, forbidden := range []string{
		"show nat44 addresses | grep -Fq 10.1.18.187",
		"vpp_wait_pattern \"tenant NAT pool\" 10.1.18.187",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("proxy linux-routing script still uses address-only NAT pool readback %q:\n%s", forbidden, script)
		}
	}
	if strings.Contains(script, "set interface nat44 in "+network.EgressVPPInterface+" out ") {
		t.Fatalf("proxy egress was attached as a NAT in/out pair:\n%s", script)
	}
}

func TestServiceUnderlayInterfaceParsesStructuredRoutes(t *testing.T) {
	cases := map[string]string{
		"10.1.18.1 lyroute-enp4s0":                   "lyroute-enp4s0",
		"10.1.18.1 via lyroute-enp4s0":               "lyroute-enp4s0",
		"10.1.18.1 dev lyroute-enp4s0":               "lyroute-enp4s0",
		"10.1.18.1 via lyroute-enp4s0 table 100":     "lyroute-enp4s0",
		"lyroute-enp4s0":                             "lyroute-enp4s0",
		"ip4-lookup-in-table 70099":                  "",
		"pppoe-runtime:wan-primary":                  "pppoe-runtime:wan-primary",
		"10.1.18.1 via 10.1.18.1 dev lyroute-enp4s0": "lyroute-enp4s0",
	}
	for input, want := range cases {
		if got := serviceUnderlayInterface(input); got != want {
			t.Fatalf("serviceUnderlayInterface(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProxyPolicyRoutingScriptPassesBashSyntaxCheck(t *testing.T) {
	network := proxy.ServiceNetworkForEgressID("proxy-egress-kurumi")
	network.MTU = 1500
	network.NATPoolAddress = "10.1.18.187"
	network.UnderlayRoute = "10.1.18.1 lyroute-enp4s0"
	network.LANRoutes = []string{"192.168.89.0/24"}
	plan := proxy.LinuxPolicyRoutingPlan{
		EgressID:      network.EgressID,
		Mark:          "0x1002c3",
		MarkValue:     0x1002c3,
		MarkMask:      "0xffffffff",
		TableID:       network.OutboundTableID,
		TableName:     "ly_route_proxy_egress",
		RulePriority:  network.OutboundRulePriority,
		DefaultRoute:  proxy.LinuxDefaultRoute{Destination: "default", Table: network.OutboundTableID, Via: network.EgressVPPAddress, Device: network.EgressHostInterface, Scope: "link"},
		Network:       network,
		UnderlayRoute: network.UnderlayRoute,
	}
	artifacts, err := RenderLinuxPolicyRouting(plan, nat.BehaviorEndpointDependent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy-routing-apply")
	if err := os.WriteFile(path, []byte(artifacts[0].Content), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n rejected policy-routing-apply: %v\n%s", err, output)
	}
}
