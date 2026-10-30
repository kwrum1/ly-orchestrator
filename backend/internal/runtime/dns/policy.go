package dns

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"ly-route/backend/internal/runtime/proxy"
)

type OutcomeKind string

const (
	OutcomeDirect OutcomeKind = "direct"
	OutcomeProxy  OutcomeKind = "proxy"
	OutcomeReject OutcomeKind = "reject"
	OutcomeFixed  OutcomeKind = "fixed_answer"
)

type ResolverOutcomeKind string

const (
	ResolverOutcomeDirect          ResolverOutcomeKind = "direct_resolution"
	ResolverOutcomeProxyResolution ResolverOutcomeKind = "proxy_resolution"
	ResolverOutcomeReject          ResolverOutcomeKind = "reject"
	ResolverOutcomeFixedAnswer     ResolverOutcomeKind = "fixed_answer"
)

// FailureMode makes the behavior of a policy when its selected resolver is
// unavailable explicit. The default is fail_closed: never leak a proxied
// answer to the direct path. fallback_direct is only legal with an explicitly
// named fallback upstream, so a fallback can never be silent.
type FailureMode string

const (
	FailureModeFailClosed     FailureMode = "fail_closed"
	FailureModeReject         FailureMode = "reject"
	FailureModeFallbackDirect FailureMode = "fallback_direct"
)

const DNSPolicyPrecedence = "dns_policy"

var ErrInvalidPolicy = errors.New("invalid dns policy")

type Outcome struct {
	Kind          OutcomeKind `json:"kind"`
	ProxyEgressID string      `json:"proxy_egress_id,omitempty"`
	UpstreamID    string      `json:"upstream_id,omitempty"`
	WANEgressID   string      `json:"wan_egress_id,omitempty"`
	FixedAnswers  []string    `json:"fixed_answers,omitempty"`
	// FailureMode applies to proxy outcomes. Empty means fail_closed.
	FailureMode FailureMode `json:"failure_mode,omitempty"`
	// FallbackUpstreamID is required only for FailureModeFallbackDirect. It
	// names the direct resolver used after an explicit operator decision.
	FallbackUpstreamID string `json:"fallback_upstream_id,omitempty"`
}

func Direct() Outcome {
	return Outcome{Kind: OutcomeDirect, UpstreamID: "dns-direct-default"}
}

func Proxy(proxyEgressID string) Outcome {
	return Outcome{Kind: OutcomeProxy, ProxyEgressID: proxyEgressID}
}

func Reject() Outcome {
	return Outcome{Kind: OutcomeReject}
}

func FixedAnswer(answers ...string) Outcome {
	return Outcome{Kind: OutcomeFixed, FixedAnswers: append([]string(nil), answers...)}
}

type Rule struct {
	ID             string   `json:"id"`
	SourcePrefixes []string `json:"source_prefixes,omitempty"`
	Domains        []string `json:"domains"`
	DomainSuffixes []string `json:"domain_suffixes,omitempty"`
	DomainSetIDs   []string `json:"domain_set_ids,omitempty"`
	Outcome        Outcome  `json:"outcome"`
}

type Policy struct {
	Name   string  `json:"name,omitempty"`
	Engine string  `json:"engine"`
	Miss   Outcome `json:"miss"`
	Rules  []Rule  `json:"rules"`
}

type CompiledPolicy struct {
	Engine                 string            `json:"engine"`
	DecisionPrecedence     string            `json:"decision_precedence"`
	Miss                   CompiledOutcome   `json:"miss"`
	Rules                  []CompiledRule    `json:"rules"`
	ProxyDNSBindings       []ProxyDNSBinding `json:"proxy_dns_bindings"`
	ReferencedEgressIDs    []string          `json:"referenced_proxy_egress_ids"`
	ReferencedDomainSetIDs []string          `json:"referenced_domain_set_ids,omitempty"`
	ReferencedWANEgressIDs []string          `json:"referenced_wan_egress_ids,omitempty"`
	ReferencedUpstreamIDs  []string          `json:"referenced_upstream_ids,omitempty"`
}

type CompiledRule struct {
	ID             string          `json:"id"`
	SourcePrefixes []string        `json:"source_prefixes,omitempty"`
	Domains        []string        `json:"domains"`
	DomainSuffixes []string        `json:"domain_suffixes,omitempty"`
	DomainSetIDs   []string        `json:"domain_set_ids,omitempty"`
	Outcome        CompiledOutcome `json:"outcome"`
}

