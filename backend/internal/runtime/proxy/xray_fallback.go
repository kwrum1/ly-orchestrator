package proxy

import (
	"fmt"
	"strings"
)

// EnsureDirectFallback makes proxy failure fail open instead of black-holed.
// Every rule that selected a proxy outbound is routed through a single-node
// balancer whose fallbackTag is the "direct" freedom outbound. The Xray
// observatory marks an unreachable node dead, after which the balancer
// selects the direct outbound. The direct outbound deliberately has no proxy
// socket mark, so it uses the ordinary WAN route.
func EnsureDirectFallback(config *XrayConfigPayload, probeURL string) error {
	if config == nil {
		return fmt.Errorf("%w: xray config is required for direct fallback", ErrInvalidEgress)
	}
	if strings.TrimSpace(probeURL) == "" {
		probeURL = ProxyDirectProbeURL
	}

	hasDirect := false
	proxyTags := make([]string, 0, len(config.Outbounds))
	for _, outbound := range config.Outbounds {
		tag := strings.TrimSpace(outbound.Tag)
		if tag == "" {
			return fmt.Errorf("%w: xray outbound requires a tag", ErrInvalidEgress)
		}
		if tag == ProxyDirectOutboundTag {
			hasDirect = true
			continue
		}
		proxyTags = append(proxyTags, tag)
	}
	if len(proxyTags) == 0 {
		return fmt.Errorf("%w: direct fallback requires at least one proxy outbound", ErrInvalidEgress)
	}
	if !hasDirect {
		config.Outbounds = append(config.Outbounds, XrayOutbound{Tag: ProxyDirectOutboundTag, Protocol: "freedom"})
	}
	if config.Routing == nil {
		config.Routing = &XrayRouting{}
	}

	routed := false
	for index := range config.Routing.Rules {
		rule := &config.Routing.Rules[index]
		if rule.BalancerTag != "" {
			if isFallbackBalancer(rule.BalancerTag, config.Routing.Balancers) {
				routed = true
			}
			continue
		}
		if !containsString(proxyTags, rule.OutboundTag) {
			continue
		}
		balancerTag := fallbackBalancerTag(rule.OutboundTag)
		appendFallbackBalancer(config.Routing, balancerTag, rule.OutboundTag)
		rule.BalancerTag = balancerTag
		rule.OutboundTag = ""
		routed = true
	}
	if !routed {
		balancerTag := fallbackBalancerTag(proxyTags[0])
		appendFallbackBalancer(config.Routing, balancerTag, proxyTags[0])
		for _, inbound := range config.Inbounds {
			if inbound.Tag == XrayRoutingAPIInbound || inbound.Protocol != "dokodemo-door" {
				continue
			}
			config.Routing.Rules = append(config.Routing.Rules, XrayRoutingRule{
				Type:        "field",
				InboundTags: []string{inbound.Tag},
				BalancerTag: balancerTag,
			})
		}
	}

	// Anything that did not match a capture inbound must not fall through to
	// the first proxy outbound by accident. Keep one explicit direct
	// catch-all at the end; repeated application is a no-op.
	if len(config.Routing.Rules) == 0 || !isDirectCatchAll(config.Routing.Rules[len(config.Routing.Rules)-1]) {
		config.Routing.Rules = append(config.Routing.Rules, XrayRoutingRule{
			Type:        "field",
			Network:     "tcp,udp",
			OutboundTag: ProxyDirectOutboundTag,
		})
	}
	ensureObservatory(config, proxyTags, probeURL)
	return nil
}

func appendFallbackBalancer(routing *XrayRouting, tag, outboundTag string) {
	for index := range routing.Balancers {
		if routing.Balancers[index].Tag == tag {
			routing.Balancers[index].Selector = []string{outboundTag}
			routing.Balancers[index].FallbackTag = ProxyDirectOutboundTag
			routing.Balancers[index].Strategy = XrayBalancerStrategy{Type: "random", Settings: map[string]any{}}
			return
		}
	}
	routing.Balancers = append(routing.Balancers, XrayBalancer{
		Tag:         tag,
		Selector:    []string{outboundTag},
		FallbackTag: ProxyDirectOutboundTag,
		Strategy:    XrayBalancerStrategy{Type: "random", Settings: map[string]any{}},
	})
}

func isFallbackBalancer(tag string, balancers []XrayBalancer) bool {
	for _, balancer := range balancers {
		if balancer.Tag == tag {
			return true
		}
	}
	return false
}

func fallbackBalancerTag(outboundTag string) string {
	var builder strings.Builder
	builder.WriteString("fallback-")
	for _, char := range outboundTag {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('-')
	}
	return builder.String()
}

func ensureObservatory(config *XrayConfigPayload, proxyTags []string, probeURL string) {
	if config.Observatory == nil {
		config.Observatory = &XrayObservatory{
			SubjectSelector:   append([]string(nil), proxyTags...),
			ProbeURL:          probeURL,
			ProbeInterval:     "10s",
			EnableConcurrency: true,
		}
		return
	}
	seen := make(map[string]struct{}, len(config.Observatory.SubjectSelector))
	for _, selector := range config.Observatory.SubjectSelector {
		seen[selector] = struct{}{}
	}
	for _, tag := range proxyTags {
		if _, exists := seen[tag]; exists {
			continue
		}
		config.Observatory.SubjectSelector = append(config.Observatory.SubjectSelector, tag)
	}
	if strings.TrimSpace(config.Observatory.ProbeURL) == "" {
		config.Observatory.ProbeURL = probeURL
	}
	if strings.TrimSpace(config.Observatory.ProbeInterval) == "" {
		config.Observatory.ProbeInterval = "10s"
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func isDirectCatchAll(rule XrayRoutingRule) bool {
	return rule.Type == "field" && rule.OutboundTag == ProxyDirectOutboundTag && len(rule.InboundTags) == 0 && rule.Network != ""
}
