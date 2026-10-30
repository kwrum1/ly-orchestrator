package proxy

import (
	"fmt"
	"net/netip"
	"strings"
)

// Proxy DNS endpoints.
//
// A proxy DNS endpoint is a loopback-only listener inside the Xray process
// that forwards raw DNS transport to a resolver reached *through* the proxy
// egress. SmartDNS renders a per-domain nameserver that points at this
// listener, so the query only leaves through the selected egress and never
// falls back to a direct resolver.
//
// The transport is deliberately raw (dokodemo-door) instead of the Xray DNS
// module: the module exposes one process-global server list, so two proxied
// upstreams would race against each other and a single endpoint could not be
// bound to one upstream. A dokodemo-door inbound with a fixed target keeps one
// inbound, one port, one upstream.
//
// Protocol mapping (SmartDNS terminates TLS, Xray only forwards bytes):
//
//	udp  -> ip:53,  network "udp,tcp"
//	tcp  -> ip:53,  network "tcp"
//	doh  -> ip:443, network "tcp" (SmartDNS speaks HTTPS to 127.0.0.1)
//	dot  -> ip:853, network "tcp" (SmartDNS speaks TLS to 127.0.0.1)
const (
	// ProxyDNSEndpointTagPrefix names every generated loopback inbound so a
	// reload can replace its own inbounds without touching capture inbounds.
	ProxyDNSEndpointTagPrefix = "lyroute-proxy-dns-in-"
	// ProxyDNSListenAddress is the only address an endpoint may bind. A
	// non-loopback listener would expose an open resolver on the LAN/WAN.
	ProxyDNSListenAddress = "127.0.0.1"
)

// ProxyDNSEndpointProtocol is the wire protocol SmartDNS uses towards the
// local endpoint. It mirrors the protocol of the configured DNS upstream.
type ProxyDNSEndpointProtocol string

const (
	ProxyDNSEndpointUDP ProxyDNSEndpointProtocol = "udp"
	ProxyDNSEndpointTCP ProxyDNSEndpointProtocol = "tcp"
	ProxyDNSEndpointDoH ProxyDNSEndpointProtocol = "doh"
	ProxyDNSEndpointDoT ProxyDNSEndpointProtocol = "dot"
)

// ParseProxyDNSEndpointProtocol normalizes an upstream protocol name.
func ParseProxyDNSEndpointProtocol(value string) (ProxyDNSEndpointProtocol, error) {
	switch ProxyDNSEndpointProtocol(strings.ToLower(strings.TrimSpace(value))) {
	case ProxyDNSEndpointUDP:
		return ProxyDNSEndpointUDP, nil
	case ProxyDNSEndpointTCP:
		return ProxyDNSEndpointTCP, nil
	case ProxyDNSEndpointDoH:
		return ProxyDNSEndpointDoH, nil
	case ProxyDNSEndpointDoT:
		return ProxyDNSEndpointDoT, nil
	default:
		return "", fmt.Errorf("%w: proxy DNS endpoint protocol %q is unsupported (use udp, tcp, DoH or DoT)", ErrInvalidEgress, value)
	}
}

// ProxyDNSEndpointSpec is the validated input of one endpoint. It contains no
// user-authored strings that reach a config line unchecked.
type ProxyDNSEndpointSpec struct {
	ID            string
	EgressID      string
	UpstreamID    string
	Protocol      ProxyDNSEndpointProtocol
	Port          int
	TargetAddress string
	TargetPort    int
	// ServerName is the DoH/DoT host verified by SmartDNS (SNI + HTTP host).
	ServerName  string
	OutboundTag string
	FailureMode string
}

// InboundTag returns the generated Xray inbound tag of the endpoint.
func (spec ProxyDNSEndpointSpec) InboundTag() string {
	return ProxyDNSEndpointTagPrefix + spec.ID
}

func (spec ProxyDNSEndpointSpec) transportNetwork() string {
	if spec.Protocol == ProxyDNSEndpointUDP {
		return "udp,tcp"
	}
	return "tcp"
}

func (spec ProxyDNSEndpointSpec) validate() error {
	if !proxyDNSConfigToken(spec.ID) {
		return fmt.Errorf("%w: proxy DNS endpoint id is missing or unsafe", ErrInvalidEgress)
	}
	if strings.TrimSpace(spec.EgressID) == "" {
		return fmt.Errorf("%w: proxy DNS endpoint %q has no egress", ErrInvalidEgress, spec.ID)
	}
	if spec.Port < 1024 || spec.Port > 65535 {
		return fmt.Errorf("%w: proxy DNS endpoint %q port %d must be in 1024-65535", ErrInvalidEgress, spec.ID, spec.Port)
	}
	if _, err := ParseProxyDNSEndpointProtocol(string(spec.Protocol)); err != nil {
		return err
	}
	address, err := netip.ParseAddr(strings.TrimSpace(spec.TargetAddress))
	if err != nil || !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() {
		return fmt.Errorf("%w: proxy DNS endpoint %q target %q must be a routable literal IP address", ErrInvalidEgress, spec.ID, spec.TargetAddress)
	}
	if spec.TargetPort < 1 || spec.TargetPort > 65535 {
		return fmt.Errorf("%w: proxy DNS endpoint %q target port %d is invalid", ErrInvalidEgress, spec.ID, spec.TargetPort)
	}
	if spec.Protocol == ProxyDNSEndpointDoH || spec.Protocol == ProxyDNSEndpointDoT {
		if spec.ServerName == "" || !proxyDNSConfigToken(spec.ServerName) || strings.Contains(spec.ServerName, "_") {
			return fmt.Errorf("%w: proxy DNS endpoint %q requires a valid %s server name", ErrInvalidEgress, spec.ID, spec.Protocol)
		}
	}
	if spec.OutboundTag != "" && !proxyDNSConfigToken(spec.OutboundTag) {
		return fmt.Errorf("%w: proxy DNS endpoint %q outbound tag %q is unsafe", ErrInvalidEgress, spec.ID, spec.OutboundTag)
	}
	return nil
}

func proxyDNSConfigToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-', character == '.', character == ':':
			continue
		default:
			return false
		}
	}
	return true
}

// AttachProxyDNSEndpoints installs the loopback DNS inbounds and their routing
// rules into an already compiled Xray payload. It is idempotent: a previous
// generation of endpoints is removed first, so a policy edit cannot leave an
// orphan listener or a rule that points at a retired inbound.
func AttachProxyDNSEndpoints(config *XrayConfigPayload, specs []ProxyDNSEndpointSpec) error {
	if config == nil {
		return fmt.Errorf("%w: xray config is required for proxy DNS endpoints", ErrInvalidEgress)
	}
	inbounds := make([]XrayInbound, 0, len(config.Inbounds)+len(specs))
	for _, inbound := range config.Inbounds {
		if strings.HasPrefix(inbound.Tag, ProxyDNSEndpointTagPrefix) {
			continue
		}
		inbounds = append(inbounds, inbound)
	}
	if config.Routing != nil {
		rules := make([]XrayRoutingRule, 0, len(config.Routing.Rules))
		for _, rule := range config.Routing.Rules {
			if proxyDNSRoutingOnly(rule) {
				continue
			}
			rules = append(rules, rule)
		}
		config.Routing.Rules = rules
	}
	if len(specs) == 0 {
		config.Inbounds = inbounds
		return nil
	}

	defaultOutbound := ""
	if len(config.Outbounds) > 0 {
		defaultOutbound = strings.TrimSpace(config.Outbounds[0].Tag)
	}
	usedPorts := make(map[int]string, len(inbounds)+len(specs))
	for _, inbound := range inbounds {
		usedPorts[inbound.Port] = inbound.Tag
	}
	rules := make([]XrayRoutingRule, 0, len(specs))
	seenTags := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if err := spec.validate(); err != nil {
			return err
		}
		tag := spec.InboundTag()
		if _, exists := seenTags[tag]; exists {
			return fmt.Errorf("%w: duplicate proxy DNS endpoint %q", ErrInvalidEgress, spec.ID)
		}
		seenTags[tag] = struct{}{}
		if owner, exists := usedPorts[spec.Port]; exists {
			return fmt.Errorf("%w: proxy DNS endpoint %q port %d collides with inbound %q", ErrInvalidEgress, spec.ID, spec.Port, owner)
		}
		usedPorts[spec.Port] = tag
		// The fixed target is never dialled for a routed DNS inbound; it is
		// kept concrete so Xray has a valid dokodemo-door destination instead
		// of an empty address.
		inbounds = append(inbounds, XrayInbound{
			Tag:      tag,
			Listen:   ProxyDNSListenAddress,
			Port:     spec.Port,
			Protocol: "dokodemo-door",
			Settings: XrayDokodemoSettings{
				Address:        spec.TargetAddress,
				Port:           spec.TargetPort,
				Network:        spec.transportNetwork(),
				FollowRedirect: false,
			},
		})
		outbound := strings.TrimSpace(spec.OutboundTag)
		if outbound == "" {
			outbound = defaultOutbound
		}
		if outbound == "" {
			return fmt.Errorf("%w: proxy DNS endpoint %q has no proxy outbound to route through", ErrInvalidEgress, spec.ID)
		}
		rules = append(rules, XrayRoutingRule{
			Type:        "field",
			InboundTags: []string{tag},
			OutboundTag: outbound,
		})
	}

	config.Inbounds = inbounds
	if config.Routing == nil {
		config.Routing = &XrayRouting{Balancers: nil}
	}
	// DNS rules must be reachable. EnsureFailureFallback appends a terminal
	// blackhole catch-all; inserting before it keeps the endpoint deterministic
	// even when the apply order changes.
	config.Routing.Rules = insertBeforeTerminalFailure(config.Routing.Rules, rules...)
	return nil
}

// proxyDNSRoutingOnly reports whether a routing rule exists only to serve a
// generated DNS endpoint, so a reload can replace it safely.
func proxyDNSRoutingOnly(rule XrayRoutingRule) bool {
	if len(rule.InboundTags) == 0 {
		return false
	}
	for _, tag := range rule.InboundTags {
		if !strings.HasPrefix(tag, ProxyDNSEndpointTagPrefix) {
			return false
		}
	}
	return true
}

func insertBeforeTerminalFailure(rules []XrayRoutingRule, newRules ...XrayRoutingRule) []XrayRoutingRule {
	if len(newRules) == 0 {
		return rules
	}
	result := make([]XrayRoutingRule, 0, len(rules)+len(newRules))
	inserted := false
	for _, rule := range rules {
		if !inserted && isFailureCatchAll(rule) {
			result = append(result, newRules...)
			inserted = true
		}
		result = append(result, rule)
	}
	if !inserted {
		result = append(result, newRules...)
	}
	return result
}
