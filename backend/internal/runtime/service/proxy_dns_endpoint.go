package service

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"ly-route/backend/internal/runtime/dns"
	"ly-route/backend/internal/runtime/proxy"
)

// Proxy DNS endpoints.
//
// SmartDNS owns the single first-match policy table, but it cannot itself
// originate a query through the proxy egress: the egress is a VPP service
// chain reached with a socket mark, and plain DNS is not allowed to leak onto
// a direct line. The product therefore gives every (proxy egress, DNS
// upstream) combination one loopback endpoint inside Xray:
//
//	SmartDNS --udp/tcp/tls--> 127.0.0.1:<port>  (Xray dokodemo-door)
//	         --egress mark---> VPP proxy service chain --> upstream
//
// Ports are derived from the pair identity (not from rule order), so adding or
// deleting an unrelated rule never moves a live listener. Collisions inside
// one generation are resolved deterministically by linear probing.
const (
	// ProxyDNSEndpointListenAddress is the only accepted bind address. The
	// endpoint must never be reachable from a LAN or WAN interface.
	ProxyDNSEndpointListenAddress = "127.0.0.1"
	// ProxyDNSEndpointPortBase is the first port of the endpoint range. It is
	// deliberately outside the SmartDNS source-rule range (12000-19999), the
	// Xray capture listener range (20000-29999) and 53/853.
	ProxyDNSEndpointPortBase = 15353
	// ProxyDNSEndpointPortSpan is the number of ports reserved for endpoints.
	ProxyDNSEndpointPortSpan = 100
)

// ProxyDNSEndpoint is one local DNS transport endpoint bound to a proxy
// egress and a resolver reached through it.
type ProxyDNSEndpoint struct {
	ID            string                         `json:"id"`
	ProxyEgressID string                         `json:"proxy_egress_id"`
	UpstreamID    string                         `json:"upstream_id"`
	Protocol      proxy.ProxyDNSEndpointProtocol `json:"protocol"`
	ListenAddress string                         `json:"listen_address"`
	Port          int                            `json:"port"`
	ServerAddress string                         `json:"server_address"`
	ServerPort    int                            `json:"server_port"`
	ServerName    string                         `json:"server_name,omitempty"`
	OutboundTag   string                         `json:"outbound_tag"`
	FailureMode   dns.FailureMode                `json:"failure_mode,omitempty"`
	Fallback      string                         `json:"fallback_upstream_id,omitempty"`
}

// Listen returns the address SmartDNS must send queries to.
func (endpoint ProxyDNSEndpoint) Listen() string {
	return fmt.Sprintf("%s:%d", endpoint.ListenAddress, endpoint.Port)
}

// Group is the SmartDNS server-group name of this endpoint.
func (endpoint ProxyDNSEndpoint) Group() string {
	return dns.ProxyDNSEndpointGroup(endpoint.ProxyEgressID, endpoint.UpstreamID)
}

// Spec converts the endpoint into the Xray payload input.
func (endpoint ProxyDNSEndpoint) Spec() proxy.ProxyDNSEndpointSpec {
	return proxy.ProxyDNSEndpointSpec{
		ID:            endpoint.ID,
		EgressID:      endpoint.ProxyEgressID,
		UpstreamID:    endpoint.UpstreamID,
		Protocol:      endpoint.Protocol,
		Port:          endpoint.Port,
		TargetAddress: endpoint.ServerAddress,
		TargetPort:    endpoint.ServerPort,
		ServerName:    endpoint.ServerName,
		OutboundTag:   endpoint.OutboundTag,
		FailureMode:   string(endpoint.FailureMode),
	}
}

// ProxyDNSEndpointProblem explains an endpoint that could not be established.
// A problem is never silent: fail_closed/reject/fallback_direct handle it per
// the documented failure matrix and the runtime status reports it.
type ProxyDNSEndpointProblem struct {
	ProxyEgressID    string          `json:"proxy_egress_id"`
	UpstreamID       string          `json:"upstream_id"`
	FailureMode      dns.FailureMode `json:"failure_mode,omitempty"`
	FallbackUpstream string          `json:"fallback_upstream_id,omitempty"`
	Reason           string          `json:"reason"`
}