type CompiledOutcome struct {
	Kind               ResolverOutcomeKind `json:"kind"`
	PolicyPrecedence   string              `json:"policy_precedence,omitempty"`
	ResolverPath       string              `json:"resolver_path"`
	ProxyEgressID      string              `json:"proxy_egress_id,omitempty"`
	UpstreamID         string              `json:"upstream_id,omitempty"`
	WANEgressID        string              `json:"wan_egress_id,omitempty"`
	DNSRequestPath     string              `json:"dns_request_path,omitempty"`
	FixedAnswers       []string            `json:"fixed_answers,omitempty"`
	FailureMode        FailureMode         `json:"failure_mode,omitempty"`
	FallbackUpstreamID string              `json:"fallback_upstream_id,omitempty"`
}

type ProxyDNSBinding struct {
	RuleID             string              `json:"rule_id"`
	ProxyEgressID      string              `json:"proxy_egress_id"`
	UpstreamID         string              `json:"upstream_id,omitempty"`
	OutcomeKind        ResolverOutcomeKind `json:"outcome_kind"`
	PolicyPrecedence   string              `json:"policy_precedence"`
	ResolverPath       string              `json:"resolver_path"`
	DNSRequestPath     string              `json:"dns_request_path"`
	FailureMode        FailureMode         `json:"failure_mode,omitempty"`
	FallbackUpstreamID string              `json:"fallback_upstream_id,omitempty"`
}

type Decision struct {
	Domain        string          `json:"domain"`
	SourceIP      string          `json:"source_ip,omitempty"`
	Matched       bool            `json:"matched"`
	RuleID        string          `json:"rule_id,omitempty"`
	Outcome       CompiledOutcome `json:"outcome"`
	RCode         string          `json:"rcode"`
	Answer        string          `json:"answer"`
	Answers       []string        `json:"answers,omitempty"`
	ContinueRules bool            `json:"continue_rules"`
	Reason        string          `json:"reason,omitempty"`
}

type SmartDNSRender struct {
	Engine             string         `json:"engine"`
	DecisionPrecedence string         `json:"decision_precedence"`
	Rules              []SmartDNSRule `json:"rules"`
	Miss               SmartDNSRule   `json:"miss"`
}

type SmartDNSRule struct {
	Order              int                 `json:"order"`
	Target             string              `json:"target"`
	RuleID             string              `json:"rule_id"`
	SourcePrefixes     []string            `json:"source_prefixes,omitempty"`
	Domains            []string            `json:"domains,omitempty"`
	DomainSuffixes     []string            `json:"domain_suffixes,omitempty"`
	DomainSetIDs       []string            `json:"domain_set_ids,omitempty"`
	Action             string              `json:"action"`
	OutcomeKind        ResolverOutcomeKind `json:"outcome_kind"`
	PolicyPrecedence   string              `json:"policy_precedence"`
	ResolverPath       string              `json:"resolver_path"`
	ProxyEgressID      string              `json:"proxy_egress_id,omitempty"`
	UpstreamID         string              `json:"upstream_id,omitempty"`
	WANEgressID        string              `json:"wan_egress_id,omitempty"`
	DNSRequestPath     string              `json:"dns_request_path,omitempty"`
	FixedAnswers       []string            `json:"fixed_answers,omitempty"`
	IPSetName          string              `json:"ipset_name,omitempty"`
	FailureMode        FailureMode         `json:"failure_mode,omitempty"`
	FallbackUpstreamID string              `json:"fallback_upstream_id,omitempty"`
}

func NewPolicy(miss Outcome, rules []Rule) Policy {
	miss = defaultMissOutcome(miss)
	return Policy{Engine: "smartdns", Miss: miss, Rules: append([]Rule(nil), rules...)}
}

// CompileOptions carries the runtime resources a policy may reference. The
// zero value preserves the historical behavior of CompilePolicy: proxy
// egresses are validated, while upstream references are only checked when the
// caller explicitly enables validation.
type CompileOptions struct {
	ProxyEgresses              []proxy.Egress
	UpstreamIDs                []string
	ValidateUpstreamReferences bool
}

func CompilePolicy(policy Policy, proxyEgresses []proxy.Egress) (CompiledPolicy, error) {
	return CompilePolicyWithOptions(policy, CompileOptions{ProxyEgresses: proxyEgresses})
}

