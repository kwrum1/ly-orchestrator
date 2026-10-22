// Command render prints the real proxy capture artifacts for the independent
// netns harness in scripts/test-proxy-tproxy-netns.sh. It lives under testdata
// so it is not part of any production package.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/service"
)

func main() {
	mode := flag.String("mode", "json", "json, nft, or xray")
	lan := flag.String("lan", "10.200.1.0/24", "LAN return prefix that must bypass capture")
	underlay := flag.String("underlay", "203.0.113.1 veth-up", "resolved VPP underlay for the policy route plan")
	guardAddress := flag.String("guard-address", "192.168.88.1", "LAN gateway address for the LCP DNS guard")
	guardInterface := flag.String("guard-interface", "lylan-eth2", "Linux LCP host interface for the DNS guard")
	flag.Parse()

	egress := proxy.NewProxyEgress("proxy-netns", "xray-vpp-service")
	compiled, err := proxy.CompileEgress(egress)
	if err != nil {
		fail(err)
	}
	if err := proxy.BindServiceNetwork(&compiled, *underlay, []string{*lan}, 1500); err != nil {
		fail(err)
	}

	switch *mode {
	case "json":
		payload := struct {
			Network proxy.ProxyServiceNetwork    `json:"network"`
			Capture proxy.NftablesCapturePlan    `json:"capture"`
			Routing proxy.LinuxPolicyRoutingPlan `json:"routing"`
		}{compiled.ServiceNetwork, compiled.NftablesCapture, compiled.LinuxPolicyRouting}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(payload); err != nil {
			fail(err)
		}
	case "nft":
		artifacts, err := service.RenderNftablesCapture(compiled.NftablesCapture)
		if err != nil {
			fail(err)
		}
		fmt.Print(artifacts[0].Content)
	case "guard":
		artifacts, err := service.RenderGatewayNftablesCapture(proxy.NftablesCapturePlan{}, service.DNSInterceptionPlan{
			LCPGuardAddresses:  []string{*guardAddress},
			LCPGuardInterfaces: []string{*guardInterface},
		})
		if err != nil {
			fail(err)
		}
		fmt.Print(artifacts[0].Content)
	case "xray":
		config := compiled.XrayRuntime.ConfigPayload
		config.Outbounds = []proxy.XrayOutbound{{Tag: "node-netns", Protocol: "freedom"}}
		if err := proxy.EnsureDirectFallback(&config, proxy.ProxyDirectProbeURL); err != nil {
			fail(err)
		}
		proxy.ApplyOutboundSocketMark(&config, compiled.ServiceNetwork.OutboundMark)
		payload, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			fail(err)
		}
		fmt.Println(string(payload))
	default:
		fail(fmt.Errorf("unknown mode %q", *mode))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
