package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type SemanticType string

const (
	ProxyEgress SemanticType = "proxy_egress"
	PhysicalWAN SemanticType = "physical_wan"
)

const WANDisplayList = "wan"

type CapturePath string

const (
	VPPService CapturePath = "vpp_service_interface"
	TProxy     CapturePath = "tproxy"
)

type Engine string

const Xray Engine = "xray"

type ListenerMode string

const (
	VPPServiceListener ListenerMode = "vpp-service"
	DokodemoDoor       ListenerMode = "dokodemo-door"
	Transparent        ListenerMode = "transparent"
)

type DataplaneHandoff string

const (
	VPPToService DataplaneHandoff = "vpp_to_service"
	VPPToHost    DataplaneHandoff = "vpp_to_host"
	VPPToTap     DataplaneHandoff = "vpp_to_tap"
)

type RuntimeProfile string

type Egress struct {
	ID             string           `json:"id"`
	SemanticType   SemanticType     `json:"semantic_type"`
	DisplayList    string           `json:"display_list"`
	RuntimeProfile RuntimeProfile   `json:"runtime_profile"`
	UnderlayWANID  string           `json:"underlay_wan_id,omitempty"`
	NodeID         string           `json:"node_id,omitempty"`
	SubscriptionID string           `json:"subscription_id,omitempty"`
	CapturePath    CapturePath      `json:"capture_path"`
	Engine         Engine           `json:"engine"`
	Handoff        DataplaneHandoff `json:"handoff"`
	ListenerMode   ListenerMode     `json:"listener_mode,omitempty"`
}

type LogicalWANRow struct {
	ID             string           `json:"id"`
	SemanticType   SemanticType     `json:"semantic_type"`
	DisplayList    string           `json:"display_list"`
	RuntimeProfile RuntimeProfile   `json:"runtime_profile"`
	UnderlayWANID  string           `json:"underlay_wan_id,omitempty"`
	NodeID         string           `json:"node_id,omitempty"`
	SubscriptionID string           `json:"subscription_id,omitempty"`
	CapturePath    CapturePath      `json:"capture_path"`
	Engine         Engine           `json:"engine"`
	Handoff        DataplaneHandoff `json:"handoff"`
	ListenerMode   ListenerMode     `json:"listener_mode,omitempty"`
}

type ServiceTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type DataplaneTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type VPPSteeringInstruction struct {
	Order            int                 `json:"order"`
	EgressID         string              `json:"egress_id"`
	UnderlayWANID    string              `json:"underlay_wan_id,omitempty"`
	UnderlayRoute    string              `json:"underlay_route,omitempty"`
	Handoff          DataplaneHandoff    `json:"handoff"`
	TargetKind       string              `json:"target_kind"`
	Action           string              `json:"action"`
	AttachmentTarget string              `json:"attachment_target"`
	ServiceNetwork   ProxyServiceNetwork `json:"service_network,omitempty"`
}

type NftablesCapturePlan struct {
	EgressID         string `json:"egress_id"`
	Family           string `json:"family"`
	Table            string `json:"table"`
	TargetPort       int    `json:"target_port"`
	Mark             string `json:"mark"`
	InboundMark      string `json:"inbound_mark,omitempty"`
	IngressInterface string `json:"ingress_interface,omitempty"`
	// BypassDestinations are IPv4 prefixes that must never enter the proxy
	// socket. VPP-selected traffic can still include LAN, link-local or the
	// proxy service networks themselves; capturing those would blackhole
	// same-source LAN upstreams or loop the proxy handoff.
	BypassDestinations []string `json:"bypass_destinations,omitempty"`
	// FailOpen marks a deliberately empty capture generation. The table is
	// flushed and left with policy accept so traffic continues on the direct
	// path while the proxy runtime is degraded or unhealthy.
	FailOpen bool            `json:"fail_open,omitempty"`
	Chains   []NftablesChain `json:"chains"`
	Rules    []NftablesRule  `json:"rules"`
}

type NftablesChain struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Hook     string `json:"hook"`
	Priority int    `json:"priority"`
	Policy   string `json:"policy"`
}

type NftablesRule struct {
	Order      int    `json:"order"`
	EgressID   string `json:"egress_id"`
	Chain      string `json:"chain"`
	Expression string `json:"expression"`
	Action     string `json:"action"`
}