func CompilePolicyWithOptions(policy Policy, options CompileOptions) (CompiledPolicy, error) {
	proxyIDs, err := validatedReferencedProxyEgressIDs(policy, options.ProxyEgresses)
	if err != nil {
		return CompiledPolicy{}, err
	}
	upstreamIDs := make(map[string]struct{}, len(options.UpstreamIDs))
	for _, id := range options.UpstreamIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			upstreamIDs[id] = struct{}{}
		}
	}

	if strings.TrimSpace(policy.Engine) == "" {
		policy.Engine = "smartdns"
	}
	if policy.Engine != "smartdns" {
		return CompiledPolicy{}, fmt.Errorf("%w: engine must be smartdns", ErrInvalidPolicy)
	}

	miss, err := compileOutcome("miss", defaultMissOutcome(policy.Miss), proxyIDs, upstreamIDs, options.ValidateUpstreamReferences)
	if err != nil {
		return CompiledPolicy{}, err
	}

	compiledRules := make([]CompiledRule, 0, len(policy.Rules))
	bindings := make([]ProxyDNSBinding, 0, len(policy.Rules))
	referenced := make([]string, 0, len(policy.Rules))
	seenReferenced := make(map[string]struct{})
	referencedDomainSets := make([]string, 0, len(policy.Rules))
	seenDomainSets := make(map[string]struct{})
	referencedWANs := make([]string, 0, len(policy.Rules))
	seenWANs := make(map[string]struct{})
	referencedUpstreams := make([]string, 0, len(policy.Rules))
	seenUpstreams := make(map[string]struct{})
	seenRules := make(map[string]struct{})
	for _, rule := range policy.Rules {
		ruleID := strings.TrimSpace(rule.ID)
		if ruleID == "" {
			return CompiledPolicy{}, fmt.Errorf("%w: rule id is required", ErrInvalidPolicy)
		}
		if !validDNSRuleID(ruleID) {
			return CompiledPolicy{}, fmt.Errorf("%w: rule id %q must match [A-Za-z0-9_.:-]{1,64}", ErrInvalidPolicy, ruleID)
		}
		if _, exists := seenRules[ruleID]; exists {
			return CompiledPolicy{}, fmt.Errorf("%w: duplicate rule id %q", ErrInvalidPolicy, ruleID)
		}
		seenRules[ruleID] = struct{}{}
		sourcePrefixes, err := validatedSourcePrefixes("rule "+ruleID, rule.SourcePrefixes)
		if err != nil {
			return CompiledPolicy{}, err
		}
		domains, suffixes, err := validatedDomainSelectors("rule "+ruleID, rule.Domains, rule.DomainSuffixes)
		if err != nil {
			return CompiledPolicy{}, err
		}
		domainSetIDs := normalizedIDs(rule.DomainSetIDs)
		for _, id := range domainSetIDs {
			addUnique(&referencedDomainSets, seenDomainSets, id)
		}

		compiledOutcome, err := compileOutcome("rule "+ruleID, rule.Outcome, proxyIDs, upstreamIDs, options.ValidateUpstreamReferences)
		if err != nil {
			return CompiledPolicy{}, err
		}
		compiledRules = append(compiledRules, CompiledRule{
			ID:             ruleID,
			SourcePrefixes: sourcePrefixes,
			Domains:        domains,
			DomainSuffixes: suffixes,
			DomainSetIDs:   domainSetIDs,
			Outcome:        compiledOutcome,
		})
		if compiledOutcome.Kind == ResolverOutcomeProxyResolution {
			bindings = append(bindings, ProxyDNSBinding{
				RuleID:             ruleID,
				ProxyEgressID:      compiledOutcome.ProxyEgressID,
				UpstreamID:         compiledOutcome.UpstreamID,
				OutcomeKind:        ResolverOutcomeProxyResolution,
				PolicyPrecedence:   compiledOutcome.PolicyPrecedence,
				ResolverPath:       compiledOutcome.ResolverPath,
				DNSRequestPath:     compiledOutcome.DNSRequestPath,
				FailureMode:        compiledOutcome.FailureMode,
				FallbackUpstreamID: compiledOutcome.FallbackUpstreamID,
			})
			addUnique(&referenced, seenReferenced, compiledOutcome.ProxyEgressID)
		}
		if compiledOutcome.WANEgressID != "" {
			addUnique(&referencedWANs, seenWANs, compiledOutcome.WANEgressID)
		}
		if compiledOutcome.UpstreamID != "" {
			addUnique(&referencedUpstreams, seenUpstreams, compiledOutcome.UpstreamID)
		}
		if compiledOutcome.FallbackUpstreamID != "" {
			addUnique(&referencedUpstreams, seenUpstreams, compiledOutcome.FallbackUpstreamID)
		}
	}

	return CompiledPolicy{
		Engine:                 policy.Engine,
		DecisionPrecedence:     DNSPolicyPrecedence,
		Miss:                   miss,
		Rules:                  compiledRules,
		ProxyDNSBindings:       bindings,
		ReferencedEgressIDs:    referenced,
		ReferencedDomainSetIDs: referencedDomainSets,
		ReferencedWANEgressIDs: referencedWANs,
		ReferencedUpstreamIDs:  referencedUpstreams,
	}, nil
}