func (problem ProxyDNSEndpointProblem) Error() string {
	target := fmt.Sprintf("proxy egress %q", problem.ProxyEgressID)
	if problem.UpstreamID != "" {
		target += fmt.Sprintf(" upstream %q", problem.UpstreamID)
	}
	return fmt.Sprintf("proxy DNS endpoint for %s is unavailable: %s", target, problem.Reason)
}

// ProxyDNSEndpointPlan is the endpoint generation rendered into Xray and
// SmartDNS plus the explicit list of endpoints that could not be built.
type ProxyDNSEndpointPlan struct {
	Endpoints []ProxyDNSEndpoint        `json:"endpoints"`
	Problems  []ProxyDNSEndpointProblem `json:"problems,omitempty"`
}

// Empty reports whether the plan has no endpoints and no problems.
func (plan ProxyDNSEndpointPlan) Empty() bool {
	return len(plan.Endpoints) == 0 && len(plan.Problems) == 0
}

// Endpoint returns the endpoint serving the rule's (egress, upstream) pair.
func (plan ProxyDNSEndpointPlan) Endpoint(egressID, upstreamID string) (ProxyDNSEndpoint, bool) {
	egressID = strings.TrimSpace(egressID)
	upstreamID = strings.TrimSpace(upstreamID)
	for _, endpoint := range plan.Endpoints {
		if endpoint.ProxyEgressID == egressID && endpoint.UpstreamID == upstreamID {
			return endpoint, true
		}
	}
	return ProxyDNSEndpoint{}, false
}

// Problem returns the failure reason recorded for a pair.
func (plan ProxyDNSEndpointPlan) Problem(egressID, upstreamID string) (ProxyDNSEndpointProblem, bool) {
	egressID = strings.TrimSpace(egressID)
	upstreamID = strings.TrimSpace(upstreamID)
	for _, problem := range plan.Problems {
		if problem.ProxyEgressID == egressID && problem.UpstreamID == upstreamID {
			return problem, true
		}
	}
	return ProxyDNSEndpointProblem{}, false
}