type LinuxPolicyRoutingPlan struct {
	EgressID      string              `json:"egress_id"`
	Mark          string              `json:"mark"`
	MarkValue     uint32              `json:"mark_value"`
	MarkMask      string              `json:"mark_mask"`
	TableID       int                 `json:"table_id"`
	TableName     string              `json:"table_name"`
	RulePriority  int                 `json:"rule_priority"`
	RuleSelector  LinuxRuleSelector   `json:"rule_selector"`
	DefaultRoute  LinuxDefaultRoute   `json:"default_route"`
	Underlay      LinuxUnderlayRoute  `json:"underlay"`
	Network       ProxyServiceNetwork `json:"network,omitempty"`
	LANRoutes     []string            `json:"lan_routes,omitempty"`
	UnderlayRoute string              `json:"underlay_route,omitempty"`
}

type LinuxRuleSelector struct {
	Family string `json:"family"`
	Mark   string `json:"mark"`
	Mask   string `json:"mask"`
	Table  int    `json:"table"`
}

type LinuxDefaultRoute struct {
	Destination string `json:"destination"`
	Table       int    `json:"table"`
	Via         string `json:"via"`
	Device      string `json:"device"`
	Scope       string `json:"scope"`
}

type LinuxUnderlayRoute struct {
	EgressID string `json:"egress_id"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
}

type XrayRuntime struct {
	EgressID          string              `json:"egress_id"`
	Engine            Engine              `json:"engine"`
	Mode              ListenerMode        `json:"mode"`
	ProcessName       string              `json:"process_name"`
	ConfigPath        string              `json:"config_path"`
	HealthCheckTarget string              `json:"health_check_target"`
	ListenAddress     string              `json:"listen_address"`
	ListenPort        int                 `json:"listen_port"`
	ListenerTarget    string              `json:"listener_target"`
	Network           string              `json:"network"`
	Protocol          string              `json:"protocol"`
	OutboundTag       string              `json:"outbound_tag"`
	ServiceNetwork    ProxyServiceNetwork `json:"service_network"`
	ConfigPayload     XrayConfigPayload   `json:"config_payload"`
}

type XrayConfigPayload struct {
	Log         XrayLog          `json:"log"`
	API         *XrayAPI         `json:"api,omitempty"`
	Metrics     *XrayMetrics     `json:"metrics,omitempty"`
	Inbounds    []XrayInbound    `json:"inbounds"`
	Outbounds   []XrayOutbound   `json:"outbounds"`
	Routing     *XrayRouting     `json:"routing,omitempty"`
	Observatory *XrayObservatory `json:"observatory,omitempty"`
}

type XrayMetrics struct {
	Tag    string `json:"tag,omitempty"`
	Listen string `json:"listen"`
}

type XrayLog struct {
	Level string `json:"level"`
}

type XrayInbound struct {
	Tag            string               `json:"tag"`
	Listen         string               `json:"listen"`
	Port           int                  `json:"port"`
	Protocol       string               `json:"protocol"`
	Settings       XrayDokodemoSettings `json:"settings"`
	StreamSettings map[string]any       `json:"streamSettings,omitempty"`
}

type XrayDokodemoSettings struct {
	Address        string `json:"address,omitempty"`
	Network        string `json:"network"`
	FollowRedirect bool   `json:"followRedirect"`
}

type XrayOutbound struct {
	Tag            string         `json:"tag"`
	Protocol       string         `json:"protocol"`
	Settings       map[string]any `json:"settings,omitempty"`
	StreamSettings map[string]any `json:"streamSettings,omitempty"`
}

type XrayRouting struct {
	Rules     []XrayRoutingRule `json:"rules"`
	Balancers []XrayBalancer    `json:"balancers"`
}

type XrayRoutingRule struct {
	Type        string   `json:"type"`
	InboundTags []string `json:"inboundTag,omitempty"`
	Network     string   `json:"network,omitempty"`
	BalancerTag string   `json:"balancerTag,omitempty"`
	OutboundTag string   `json:"outboundTag,omitempty"`
}

type XrayAPI struct {
	Tag      string   `json:"tag"`
	Services []string `json:"services"`
}

type XrayBalancer struct {
	Tag         string               `json:"tag"`
	Selector    []string             `json:"selector"`
	FallbackTag string               `json:"fallbackTag,omitempty"`
	Strategy    XrayBalancerStrategy `json:"strategy"`
}

type XrayBalancerStrategy struct {
	Type     string         `json:"type"`
	Settings map[string]any `json:"settings"`
}

type XrayObservatory struct {
	SubjectSelector   []string `json:"subjectSelector"`
	ProbeURL          string   `json:"probeUrl"`
	ProbeInterval     string   `json:"probeInterval"`
	EnableConcurrency bool     `json:"enableConcurrency"`
}

type Node struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Protocol string         `json:"protocol"`
	Address  string         `json:"address"`
	Port     int            `json:"port"`
	Secret   string         `json:"secret,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

type Subscription struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	URL       string        `json:"url"`
	Enabled   bool          `json:"enabled"`
	NodeRefs  []string      `json:"node_refs,omitempty"`
	Selection SelectionMode `json:"selection,omitempty"`
	Strategy  string        `json:"strategy,omitempty"`
	TopN      int           `json:"top_n,omitempty"`
}