func Decide(compiled CompiledPolicy, domain string, unavailableResolverPaths map[string]bool) Decision {
	return DecideForSource(compiled, domain, "", unavailableResolverPaths)
}

func DecideForSource(compiled CompiledPolicy, domain, sourceIP string, unavailableResolverPaths map[string]bool) Decision {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	sourceIP = strings.TrimSpace(sourceIP)
	for _, rule := range compiled.Rules {
		if !ruleMatchesSource(rule.SourcePrefixes, sourceIP) || !ruleMatchesDomain(rule.Domains, rule.DomainSuffixes, domain) {
			continue
		}
		return decisionForOutcome(domain, sourceIP, true, rule.ID, rule.Outcome, unavailableResolverPaths)
	}
	return decisionForOutcome(domain, sourceIP, false, "", compiled.Miss, unavailableResolverPaths)
}

func decisionForOutcome(domain, sourceIP string, matched bool, ruleID string, outcome CompiledOutcome, unavailableResolverPaths map[string]bool) Decision {
	decision := Decision{Domain: domain, SourceIP: sourceIP, Matched: matched, RuleID: ruleID, Outcome: outcome, RCode: "NOERROR", Answer: "NODATA", ContinueRules: false}
	if unavailableResolverPaths != nil {
		// Direct resolvers already have a per-resource key. Proxy used to share
		// the constant "proxy_egress_resolver", so one broken egress marked all
		// proxy rules unavailable. Use a per-egress (and, when present,
		// per-upstream) key; keep the legacy constant as an explicit wildcard
		// for callers that genuinely want to disable every proxy resolver.
		key := resolverAvailabilityKey(outcome)
		if unavailableResolverPaths[key] || (outcome.Kind == ResolverOutcomeProxyResolution && unavailableResolverPaths[outcome.ResolverPath]) {
			decision.Reason = "selected resolver is unavailable"
			return decision
		}
	}
	switch outcome.Kind {
	case ResolverOutcomeDirect, ResolverOutcomeProxyResolution:
		decision.Answer = "RESOLVE"
	case ResolverOutcomeFixedAnswer:
		decision.Answer = "FIXED"
		decision.Answers = append([]string(nil), outcome.FixedAnswers...)
	default:
		decision.Answer = "NODATA"
	}
	return decision
}

func resolverAvailabilityKey(outcome CompiledOutcome) string {
	if outcome.Kind != ResolverOutcomeProxyResolution {
		return outcome.ResolverPath
	}
	egressID := strings.TrimSpace(outcome.ProxyEgressID)
	if egressID == "" {
		return outcome.ResolverPath
	}
	key := "proxy:" + egressID
	if upstreamID := strings.TrimSpace(outcome.UpstreamID); upstreamID != "" {
		key += ":" + upstreamID
	}
	return key
}