// PlanProxyDNSEndpoints builds one endpoint per referenced (egress, upstream)
// combination. outboundTags maps an egress to the Xray outbound (or balancer)
// tag its DNS traffic must use; established marks egresses whose proxy runtime
// was compiled in this generation. A missing tag or an unestablished egress is
// reported as a problem instead of silently falling back to direct.
func PlanProxyDNSEndpoints(bindings []dns.ProxyDNSBinding, upstreams []SmartDNSUpstream, outboundTags map[string]string, established map[string]bool) ProxyDNSEndpointPlan {
	type bindingKey struct {
		egressID   string
		upstreamID string
		failure    dns.FailureMode
		fallback   string
	}
	keys := make([]bindingKey, 0, len(bindings))
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		key := bindingKey{
			egressID:   strings.TrimSpace(binding.ProxyEgressID),
			upstreamID: strings.TrimSpace(binding.UpstreamID),
			failure:    binding.FailureMode,
			fallback:   strings.TrimSpace(binding.FallbackUpstreamID),
		}
		if key.egressID == "" {
			continue
		}
		if key.failure == "" {
			key.failure = dns.FailureModeFailClosed
		}
		dedupe := key.egressID + "\x00" + key.upstreamID
		if _, exists := seen[dedupe]; exists {
			continue
		}
		seen[dedupe] = struct{}{}
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right bindingKey) int {
		if left.egressID != right.egressID {
			return strings.Compare(left.egressID, right.egressID)
		}
		return strings.Compare(left.upstreamID, right.upstreamID)
	})

	upstreamByID := make(map[string]SmartDNSUpstream, len(upstreams))
	for _, upstream := range upstreams {
		upstreamByID[strings.TrimSpace(upstream.ID)] = upstream
	}

	usedPorts := make(map[int]string, len(keys))
	plan := ProxyDNSEndpointPlan{Endpoints: make([]ProxyDNSEndpoint, 0, len(keys))}
	for _, key := range keys {
		problem := func(reason string) {
			plan.Problems = append(plan.Problems, ProxyDNSEndpointProblem{
				ProxyEgressID:    key.egressID,
				UpstreamID:       key.upstreamID,
				FailureMode:      key.failure,
				FallbackUpstream: key.fallback,
				Reason:           reason,
			})
		}
		if key.upstreamID == "" {
			problem("the rule does not name a DNS upstream; a proxy outcome must select the resolver reached through the egress (set upstream_id)")
			continue
		}
		upstream, exists := upstreamByID[key.upstreamID]
		if !exists {
			problem(fmt.Sprintf("DNS upstream %q is not configured or is disabled", key.upstreamID))
			continue
		}
		if pinned := strings.TrimSpace(upstream.WANEgressID); pinned != "" {
			problem(fmt.Sprintf("DNS upstream %q is pinned to WAN egress %q; a proxied resolver cannot keep a direct WAN binding", key.upstreamID, pinned))
			continue
		}
		if established != nil && !established[key.egressID] {
			problem(fmt.Sprintf("proxy egress %q has no compiled runtime in this generation (the proxy subscription/node did not compile); fix the egress before DNS can be proxied", key.egressID))
			continue
		}
		protocol, serverAddress, serverPort, serverName, err := proxyDNSEndpointTarget(upstream)
		if err != nil {
			problem(err.Error())
			continue
		}
		outboundTag := strings.TrimSpace(outboundTags[key.egressID])
		if outboundTag == "" {
			outboundTag = key.egressID
		}
		endpoint := ProxyDNSEndpoint{
			ID:            dns.ProxyDNSEndpointID(key.egressID, key.upstreamID),
			ProxyEgressID: key.egressID,
			UpstreamID:    key.upstreamID,
			Protocol:      protocol,
			ListenAddress: ProxyDNSEndpointListenAddress,
			ServerAddress: serverAddress,
			ServerPort:    serverPort,
			ServerName:    serverName,
			OutboundTag:   outboundTag,
			FailureMode:   key.failure,
			Fallback:      key.fallback,
		}
		endpoint.Port = allocateProxyDNSEndpointPort(key.egressID, key.upstreamID, usedPorts)
		if endpoint.Port == 0 {
			problem(fmt.Sprintf("the proxy DNS endpoint port range %d-%d is exhausted", ProxyDNSEndpointPortBase, ProxyDNSEndpointPortBase+ProxyDNSEndpointPortSpan-1))
			continue
		}
		usedPorts[endpoint.Port] = endpoint.ID
		plan.Endpoints = append(plan.Endpoints, endpoint)
	}
	return plan
}

// allocateProxyDNSEndpointPort returns the port for a pair: hashed for
// stability, then linearly probed inside the reserved range.
func allocateProxyDNSEndpointPort(egressID, upstreamID string, used map[int]string) int {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(egressID)) + "\x00" + strings.ToLower(strings.TrimSpace(upstreamID))))
	offset := int(binary.BigEndian.Uint32(digest[:4]) % ProxyDNSEndpointPortSpan)
	for attempt := 0; attempt < ProxyDNSEndpointPortSpan; attempt++ {
		port := ProxyDNSEndpointPortBase + (offset+attempt)%ProxyDNSEndpointPortSpan
		if _, exists := used[port]; exists {
			continue
		}
		return port
	}
	return 0
}