type RedactedNode struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Protocol string         `json:"protocol"`
	Address  string         `json:"address"`
	Port     int            `json:"port"`
	Secret   string         `json:"secret,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

type RedactedSubscription struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	URL       string        `json:"url"`
	Enabled   bool          `json:"enabled"`
	NodeRefs  []string      `json:"node_refs,omitempty"`
	Selection SelectionMode `json:"selection,omitempty"`
	Strategy  string        `json:"strategy,omitempty"`
	TopN      int           `json:"top_n,omitempty"`
}

type CompiledEgress struct {
	ID                 string                   `json:"id"`
	SemanticType       SemanticType             `json:"semantic_type"`
	DisplayList        string                   `json:"display_list"`
	RuntimeProfile     RuntimeProfile           `json:"runtime_profile"`
	UnderlayWANID      string                   `json:"underlay_wan_id,omitempty"`
	CapturePath        CapturePath              `json:"capture_path"`
	Engine             Engine                   `json:"engine"`
	Handoff            DataplaneHandoff         `json:"handoff"`
	ListenerMode       ListenerMode             `json:"listener_mode"`
	ServiceTargets     []ServiceTarget          `json:"service_targets"`
	DataplaneTargets   []DataplaneTarget        `json:"dataplane_targets"`
	VPPSteering        []VPPSteeringInstruction `json:"vpp_steering"`
	NftablesCapture    NftablesCapturePlan      `json:"nftables_capture"`
	LinuxPolicyRouting LinuxPolicyRoutingPlan   `json:"linux_policy_routing"`
	XrayRuntime        XrayRuntime              `json:"xray_runtime"`
	ServiceNetwork     ProxyServiceNetwork      `json:"service_network"`
}

var ErrInvalidEgress = errors.New("invalid proxy egress")

var physicalOnlyJSONFields = map[string]struct{}{
	"physical_interface_id":       {},
	"physical_interface_identity": {},
	"interface_id":                {},
	"interface_name":              {},
	"mac":                         {},
	"mac_address":                 {},
	"mac_negotiation":             {},
	"link_speed":                  {},
	"speed":                       {},
	"pppoe":                       {},
	"pppoe_username":              {},
	"pppoe_password":              {},
	"dhcp_client":                 {},
}

func NewProxyEgress(id string, runtimeProfile RuntimeProfile) Egress {
	return Egress{
		ID:             id,
		SemanticType:   ProxyEgress,
		DisplayList:    WANDisplayList,
		RuntimeProfile: runtimeProfile,
		CapturePath:    VPPService,
		Engine:         Xray,
		Handoff:        VPPToService,
		ListenerMode:   VPPServiceListener,
	}
}

func (egress *Egress) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for field := range fields {
		if _, forbidden := physicalOnlyJSONFields[field]; forbidden {
			return fmt.Errorf("%w: field %q is physical WAN-only", ErrInvalidEgress, field)
		}
	}

	type egressJSON Egress
	var decoded egressJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*egress = Egress(decoded)
	return nil
}

func (egress Egress) LogicalWANRow() (LogicalWANRow, error) {
	if err := ValidateEgress(egress); err != nil {
		return LogicalWANRow{}, err
	}

	return LogicalWANRow{
		ID:             egress.ID,
		SemanticType:   egress.SemanticType,
		DisplayList:    egress.DisplayList,
		RuntimeProfile: egress.RuntimeProfile,
		UnderlayWANID:  strings.TrimSpace(egress.UnderlayWANID),
		NodeID:         strings.TrimSpace(egress.NodeID),
		SubscriptionID: strings.TrimSpace(egress.SubscriptionID),
		CapturePath:    egress.CapturePath,
		Engine:         egress.Engine,
		Handoff:        egress.Handoff,
		ListenerMode:   normalizedListenerMode(egress.ListenerMode),
	}, nil
}

func ValidateEgress(egress Egress) error {
	if strings.TrimSpace(egress.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidEgress)
	}
	if egress.SemanticType != ProxyEgress {
		return fmt.Errorf("%w: semantic_type must be %q", ErrInvalidEgress, ProxyEgress)
	}
	if egress.SemanticType == PhysicalWAN {
		return fmt.Errorf("%w: physical WAN semantics are not valid for proxy egress", ErrInvalidEgress)
	}
	if egress.DisplayList != WANDisplayList {
		return fmt.Errorf("%w: display_list must be %q", ErrInvalidEgress, WANDisplayList)
	}
	if strings.TrimSpace(string(egress.RuntimeProfile)) == "" {
		return fmt.Errorf("%w: runtime_profile is required", ErrInvalidEgress)
	}
	if egress.CapturePath != VPPService {
		return fmt.Errorf("%w: capture_path must be %q", ErrInvalidEgress, VPPService)
	}
	if egress.Engine != Xray {
		return fmt.Errorf("%w: engine must be %q", ErrInvalidEgress, Xray)
	}
	if !isSupportedHandoff(egress.Handoff) {
		return fmt.Errorf("%w: handoff must be %q", ErrInvalidEgress, VPPToService)
	}
	if !isSupportedListenerMode(egress.ListenerMode) {
		return fmt.Errorf("%w: listener_mode must be %q", ErrInvalidEgress, VPPServiceListener)
	}
	if strings.TrimSpace(egress.NodeID) != "" && strings.TrimSpace(egress.SubscriptionID) != "" {
		return fmt.Errorf("%w: node_id and subscription_id are mutually exclusive", ErrInvalidEgress)
	}

	return nil
}

func CompileEgress(egress Egress) (CompiledEgress, error) {
	if err := ValidateEgress(egress); err != nil {
		return CompiledEgress{}, err
	}

	network := ServiceNetworkForEgressID(egress.ID)
	dataplaneTargets := []DataplaneTarget{
		{Kind: "vpp.proxy-service.network", ID: egress.ID},
	}

	listenerMode := normalizedListenerMode(egress.ListenerMode)
	capture := compileNftablesCaptureForNetwork(egress, network)
	routing, err := compileLinuxPolicyRoutingForNetwork(egress, network, capture)
	if err != nil {
		return CompiledEgress{}, err
	}

	return CompiledEgress{
		ID:             egress.ID,
		SemanticType:   egress.SemanticType,
		DisplayList:    egress.DisplayList,
		RuntimeProfile: egress.RuntimeProfile,
		UnderlayWANID:  strings.TrimSpace(egress.UnderlayWANID),
		CapturePath:    egress.CapturePath,
		Engine:         egress.Engine,
		Handoff:        egress.Handoff,
		ListenerMode:   listenerMode,
		ServiceTargets: []ServiceTarget{
			{Kind: "proxy.runtime.xray-config", ID: egress.ID},
			{Kind: "proxy.runtime.listener", ID: egress.ID},
			{Kind: "proxy.runtime.upstream", ID: egress.ID},
			{Kind: "proxy.runtime.health-check", ID: egress.ID},
			{Kind: "proxy.runtime.traffic-counters", ID: egress.ID},
			{Kind: "proxy.runtime.dns-policy-binding", ID: egress.ID},
		},
		DataplaneTargets:   dataplaneTargets,
		VPPSteering:        compileVPPSteering(egress, dataplaneTargets),
		NftablesCapture:    capture,
		LinuxPolicyRouting: routing,
		XrayRuntime:        compileXrayRuntime(egress, network, listenerMode),
		ServiceNetwork:     network,
	}, nil
}

func RedactNode(node Node) (RedactedNode, error) {
	if strings.TrimSpace(node.ID) == "" || strings.TrimSpace(node.Protocol) == "" || strings.TrimSpace(node.Address) == "" || node.Port <= 0 {
		return RedactedNode{}, fmt.Errorf("%w: node requires id, protocol, address, and port", ErrInvalidEgress)
	}
	return RedactedNode{ID: strings.TrimSpace(node.ID), Name: strings.TrimSpace(node.Name), Protocol: strings.TrimSpace(node.Protocol), Address: strings.TrimSpace(node.Address), Port: node.Port, Secret: redactedSecret(node.Secret), Settings: cloneSettings(node.Settings)}, nil
}

func RedactSubscription(subscription Subscription) (RedactedSubscription, error) {
	if strings.TrimSpace(subscription.ID) == "" || strings.TrimSpace(subscription.URL) == "" {
		return RedactedSubscription{}, fmt.Errorf("%w: subscription requires id and url", ErrInvalidEgress)
	}
	return RedactedSubscription{
		ID:        strings.TrimSpace(subscription.ID),
		Name:      strings.TrimSpace(subscription.Name),
		URL:       redactURL(subscription.URL),
		Enabled:   subscription.Enabled,
		NodeRefs:  append([]string(nil), subscription.NodeRefs...),
		Selection: subscription.Selection,
		Strategy:  strings.TrimSpace(subscription.Strategy),
		TopN:      subscription.TopN,
	}, nil
}

func CompileNodeOutbound(node Node) (XrayOutbound, error) {
	redacted, err := RedactNode(node)
	if err != nil {
		return XrayOutbound{}, err
	}
	secret := strings.TrimSpace(node.Secret)
	if secret == "" {
		return XrayOutbound{}, fmt.Errorf("%w: node %q requires a runtime credential", ErrInvalidEgress, redacted.ID)
	}
	protocol := strings.ToLower(strings.TrimSpace(redacted.Protocol))
	stream, err := compileStreamSettings(node.Settings)
	if err != nil {
		return XrayOutbound{}, fmt.Errorf("%w: node %q stream settings: %v", ErrInvalidEgress, redacted.ID, err)
	}
	outbound := XrayOutbound{Tag: "node-" + redacted.ID, Protocol: protocol, StreamSettings: stream}
	switch protocol {
	case "vless":
		user := map[string]any{"id": secret, "encryption": settingString(node.Settings, "encryption", "none")}
		if flow := settingString(node.Settings, "flow", ""); flow != "" {
			user["flow"] = flow
		}
		outbound.Settings = map[string]any{"vnext": []any{map[string]any{"address": redacted.Address, "port": redacted.Port, "users": []any{user}}}}
	case "vmess":
		user := map[string]any{"id": secret, "security": settingString(node.Settings, "cipher", "auto")}
		outbound.Settings = map[string]any{"vnext": []any{map[string]any{"address": redacted.Address, "port": redacted.Port, "users": []any{user}}}}
	case "trojan":
		outbound.Settings = map[string]any{"servers": []any{map[string]any{"address": redacted.Address, "port": redacted.Port, "password": secret}}}
	case "shadowsocks":
		method := settingString(node.Settings, "method", "")
		if method == "" {
			return XrayOutbound{}, fmt.Errorf("%w: shadowsocks node %q requires method", ErrInvalidEgress, redacted.ID)
		}
		outbound.Settings = map[string]any{"servers": []any{map[string]any{"address": redacted.Address, "port": redacted.Port, "password": secret, "method": method}}}
	default:
		return XrayOutbound{}, fmt.Errorf("%w: node %q uses unsupported protocol %q", ErrInvalidEgress, redacted.ID, protocol)
	}
	return outbound, nil
}

func compileStreamSettings(settings map[string]any) (map[string]any, error) {
	if len(settings) == 0 {
		return nil, nil
	}
	stream := make(map[string]any)
	for _, key := range []string{"network", "security", "tlsSettings", "realitySettings", "wsSettings", "grpcSettings", "tcpSettings", "httpSettings", "sockopt"} {
		if value, exists := settings[key]; exists {
			stream[key] = value
		}
	}
	allowed := map[string]bool{"network": true, "security": true, "tlsSettings": true, "realitySettings": true, "wsSettings": true, "grpcSettings": true, "tcpSettings": true, "httpSettings": true, "sockopt": true, "encryption": true, "flow": true, "cipher": true, "method": true}
	for key := range settings {
		if !allowed[key] {
			return nil, fmt.Errorf("unsupported field %q", key)
		}
	}
	if len(stream) == 0 {
		return nil, nil
	}
	return stream, nil
}

func settingString(settings map[string]any, key, fallback string) string {
	value, ok := settings[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func redactedSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "redacted"
}

func redactURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if idx := strings.Index(trimmed, "://"); idx > 0 {
		return trimmed[:idx+3] + "redacted"
	}
	return "redacted"
}

func cloneSettings(settings map[string]any) map[string]any {
	if len(settings) == 0 {
		return map[string]any{}
	}
	clone := make(map[string]any, len(settings))
	for key, value := range settings {
		clone[key] = value
	}
	return clone
}

func isSupportedHandoff(handoff DataplaneHandoff) bool {
	return handoff == VPPToService
}

func isSupportedListenerMode(mode ListenerMode) bool {
	return mode == "" || mode == VPPServiceListener
}

func normalizedListenerMode(mode ListenerMode) ListenerMode {
	if mode == "" {
		return VPPServiceListener
	}

	return mode
}

func compileVPPSteering(egress Egress, targets []DataplaneTarget) []VPPSteeringInstruction {
	action, attachmentTarget := vppHandoffAction(egress.Handoff)
	network := ServiceNetworkForEgressID(egress.ID)
	steering := make([]VPPSteeringInstruction, len(targets))
	for i, target := range targets {
		steering[i] = VPPSteeringInstruction{
			Order:            i + 1,
			EgressID:         egress.ID,
			UnderlayWANID:    strings.TrimSpace(egress.UnderlayWANID),
			UnderlayRoute:    network.UnderlayRoute,
			Handoff:          egress.Handoff,
			TargetKind:       target.Kind,
			Action:           action,
			AttachmentTarget: attachmentTarget,
			ServiceNetwork:   network,
		}
	}

	return steering
}

func vppHandoffAction(handoff DataplaneHandoff) (string, string) {
	return "handoff.to-vpp-service", "vpp.proxy-service"
}

func compileNftablesCapture(egress Egress) NftablesCapturePlan {
	return compileNftablesCaptureForNetwork(egress, ServiceNetworkForEgressID(egress.ID))
}

// Proxy capture identities. The table is owned exclusively by Ly Route, so a
// reload can flush only this table and never touch host firewall or other
// container rules.
const (
	ProxyCaptureFamily = "inet"
	ProxyCaptureTable  = "ly_route_proxy_capture"
	ProxyCaptureChain  = "proxy_prerouting"

	// ProxyDirectOutboundTag is the legacy fail-open freedom outbound. It is
	// deliberately removed by EnsureFailureFallback: a captured packet must
	// never be able to reach the direct Internet path when the proxy is down.
	ProxyDirectOutboundTag = "direct"
	// ProxyFailureOutboundTag is the blackhole outbound every proxy balancer
	// falls back to, and the terminal routing catch-all target.
	ProxyFailureOutboundTag = "failure"
	// ProxyProbeURL is the observatory probe target. The probe is performed
	// through each proxy outbound, never on the direct path.
	ProxyProbeURL = "https://www.gstatic.com/generate_204"
	// DefaultXrayMetricsListen exposes observatory state for every proxy mode.
	DefaultXrayMetricsListen = "127.0.0.1:11111"
)

// NftablesBypassDestinations returns the IPv4 prefixes that must stay on the
// direct path. The proxy service /30 peers are always included, as are the
// reserved ranges that can never legitimately terminate in a proxy socket.
func NftablesBypassDestinations(network ProxyServiceNetwork, lanRoutes []string) []string {
	candidates := []string{
		"0.0.0.0/8",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"255.255.255.255/32",
		network.IngressCIDR,
		network.EgressCIDR,
	}
	candidates = append(candidates, lanRoutes...)
	seen := map[string]struct{}{}
	destinations := make([]string, 0, len(candidates))
	for _, raw := range candidates {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		canonical := prefix.Masked().String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		destinations = append(destinations, canonical)
	}
	return destinations
}

// CaptureGuardRules builds the non-TPROXY prefix of the capture chain:
// already-marked packets and everything that must stay direct return before
// the terminal tproxy statements. Because the base chain policy is accept, an
// empty guard/chain is fail-open by construction.
func CaptureGuardRules(plan NftablesCapturePlan) []NftablesRule {
	ingress := strings.TrimSpace(plan.IngressInterface)
	chain := CaptureChainName(plan)
	mark := strings.TrimSpace(plan.InboundMark)
	if mark == "" {
		mark = strings.TrimSpace(plan.Mark)
	}
	if ingress == "" || chain == "" {
		return nil
	}
	ipv4 := fmt.Sprintf("iifname %q meta nfproto ipv4", ingress)
	rules := make([]NftablesRule, 0, 4+len(plan.BypassDestinations))
	add := func(expression, action string) {
		rules = append(rules, NftablesRule{
			Order:      len(rules) + 1,
			EgressID:   plan.EgressID,
			Chain:      chain,
			Expression: expression,
			Action:     action,
		})
	}
	if mark != "" {
		add(fmt.Sprintf("iifname %q meta mark %s", ingress, mark), "counter return")
	}
	add(fmt.Sprintf("iifname != %q", ingress), "counter return")
	for _, protocol := range []string{"tcp", "udp"} {
		// Keep the transport keyword explicit: `meta l4proto tcp dport` has no
		// symbol type information and is rejected by nft.
		add(ipv4+" "+protocol+" dport 53", "counter return")
	}
	for _, destination := range plan.BypassDestinations {
		add(ipv4+" ip daddr "+destination, "counter return")
	}
	return rules
}

// CaptureChainName returns the managed capture chain for a plan.
func CaptureChainName(plan NftablesCapturePlan) string {
	if len(plan.Chains) > 0 && strings.TrimSpace(plan.Chains[0].Name) != "" {
		return strings.TrimSpace(plan.Chains[0].Name)
	}
	return ProxyCaptureChain
}

// FailOpenNftablesCapture returns a valid capture generation that flushes the
// managed table and installs no TPROXY statement. It is used when a proxy
// egress exists but its runtime could not be compiled or verified: selected
// traffic then falls through the accept policy and stays on the direct path.
func FailOpenNftablesCapture(egressID string) NftablesCapturePlan {
	return NftablesCapturePlan{
		EgressID: strings.TrimSpace(egressID),
		Family:   ProxyCaptureFamily,
		Table:    ProxyCaptureTable,
		FailOpen: true,
	}
}

func compileNftablesCaptureForNetwork(egress Egress, network ProxyServiceNetwork) NftablesCapturePlan {
	return buildNftablesCapturePlan(egress.ID, network, network.LANRoutes)
}

func buildNftablesCapturePlan(egressID string, network ProxyServiceNetwork, lanRoutes []string) NftablesCapturePlan {
	mark := fmt.Sprintf("0x%x", network.IngressMark)
	plan := NftablesCapturePlan{
		EgressID:           egressID,
		Family:             ProxyCaptureFamily,
		Table:              ProxyCaptureTable,
		TargetPort:         network.ListenerPort,
		Mark:               mark,
		InboundMark:        mark,
		IngressInterface:   network.IngressHostInterface,
		BypassDestinations: NftablesBypassDestinations(network, lanRoutes),
		Chains: []NftablesChain{
			{Name: ProxyCaptureChain, Type: "filter", Hook: "prerouting", Priority: -150, Policy: "accept"},
		},
	}
	rules := CaptureGuardRules(plan)
	ipv4 := fmt.Sprintf("iifname %q meta nfproto ipv4", network.IngressHostInterface)
	for _, protocol := range []string{"tcp", "udp"} {
		rules = append(rules, NftablesRule{
			Order:      len(rules) + 1,
			EgressID:   egressID,
			Chain:      ProxyCaptureChain,
			Expression: ipv4 + " meta l4proto " + protocol,
			Action:     fmt.Sprintf("counter meta mark set %s tproxy ip to :%d accept", mark, network.ListenerPort),
		})
	}
	plan.Rules = rules
	return plan
}

// refreshCaptureRules regenerates the guard prefix while preserving any
// per-lane terminal TPROXY rules produced by adaptive subscription routing.
func refreshCaptureRules(plan NftablesCapturePlan, network ProxyServiceNetwork, lanRoutes []string) NftablesCapturePlan {
	plan.EgressID = network.EgressID
	plan.Family = ProxyCaptureFamily
	plan.Table = ProxyCaptureTable
	plan.TargetPort = network.ListenerPort
	plan.Mark = fmt.Sprintf("0x%x", network.IngressMark)
	plan.InboundMark = plan.Mark
	plan.IngressInterface = network.IngressHostInterface
	plan.BypassDestinations = NftablesBypassDestinations(network, lanRoutes)
	if len(plan.Chains) == 0 {
		plan.Chains = []NftablesChain{{Name: ProxyCaptureChain, Type: "filter", Hook: "prerouting", Priority: -150, Policy: "accept"}}
	}
	terminal := make([]NftablesRule, 0, len(plan.Rules))
	for _, rule := range plan.Rules {
		if strings.TrimSpace(rule.Action) == "counter return" || strings.TrimSpace(rule.Action) == "return" {
			continue
		}
		terminal = append(terminal, rule)
	}
	chain := CaptureChainName(plan)
	plan.Rules = make([]NftablesRule, 0, len(terminal)+8)
	for index := range terminal {
		terminal[index].Chain = chain
		terminal[index].EgressID = plan.EgressID
	}
	guard := CaptureGuardRules(plan)
	plan.Rules = append(guard, terminal...)
	for index := range plan.Rules {
		plan.Rules[index].Order = index + 1
	}
	return plan
}

func compileLinuxPolicyRouting(egress Egress, capture NftablesCapturePlan) (LinuxPolicyRoutingPlan, error) {
	return compileLinuxPolicyRoutingForNetwork(egress, ServiceNetworkForEgressID(egress.ID), capture)
}

func compileLinuxPolicyRoutingForNetwork(egress Egress, network ProxyServiceNetwork, capture NftablesCapturePlan) (LinuxPolicyRoutingPlan, error) {
	const (
		markMask     = "0xffffffff"
		tableName    = "ly_route_proxy_egress"
		family       = "inet"
		underlayKind = "vpp.proxy-service.network"
		defaultRoute = "default"
	)

	markValue, err := parseHexUint32(capture.Mark)
	if err != nil {
		return LinuxPolicyRoutingPlan{}, fmt.Errorf("%w: linux policy routing mark %q is invalid", ErrInvalidEgress, capture.Mark)
	}

	plan := LinuxPolicyRoutingPlan{
		EgressID:     egress.ID,
		Mark:         capture.Mark,
		MarkValue:    markValue,
		MarkMask:     markMask,
		TableID:      network.OutboundTableID,
		TableName:    tableName,
		RulePriority: network.OutboundRulePriority,
		RuleSelector: LinuxRuleSelector{
			Family: family,
			Mark:   capture.Mark,
			Mask:   markMask,
			Table:  network.OutboundTableID,
		},
		DefaultRoute: LinuxDefaultRoute{
			Destination: defaultRoute,
			Table:       network.OutboundTableID,
			Via:         network.EgressVPPAddress,
			Device:      network.EgressHostInterface,
			Scope:       "link",
		},
		Underlay: LinuxUnderlayRoute{
			EgressID: egress.ID,
			Kind:     underlayKind,
			ID:       egress.ID,
		},
		Network: network,
	}
	if err := validateLinuxPolicyRoutingPlan(plan); err != nil {
		return LinuxPolicyRoutingPlan{}, err
	}

	return plan, nil
}

func validateLinuxPolicyRoutingPlan(plan LinuxPolicyRoutingPlan) error {
	if strings.TrimSpace(plan.EgressID) == "" || strings.TrimSpace(plan.Underlay.EgressID) == "" || plan.Underlay.EgressID != plan.EgressID || plan.Underlay.ID != plan.EgressID {
		return fmt.Errorf("%w: linux policy routing must be linked to proxy egress", ErrInvalidEgress)
	}
	if plan.Mark == "" || plan.MarkValue == 0 || plan.RuleSelector.Mark != plan.Mark {
		return fmt.Errorf("%w: linux policy routing requires a non-zero fwmark", ErrInvalidEgress)
	}
	if plan.MarkMask != "0xffffffff" || plan.RuleSelector.Mask != plan.MarkMask {
		return fmt.Errorf("%w: linux policy routing mark mask is unsupported", ErrInvalidEgress)
	}
	if plan.TableID <= 0 || plan.RulePriority <= 0 || plan.RuleSelector.Table != plan.TableID || plan.DefaultRoute.Table != plan.TableID {
		return fmt.Errorf("%w: linux policy routing table and rule priority must be positive and linked", ErrInvalidEgress)
	}
	if plan.TableID == 253 || plan.TableID == 254 || plan.TableID == 255 {
		return fmt.Errorf("%w: linux policy routing table %d is reserved", ErrInvalidEgress, plan.TableID)
	}
	if plan.RuleSelector.Family != "inet" || plan.DefaultRoute.Destination != "default" || strings.TrimSpace(plan.DefaultRoute.Device) == "" || (plan.Underlay.Kind != "vpp.service-chain.egress-binding" && plan.Underlay.Kind != "vpp.proxy-service.network") {
		return fmt.Errorf("%w: linux policy routing underlay/default route combination is unsupported", ErrInvalidEgress)
	}

	return nil
}

func parseHexUint32(value string) (uint32, error) {
	if !strings.HasPrefix(value, "0x") {
		return 0, fmt.Errorf("missing 0x prefix")
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 32)
	if err != nil {
		return 0, err
	}

	return uint32(parsed), nil
}

func compileXrayRuntime(egress Egress, network ProxyServiceNetwork, mode ListenerMode) XrayRuntime {
	const (
		processName      = "xray"
		listenAddress    = "0.0.0.0"
		healthAddress    = "127.0.0.1"
		transportNetwork = "tcp,udp"
		protocol         = "dokodemo-door"
	)

	inboundTag := egress.ID + "-vpp-service-inbound"
	configPath := "/etc/xray/config.json"
	listenerPort := network.ListenerPort
	listenerTarget := fmt.Sprintf("%s:%d", listenAddress, listenerPort)
	healthCheckTarget := fmt.Sprintf("%s:%d", healthAddress, listenerPort)
	config := XrayConfigPayload{
		Log: XrayLog{Level: "warning"},
		Inbounds: []XrayInbound{
			{
				Tag:            inboundTag,
				Listen:         listenAddress,
				Port:           listenerPort,
				Protocol:       protocol,
				Settings:       XrayDokodemoSettings{Network: transportNetwork, FollowRedirect: true},
				StreamSettings: map[string]any{"sockopt": map[string]any{"tproxy": "tproxy"}},
			},
		},
		Outbounds: []XrayOutbound{
			{Tag: egress.ID, Protocol: "freedom", StreamSettings: map[string]any{"sockopt": map[string]any{"mark": network.OutboundMark}}},
		},
	}

	return XrayRuntime{
		EgressID:          egress.ID,
		Engine:            egress.Engine,
		Mode:              mode,
		ProcessName:       processName,
		ConfigPath:        configPath,
		HealthCheckTarget: healthCheckTarget,
		ListenAddress:     listenAddress,
		ListenPort:        listenerPort,
		ListenerTarget:    listenerTarget,
		Network:           transportNetwork,
		Protocol:          protocol,
		OutboundTag:       egress.ID,
		ConfigPayload:     config,
		ServiceNetwork:    network,
	}
}