func ruleMatchesSource(prefixes []string, sourceIP string) bool {
	if len(prefixes) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return false
	}
	for _, value := range prefixes {
		prefix, err := parseSourcePrefix(value)
		if err == nil && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func ruleMatchesDomain(exactDomains, suffixes []string, domain string) bool {
	for _, pattern := range exactDomains {
		pattern = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
		pattern = strings.TrimPrefix(pattern, "*.")
		pattern = strings.TrimPrefix(pattern, ".")
		if pattern != "" && domain == pattern {
			return true
		}
	}
	for _, pattern := range suffixes {
		pattern = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
		pattern = strings.TrimPrefix(pattern, "*.")
		pattern = strings.TrimPrefix(pattern, ".")
		if pattern != "" && (domain == pattern || strings.HasSuffix(domain, "."+pattern)) {
			return true
		}
	}
	return false
}

func (policy CompiledPolicy) RenderSmartDNS() SmartDNSRender {
	rules := make([]SmartDNSRule, 0, len(policy.Rules))
	for index, rule := range policy.Rules {
		rules = append(rules, renderSmartDNSRule(index+1, rule.ID, rule.SourcePrefixes, rule.Domains, rule.DomainSuffixes, rule.DomainSetIDs, rule.Outcome))
	}

	return SmartDNSRender{
		Engine:             policy.Engine,
		DecisionPrecedence: DNSPolicyPrecedence,
		Rules:              rules,
		Miss:               renderSmartDNSRule(0, "miss", nil, nil, nil, nil, policy.Miss),
	}
}

func defaultMissOutcome(outcome Outcome) Outcome {
	if outcome.Kind == "" {
		outcome.Kind = OutcomeReject
	}
	return outcome
}

func renderSmartDNSRule(order int, ruleID string, sourcePrefixes, domains, suffixes, domainSetIDs []string, outcome CompiledOutcome) SmartDNSRule {
	rule := SmartDNSRule{
		Order:              order,
		RuleID:             ruleID,
		SourcePrefixes:     append([]string(nil), sourcePrefixes...),
		Domains:            append([]string(nil), domains...),
		DomainSuffixes:     append([]string(nil), suffixes...),
		DomainSetIDs:       append([]string(nil), domainSetIDs...),
		OutcomeKind:        outcome.Kind,
		PolicyPrecedence:   outcome.PolicyPrecedence,
		ResolverPath:       outcome.ResolverPath,
		UpstreamID:         outcome.UpstreamID,
		WANEgressID:        outcome.WANEgressID,
		FixedAnswers:       append([]string(nil), outcome.FixedAnswers...),
		FailureMode:        outcome.FailureMode,
		FallbackUpstreamID: outcome.FallbackUpstreamID,
	}

	switch outcome.Kind {
	case ResolverOutcomeDirect:
		rule.Target = "smartdns.nameserver-policy"
		rule.Action = "direct_resolution"
		if outcome.WANEgressID != "" {
			rule.IPSetName = SmartDNSIPSetName(ruleID)
		}
	case ResolverOutcomeProxyResolution:
		rule.Target = "smartdns.nameserver-policy"
		rule.Action = "proxy_dns_request"
		rule.ProxyEgressID = outcome.ProxyEgressID
		rule.DNSRequestPath = outcome.DNSRequestPath
	case ResolverOutcomeReject:
		rule.Target = "smartdns.address-rule"
		rule.Action = "empty_answer"
	case ResolverOutcomeFixedAnswer:
		rule.Target = "smartdns.address-rule"
		rule.Action = "fixed_answer"
	}

	return rule
}

// SmartDNSIPSetName returns a Linux ipset-compatible name for the
// DNS-to-dataplane handoff. Linux limits ipset names to 31 characters, so the
// stable digest keeps long user-authored rule IDs distinct without leaking a
// platform limit into the UI.
func SmartDNSIPSetName(ruleID string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(ruleID), ".", "_")
	const prefix = "lyroute_dns_"
	const maxLength = 31
	if len(prefix)+len(normalized) <= maxLength {
		return prefix + normalized
	}
	digest := sha256.Sum256([]byte(normalized))
	const suffixLength = 8
	keep := maxLength - len(prefix) - 1 - suffixLength
	return prefix + normalized[:keep] + "_" + fmt.Sprintf("%x", digest[:4])
}

// requiredProxyEgressIDs returns only the egresses a policy actually
// references. Unrelated broken resources must not block this policy: their
// own resource endpoint owns that error.
func requiredProxyEgressIDs(policy Policy) map[string]struct{} {
	ids := make(map[string]struct{})
	add := func(outcome Outcome) {
		if outcome.Kind != OutcomeProxy {
			return
		}
		if id := strings.TrimSpace(outcome.ProxyEgressID); id != "" {
			ids[id] = struct{}{}
		}
	}
	add(policy.Miss)
	for _, rule := range policy.Rules {
		add(rule.Outcome)
	}
	return ids
}

