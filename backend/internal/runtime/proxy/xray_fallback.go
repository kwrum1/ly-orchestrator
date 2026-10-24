package proxy

import (
	"fmt"
	"strings"
)

// EnsureFailureFallback makes proxy failure fail closed instead of failing
// open to the direct Internet path.
//
// Every routing rule that selects a proxy outbound is rewritten to a
// single-node balancer whose fallbackTag is the "failure" blackhole outbound.
// When the Xray observatory marks the node dead, the balancer therefore
// selects blackhole, not a direct freedom outbound. The legacy "direct"
// freedom outbound is removed and the terminal catch-all also points at the
// same blackhole so no captured packet can fall through to direct routing.
func EnsureFailureFallback(config *XrayConfigPayload, probeURL string) error {
	if config == nil {
		return fmt.Errorf("%w: xray config is required for failure fallback", ErrInvalidEgress)
	}
	if strings.TrimSpace(probeURL) == "" {
		probeURL = ProxyProbeURL
	}

	// Preserve proxy outbounds, drop a legacy direct outbound (the old
	// fail-open path), and normalise exactly one failure blackhole outbound.
	outbounds := make([]XrayOutbound, 0, len(config.Outbounds)+1)
	proxyTags := make([]string, 0, len(config.Outbounds))
	for _, outbound := range config.Outbounds {
		tag := strings.TrimSpace(outbound.Tag)
		if tag == "" {
			return fmt.Errorf("%w: xray outbound requires a tag", ErrInvalidEgress)
		}
		switch tag {
		case ProxyFailureOutboundTag:
			continue
		case ProxyDirectOutboundTag:
			continue
		}
		if strings.EqualFold(strings.TrimSpace(outbound.Protocol), "freedom") {
			return fmt.Errorf("%w: freedom outbound %q would leak captured traffic to the direct path", ErrInvalidEgress, tag)
		}
		outbound.Tag = tag
		outbounds = append(outbounds, outbound)
		proxyTags = append(proxyTags, tag)
	}
	if len(proxyTags) == 0 {
		return fmt.Errorf("%w: failure fallback requires at least one proxy outbound", ErrInvalidEgress)
	}
	outbounds = append(outbounds, XrayOutbound{Tag: ProxyFailureOutboundTag, Protocol: "blackhole"})
	config.Outbounds = outbounds

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
		if rule.OutboundTag == ProxyDirectOutboundTag {
			// A stale rule pointing at the removed fail-open outbound would be
			// rejected by Xray. Re-point it at blackhole rather than dropping
			// it, so the policy stays fail closed.
			rule.OutboundTag = ProxyFailureOutboundTag
			continue
		}
		if !containsString(proxyTags, rule.OutboundTag) {
			continue
		}
		balancerTag := fallbackBalancerTag(rule.OutboundTag)
		appendFailureBalancer(config.Routing, balancerTag, rule.OutboundTag)
		rule.BalancerTag = balancerTag
		rule.OutboundTag = ""
		routed = true
	}
	if !routed {
		balancerTag := fallbackBalancerTag(proxyTags[0])
		appendFailureBalancer(config.Routing, balancerTag, proxyTags[0])
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

	// Existing balancers (for example a previous generation) must not keep a
	// direct fallback. Only the selectors generated from proxy outbounds may
	// remain selectable.
	for index := range config.Routing.Balancers {
		balancer := &config.Routing.Balancers[index]
		balancer.FallbackTag = ProxyFailureOutboundTag
		selector := make([]string, 0, len(balancer.Selector))
		for _, tag := range balancer.Selector {
			if containsString(proxyTags, strings.TrimSpace(tag)) {
				selector = append(selector, strings.TrimSpace(tag))
			}
		}
		balancer.Selector = selector
	}

	// Anything that did not match a capture inbound must be black-holed, not
	// sent to the direct Internet path. Keep exactly one terminal catch-all.
	config.Routing.Rules = withoutTerminalFailure(config.Routing.Rules)
	config.Routing.Rules = append(config.Routing.Rules, XrayRoutingRule{
		Type:        "field",
		Network:     "tcp,udp",
		OutboundTag: ProxyFailureOutboundTag,
	})
	ensureObservatory(config, proxyTags, probeURL)
	return nil
}

func appendFailureBalancer(routing *XrayRouting, tag, outboundTag string) {
	for index := range routing.Balancers {
		if routing.Balancers[index].Tag == tag {
			routing.Balancers[index].Selector = []string{outboundTag}
			routing.Balancers[index].FallbackTag = ProxyFailureOutboundTag
			routing.Balancers[index].Strategy = XrayBalancerStrategy{Type: "random", Settings: map[string]any{}}
			return
		}
	}
	routing.Balancers = append(routing.Balancers, XrayBalancer{
		Tag:         tag,
		Selector:    []string{outboundTag},
		FallbackTag: ProxyFailureOutboundTag,
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
	// The reconciler reads liveness from the metrics endpoint, so every proxy
	// mode (fixed, fastest and adaptive) must expose it.
	if config.Metrics == nil {
		config.Metrics = &XrayMetrics{Listen: DefaultXrayMetricsListen}
	}
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

// withoutTerminalFailure removes the generated terminal blackhole catch-all so
// repeated application stays idempotent. Older configs may end with the
// legacy direct catch-all, which is removed as well.
func withoutTerminalFailure(rules []XrayRoutingRule) []XrayRoutingRule {
	for len(rules) > 0 {
		last := rules[len(rules)-1]
		if len(last.InboundTags) == 0 && last.BalancerTag == "" && last.Network != "" &&
			(last.OutboundTag == ProxyFailureOutboundTag || last.OutboundTag == ProxyDirectOutboundTag) {
			rules = rules[:len(rules)-1]
			continue
		}
		break
	}
	return rules
}

func isFailureCatchAll(rule XrayRoutingRule) bool {
	return rule.Type == "field" && rule.OutboundTag == ProxyFailureOutboundTag && len(rule.InboundTags) == 0 && rule.Network != ""
}