// proxyDNSEndpointTarget resolves the wire transport of an upstream into a
// literal target address. The product only ever dials a literal from Xray: a
// hostname target would be resolved by the host resolver (SmartDNS), which is
// exactly the recursion the dependency check forbids.
func proxyDNSEndpointTarget(upstream SmartDNSUpstream) (proxy.ProxyDNSEndpointProtocol, string, int, string, error) {
	if len(upstream.Servers) == 0 {
		return "", "", 0, "", fmt.Errorf("DNS upstream %q has no servers", upstream.ID)
	}
	raw := strings.TrimSpace(upstream.Servers[0])
	if raw == "" {
		return "", "", 0, "", fmt.Errorf("DNS upstream %q has an empty server", upstream.ID)
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "h3://"), strings.HasPrefix(lower, "quic://"):
		return "", "", 0, "", fmt.Errorf("DNS upstream %q uses %s: HTTP/3 and QUIC upstreams cannot be proxied by a local TCP/UDP endpoint; use https://, tls:// or a plain resolver", upstream.ID, strings.SplitN(lower, "://", 2)[0])
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "tls://"):
		protocol := proxy.ProxyDNSEndpointDoH
		defaultPort := 443
		if strings.HasPrefix(lower, "tls://") {
			protocol = proxy.ProxyDNSEndpointDoT
			defaultPort = 853
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q has an invalid %s endpoint", upstream.ID, protocol)
		}
		host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
		port := defaultPort
		if parsed.Port() != "" {
			parsedPort, portErr := parsePort(parsed.Port())
			if portErr != nil {
				return "", "", 0, "", fmt.Errorf("DNS upstream %q has an invalid port: %v", upstream.ID, portErr)
			}
			port = parsedPort
		}
		if address, parseErr := netip.ParseAddr(host); parseErr == nil {
			return protocol, address.String(), port, host, nil
		}
		var chosen netip.Addr
		for _, candidate := range upstream.ResolvedHostIPs[host] {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(candidate))
			if parseErr == nil && address.Is4() {
				chosen = address
				break
			}
		}
		if !chosen.IsValid() {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q host %q has no resolved IPv4 address; apply the upstream once with a reachable bootstrap resolver (or use a literal resolver address)", upstream.ID, host)
		}
		return protocol, chosen.String(), port, host, nil
	case strings.HasPrefix(lower, "tcp://"), strings.HasPrefix(lower, "udp://"):
		protocol := proxy.ProxyDNSEndpointTCP
		if strings.HasPrefix(lower, "udp://") {
			protocol = proxy.ProxyDNSEndpointUDP
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q has an invalid %s endpoint", upstream.ID, protocol)
		}
		host := strings.TrimSpace(parsed.Hostname())
		port := 53
		if parsed.Port() != "" {
			parsedPort, portErr := parsePort(parsed.Port())
			if portErr != nil {
				return "", "", 0, "", fmt.Errorf("DNS upstream %q has an invalid port: %v", upstream.ID, portErr)
			}
			port = parsedPort
		}
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q uses hostname %q for a plain %s resolver; use a literal IP address", upstream.ID, host, protocol)
		}
		return protocol, address.String(), port, "", nil
	}

	host := raw
	port := 53
	if parsedHost, parsedPort, err := net.SplitHostPort(raw); err == nil {
		host = parsedHost
		parsedValue, portErr := parsePort(parsedPort)
		if portErr != nil {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q server %q is invalid: %v", upstream.ID, raw, portErr)
		}
		port = parsedValue
	} else if strings.Count(raw, ":") > 1 {
		// A bare IPv6 literal without a port.
		if _, ipErr := netip.ParseAddr(raw); ipErr != nil {
			return "", "", 0, "", fmt.Errorf("DNS upstream %q server %q is ambiguous; write [address]:port", upstream.ID, raw)
		}
	}
	address, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
	if parseErr != nil {
		return "", "", 0, "", fmt.Errorf("DNS upstream %q uses hostname %q for a plain resolver; use a literal IP address so the query cannot be resolved by the resolver it is trying to reach", upstream.ID, host)
	}
	return proxy.ProxyDNSEndpointUDP, address.String(), port, "", nil
}

func parsePort(value string) (int, error) {
	port := 0
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("port is empty")
	}
	for _, character := range trimmed {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("port %q is not numeric", value)
		}
		port = port*10 + int(character-'0')
		if port > 65535 {
			return 0, fmt.Errorf("port %q is out of range", value)
		}
	}
	if port < 1 {
		return 0, fmt.Errorf("port %q is out of range", value)
	}
	return port, nil
}