func validatedReferencedProxyEgressIDs(policy Policy, proxyEgresses []proxy.Egress) (map[string]struct{}, error) {
	required := requiredProxyEgressIDs(policy)
	ids := make(map[string]struct{}, len(required))
	if len(required) == 0 {
		return ids, nil
	}
	for _, egress := range proxyEgresses {
		id := strings.TrimSpace(egress.ID)
		if _, needed := required[id]; !needed {
			continue
		}
		if err := proxy.ValidateEgress(egress); err != nil {
			return nil, fmt.Errorf("%w: proxy egress %q failed validation: %v", ErrInvalidPolicy, egress.ID, err)
		}
		if _, exists := ids[id]; exists {
			return nil, fmt.Errorf("%w: duplicate proxy egress id %q", ErrInvalidPolicy, id)
		}
		ids[id] = struct{}{}
	}
	// A required ID that is absent from the provided list is reported by
	// compileOutcome at the exact rule/miss label, which is more actionable
	// than a generic list error here.
	return ids, nil
}

func validatedSourcePrefixes(label string, values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		// The UI uses an explicit Any selector for readability. In the
		// canonical policy, an empty source list means any source; accepting the
		// alias here keeps persisted/UI-authored policies valid while retaining
		// one normalized representation for SmartDNS and decision evaluation.
		if strings.EqualFold(value, "any") || value == "*" {
			continue
		}
		prefix, err := parseSourcePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %s source selector %q is not a valid IP or prefix", ErrInvalidPolicy, label, value)
		}
		text := prefix.String()
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	return result, nil
}

