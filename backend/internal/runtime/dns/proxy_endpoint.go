package dns

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Proxy DNS endpoint identity.
//
// A proxy outcome is resolved through a local transport endpoint owned by the
// proxy runtime (see docs/DNS-PROXY-SEMANTICS.md). The endpoint is identified
// by the (proxy egress, DNS upstream) pair, not by the rule that happens to
// reference it, so editing a rule does not move the listener and two rules
// with the same combination share one endpoint.
//
// Both the ID and the SmartDNS server-group name are derived from a digest
// instead of the raw IDs: rule/egress IDs are user-editable up to 64 bytes,
// while SmartDNS group names and Xray inbound tags must stay short and must
// never let a crafted ID inject a configuration line.

const proxyDNSEndpointDigestLength = 6

func proxyDNSEndpointDigest(egressID, upstreamID string) string {
	normalized := strings.ToLower(strings.TrimSpace(egressID)) + "\x00" + strings.ToLower(strings.TrimSpace(upstreamID))
	digest := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", digest[:proxyDNSEndpointDigestLength])
}

// ProxyDNSEndpointID is the stable identity of the local endpoint for one
// (proxy egress, upstream) combination.
func ProxyDNSEndpointID(egressID, upstreamID string) string {
	return "px-" + proxyDNSEndpointDigest(egressID, upstreamID)
}

// ProxyDNSEndpointGroup is the SmartDNS server-group name of that endpoint.
// It must stay a single token because it is written into `server` and
// `nameserver` directives.
func ProxyDNSEndpointGroup(egressID, upstreamID string) string {
	return "lyroute-px-" + proxyDNSEndpointDigest(egressID, upstreamID)
}

// ProxyBindingsFromRender extracts the proxy outcomes of a rendered plan. It
// exists for components that only hold the compiled/persisted render (status,
// xray restart, diagnostics) and must derive the same endpoint generation the
// apply would create. The miss rule is kept as a binding so an all-proxy miss
// policy cannot silently disappear.
func ProxyBindingsFromRender(render SmartDNSRender) []ProxyDNSBinding {
	bindings := make([]ProxyDNSBinding, 0, len(render.Rules)+1)
	appendOutcome := func(ruleID string, rule SmartDNSRule) {
		if rule.OutcomeKind != ResolverOutcomeProxyResolution {
			return
		}
		mode := rule.FailureMode
		if mode == "" {
			mode = FailureModeFailClosed
		}
		bindings = append(bindings, ProxyDNSBinding{
			RuleID:             ruleID,
			ProxyEgressID:      rule.ProxyEgressID,
			UpstreamID:         rule.UpstreamID,
			OutcomeKind:        ResolverOutcomeProxyResolution,
			PolicyPrecedence:   rule.PolicyPrecedence,
			ResolverPath:       rule.ResolverPath,
			DNSRequestPath:     rule.DNSRequestPath,
			FailureMode:        mode,
			FallbackUpstreamID: rule.FallbackUpstreamID,
		})
	}
	for _, rule := range render.Rules {
		appendOutcome(rule.RuleID, rule)
	}
	appendOutcome("miss", render.Miss)
	return bindings
}
