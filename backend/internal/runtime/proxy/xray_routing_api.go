package proxy

import "fmt"

const (
	XrayRoutingAPIAddress = "127.0.0.1:10085"
	XrayRoutingAPITag     = "ly-route-routing-api"
	XrayRoutingAPIInbound = "ly-route-routing-api-in"
)

// EnableXrayRoutingAPI adds a loopback-only RoutingService endpoint used to
// read the balancer's actual selection and health. It is never exposed on LAN.
// The operation is idempotent: generation code may run it on an already
// normalised payload without producing a duplicate listener or routing rule.
func EnableXrayRoutingAPI(payload *XrayConfigPayload) error {
	if payload == nil || payload.Routing == nil {
		return fmt.Errorf("xray routing API requires a routing configuration")
	}
	hasInbound := false
	for _, inbound := range payload.Inbounds {
		if inbound.Tag != XrayRoutingAPIInbound && !(inbound.Listen == "127.0.0.1" && inbound.Port == 10085) {
			continue
		}
		if inbound.Tag != XrayRoutingAPIInbound || inbound.Listen != "127.0.0.1" || inbound.Port != 10085 || inbound.Protocol != "dokodemo-door" {
			return fmt.Errorf("xray routing API endpoint conflicts with inbound %q", inbound.Tag)
		}
		hasInbound = true
	}
	if payload.API != nil && payload.API.Tag != "" && payload.API.Tag != XrayRoutingAPITag {
		return fmt.Errorf("xray routing API conflicts with api tag %q", payload.API.Tag)
	}
	payload.API = &XrayAPI{Tag: XrayRoutingAPITag, Services: []string{"RoutingService"}}
	if !hasInbound {
		payload.Inbounds = append(payload.Inbounds, XrayInbound{
			Tag:      XrayRoutingAPIInbound,
			Listen:   "127.0.0.1",
			Port:     10085,
			Protocol: "dokodemo-door",
			Settings: XrayDokodemoSettings{Address: "127.0.0.1", Network: "tcp"},
		})
	}
	rules := make([]XrayRoutingRule, 0, len(payload.Routing.Rules)+1)
	for _, rule := range payload.Routing.Rules {
		if len(rule.InboundTags) == 1 && rule.InboundTags[0] == XrayRoutingAPIInbound && rule.OutboundTag == XrayRoutingAPITag {
			continue
		}
		rules = append(rules, rule)
	}
	payload.Routing.Rules = append([]XrayRoutingRule{{
		Type:        "field",
		InboundTags: []string{XrayRoutingAPIInbound},
		OutboundTag: XrayRoutingAPITag,
	}}, rules...)
	return nil
}