func parseSourcePrefix(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func validatedDomainSelectors(label string, domains, suffixes []string) ([]string, []string, error) {
	exact := make([]string, 0, len(domains))
	seenExact := map[string]struct{}{}
	for _, domain := range domains {
		domain, err := normalizedDomainSelector(label, domain)
		if err != nil {
			return nil, nil, err
		}
		addUnique(&exact, seenExact, domain)
	}
	normalizedSuffixes := make([]string, 0, len(suffixes))
	seenSuffixes := map[string]struct{}{}
	for _, suffix := range suffixes {
		suffix, err := normalizedDomainSelector(label, suffix)
		if err != nil {
			return nil, nil, err
		}
		addUnique(&normalizedSuffixes, seenSuffixes, suffix)
	}
	return exact, normalizedSuffixes, nil
}

func normalizedDomainSelector(label, value string) (string, error) {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	value = strings.TrimPrefix(value, "*.")
	value = strings.TrimPrefix(value, ".")
	if value == "" {
		return "", fmt.Errorf("%w: %s domain selector is empty", ErrInvalidPolicy, label)
	}
	if value == "*" || value == "any" {
		return "", fmt.Errorf("%w: %s must not use a default any DNS rule", ErrInvalidPolicy, label)
	}
	if _, err := netip.ParseAddr(value); err == nil || strings.Contains(value, "/") {
		return "", fmt.Errorf("%w: %s domain selector %q must not be an IP rule", ErrInvalidPolicy, label, value)
	}
	if !validDNSDomainName(value) {
		return "", fmt.Errorf("%w: %s domain selector %q contains characters or labels that are invalid for a DNS name", ErrInvalidPolicy, label, value)
	}
	return value, nil
}

// validDNSRuleID is the allow-list for rule IDs that appear in generated
// SmartDNS comments and in group names. It intentionally excludes whitespace,
// quotes and control characters so a persisted ID cannot inject a config line.
func validDNSRuleID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' ||
			character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

// validDNSUpstreamID matches the token set accepted by SmartDNS group names.
// It rejects whitespace and control characters before an ID reaches a
// nameserver directive.
func validDNSUpstreamID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validDNSDomainName(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, character := range label {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

func normalizedIDs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		addUnique(&result, seen, strings.TrimSpace(value))
	}
	return result
}

func addUnique(values *[]string, seen map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if _, exists := seen[value]; exists {
		return
	}
	seen[value] = struct{}{}
	*values = append(*values, value)
}

func compileOutcome(label string, outcome Outcome, proxyIDs, upstreamIDs map[string]struct{}, validateUpstreams bool) (CompiledOutcome, error) {
	if upstreamID := strings.TrimSpace(outcome.UpstreamID); upstreamID != "" && !validDNSUpstreamID(upstreamID) {
		return CompiledOutcome{}, fmt.Errorf("%w: %s upstream id %q must match [A-Za-z0-9_.-]{1,64}", ErrInvalidPolicy, label, upstreamID)
	}
	if fallback := strings.TrimSpace(outcome.FallbackUpstreamID); fallback != "" && !validDNSUpstreamID(fallback) {
		return CompiledOutcome{}, fmt.Errorf("%w: %s fallback upstream id %q must match [A-Za-z0-9_.-]{1,64}", ErrInvalidPolicy, label, fallback)
	}
	if wanEgressID := strings.TrimSpace(outcome.WANEgressID); wanEgressID != "" && !validDNSRuleID(wanEgressID) {
		return CompiledOutcome{}, fmt.Errorf("%w: %s WAN egress id %q must match [A-Za-z0-9_.:-]{1,64}", ErrInvalidPolicy, label, wanEgressID)
	}
	if proxyEgressID := strings.TrimSpace(outcome.ProxyEgressID); proxyEgressID != "" && !validDNSRuleID(proxyEgressID) {
		return CompiledOutcome{}, fmt.Errorf("%w: %s proxy egress id %q must match [A-Za-z0-9_.:-]{1,64}", ErrInvalidPolicy, label, proxyEgressID)
	}
	switch outcome.Kind {
	case OutcomeDirect:
		if strings.TrimSpace(outcome.ProxyEgressID) != "" {
			return CompiledOutcome{}, fmt.Errorf("%w: %s direct outcome must not reference proxy egress", ErrInvalidPolicy, label)
		}
		if len(outcome.FixedAnswers) != 0 {
			return CompiledOutcome{}, fmt.Errorf("%w: %s direct outcome must not include fixed answers", ErrInvalidPolicy, label)
		}
		if err := rejectNonProxyFailurePolicy(label, outcome); err != nil {
			return CompiledOutcome{}, err
		}
		upstreamID := strings.TrimSpace(outcome.UpstreamID)
		wanEgressID := strings.TrimSpace(outcome.WANEgressID)
		if upstreamID == "" && wanEgressID == "" {
			return CompiledOutcome{}, fmt.Errorf("%w: %s direct outcome must select an upstream or WAN egress", ErrInvalidPolicy, label)
		}
		resolverPath := ""
		if upstreamID != "" {
			resolverPath = "upstream:" + upstreamID
		} else if wanEgressID != "" {
			resolverPath = "wan:" + wanEgressID
		}
		return CompiledOutcome{Kind: ResolverOutcomeDirect, PolicyPrecedence: DNSPolicyPrecedence, ResolverPath: resolverPath, UpstreamID: upstreamID, WANEgressID: wanEgressID}, nil
	case OutcomeProxy:
		proxyEgressID := strings.TrimSpace(outcome.ProxyEgressID)
		if proxyEgressID == "" {
			return CompiledOutcome{}, fmt.Errorf("%w: %s proxy outcome requires proxy egress id", ErrInvalidPolicy, label)
		}
		if strings.TrimSpace(outcome.WANEgressID) != "" || len(outcome.FixedAnswers) != 0 {
			return CompiledOutcome{}, fmt.Errorf("%w: %s proxy outcome must not include WAN or fixed-answer fields", ErrInvalidPolicy, label)
		}
		if _, exists := proxyIDs[proxyEgressID]; !exists {
			return CompiledOutcome{}, fmt.Errorf("%w: %s references missing proxy egress %q", ErrInvalidPolicy, label, proxyEgressID)
		}
		// The upstream is the resolver reached *through* the proxy egress, not
		// a direct resolver. It is optional so a proxy egress can use its own
		// default resolver, but an explicit reference must exist.
		upstreamID := strings.TrimSpace(outcome.UpstreamID)
		if upstreamID != "" && validateUpstreams {
			if _, exists := upstreamIDs[upstreamID]; !exists {
				return CompiledOutcome{}, fmt.Errorf("%w: %s proxy outcome references missing DNS upstream %q", ErrInvalidPolicy, label, upstreamID)
			}
		}
		failureMode, fallbackUpstreamID, err := compileFailurePolicy(label, outcome, upstreamIDs, validateUpstreams)
		if err != nil {
			return CompiledOutcome{}, err
		}
		return CompiledOutcome{
			Kind:               ResolverOutcomeProxyResolution,
			PolicyPrecedence:   DNSPolicyPrecedence,
			ResolverPath:       "proxy_egress_resolver",
			ProxyEgressID:      proxyEgressID,
			UpstreamID:         upstreamID,
			DNSRequestPath:     "proxy_egress_dns",
			FailureMode:        failureMode,
			FallbackUpstreamID: fallbackUpstreamID,
		}, nil
	case OutcomeReject:
		if strings.TrimSpace(outcome.ProxyEgressID) != "" || strings.TrimSpace(outcome.UpstreamID) != "" || strings.TrimSpace(outcome.WANEgressID) != "" || len(outcome.FixedAnswers) != 0 {
			return CompiledOutcome{}, fmt.Errorf("%w: %s reject outcome must not include egress or fixed-answer fields", ErrInvalidPolicy, label)
		}
		if err := rejectNonProxyFailurePolicy(label, outcome); err != nil {
			return CompiledOutcome{}, err
		}
		return CompiledOutcome{Kind: ResolverOutcomeReject, PolicyPrecedence: DNSPolicyPrecedence, ResolverPath: "reject_response"}, nil
	case OutcomeFixed:
		if strings.TrimSpace(outcome.ProxyEgressID) != "" || strings.TrimSpace(outcome.UpstreamID) != "" || strings.TrimSpace(outcome.WANEgressID) != "" {
			return CompiledOutcome{}, fmt.Errorf("%w: %s fixed-answer outcome must not reference egress fields", ErrInvalidPolicy, label)
		}
		if err := rejectNonProxyFailurePolicy(label, outcome); err != nil {
			return CompiledOutcome{}, err
		}
		answers := normalizedIDs(outcome.FixedAnswers)
		if len(answers) == 0 {
			return CompiledOutcome{}, fmt.Errorf("%w: %s fixed-answer outcome requires at least one answer", ErrInvalidPolicy, label)
		}
		return CompiledOutcome{Kind: ResolverOutcomeFixedAnswer, PolicyPrecedence: DNSPolicyPrecedence, ResolverPath: "fixed_answer", FixedAnswers: answers}, nil
	default:
		return CompiledOutcome{}, fmt.Errorf("%w: %s outcome kind %q is unsupported", ErrInvalidPolicy, label, outcome.Kind)
	}
}

// compileFailurePolicy normalizes the proxy outcome's failure behavior. An
// empty mode defaults to fail_closed, and a fallback is never implicit: the
// caller must name the direct upstream that will receive the query.
func compileFailurePolicy(label string, outcome Outcome, upstreamIDs map[string]struct{}, validateUpstreams bool) (FailureMode, string, error) {
	mode := FailureMode(strings.ToLower(strings.TrimSpace(string(outcome.FailureMode))))
	if mode == "" {
		mode = FailureModeFailClosed
	}
	fallback := strings.TrimSpace(outcome.FallbackUpstreamID)
	switch mode {
	case FailureModeFailClosed, FailureModeReject:
		if fallback != "" {
			return "", "", fmt.Errorf("%w: %s failure_mode %q must not name fallback_upstream_id", ErrInvalidPolicy, label, mode)
		}
	case FailureModeFallbackDirect:
		if fallback == "" {
			return "", "", fmt.Errorf("%w: %s failure_mode %q requires an explicit fallback_upstream_id", ErrInvalidPolicy, label, mode)
		}
		if validateUpstreams {
			if _, exists := upstreamIDs[fallback]; !exists {
				return "", "", fmt.Errorf("%w: %s fallback_upstream_id %q is not a configured DNS upstream", ErrInvalidPolicy, label, fallback)
			}
		}
	default:
		return "", "", fmt.Errorf("%w: %s failure_mode %q is unsupported", ErrInvalidPolicy, label, outcome.FailureMode)
	}
	return mode, fallback, nil
}

func rejectNonProxyFailurePolicy(label string, outcome Outcome) error {
	if strings.TrimSpace(string(outcome.FailureMode)) != "" || strings.TrimSpace(outcome.FallbackUpstreamID) != "" {
		return fmt.Errorf("%w: %s failure_mode and fallback_upstream_id are valid only for proxy outcomes", ErrInvalidPolicy, label)
	}
	return nil
}
