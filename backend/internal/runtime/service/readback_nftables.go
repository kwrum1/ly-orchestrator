package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// errNftablesLiveDrift marks a live table that is missing a rule (or the table
// itself) the rendered generation owns. It is the only condition the managed
// table rebuild is allowed to repair; malformed rendered plans never reach it.
var errNftablesLiveDrift = errors.New("nftables live state differs from the managed generation")

// managedNftablesTablePrefix is the only namespace this product may delete.
// Host firewalls, container rules and operator tables must never be touched.
const managedNftablesTablePrefix = "ly_route_"

type managedNftablesTable struct {
	Family string
	Name   string
}

func validateNftablesReadback(ctx context.Context, runner CommandRunner, artifacts []RenderedArtifact) error {
	content, err := artifactContent(artifacts, "/etc/nftables.conf")
	if err != nil {
		return err
	}
	family, table, rules, err := expectedNftablesRules(content)
	if err != nil {
		return err
	}
	output, err := requiredOutput(ctx, runner, "nft", "list", "table", family, table)
	if err != nil {
		// A table that was removed by a service restart, a partial reload or a
		// VPP/egress rebuild is recoverable: the rendered artifact owns the
		// complete desired table. Only the drift class is repairable.
		return fmt.Errorf("%w: %v", errNftablesLiveDrift, err)
	}
	normalized := normalizedNftables(output)
	position := -1
	for _, rule := range rules {
		next := nftablesRulePosition(normalized, rule, position)
		if next < 0 {
			return fmt.Errorf("%w: nftables live table missing rule %q", errNftablesLiveDrift, rule)
		}
		if next <= position {
			return fmt.Errorf("%w: nftables live rule %q violates rendered rule order", errNftablesLiveDrift, rule)
		}
		position = next
	}
	return nil
}

// managedNftablesTables returns every table the rendered artifact declares,
// including the idempotent `add table`/`delete table` lines emitted by the
// no-proxy teardown generation.
func managedNftablesTables(content string) ([]managedNftablesTable, error) {
	tables := make([]managedNftablesTable, 0, 2)
	seen := make(map[string]bool, 2)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var family, table string
		switch {
		case fields[0] == "table":
			family, table = fields[1], fields[2]
		case len(fields) >= 4 && fields[0] == "add" && fields[1] == "table":
			family, table = fields[2], fields[3]
		default:
			continue
		}
		if family == "" || table == "" {
			continue
		}
		key := family + " " + table
		if seen[key] {
			continue
		}
		seen[key] = true
		tables = append(tables, managedNftablesTable{Family: family, Name: table})
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("rendered nftables plan lacks a table declaration")
	}
	return tables, nil
}

func managedNftablesTableOwned(table string) bool {
	return strings.HasPrefix(strings.TrimSpace(table), managedNftablesTablePrefix)
}

// rebuildManagedNftables deletes only tables declared by the rendered artifact
// whose names are in the product namespace, then reinstalls the complete
// expected generation from the persisted artifact. It never runs `flush
// ruleset` and never touches a table it does not own.
func rebuildManagedNftables(ctx context.Context, runner CommandRunner, artifacts []RenderedArtifact) error {
	artifact, err := nftablesArtifact(artifacts)
	if err != nil {
		return err
	}
	tables, err := managedNftablesTables(artifact.Content)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if !managedNftablesTableOwned(table.Name) {
			return fmt.Errorf("refusing to rebuild non-ly-route nftables table %s %s", table.Family, table.Name)
		}
	}
	for _, table := range tables {
		// A table that is already absent is the desired state. Kernel errors
		// are intentionally ignored here; the post-rebuild readback is the
		// authority and reports a precise missing rule if the reinstall fails.
		_ = runner.Run(ctx, "nft", "delete", "table", table.Family, table.Name)
	}
	if err := runner.Run(ctx, "nft", "-f", artifact.Path); err != nil {
		return fmt.Errorf("reinstall managed nftables generation %s: %w", artifact.Path, err)
	}
	return nil
}

func nftablesArtifact(artifacts []RenderedArtifact) (RenderedArtifact, error) {
	for _, artifact := range artifacts {
		if artifact.Service == Nftables && artifact.Path == "/etc/nftables.conf" {
			return artifact, nil
		}
	}
	return RenderedArtifact{}, fmt.Errorf("rendered nftables artifact is missing")
}

func nftablesRulePosition(live, rendered string, previous int) int {
	position := strings.Index(live, rendered)
	if position > previous {
		return position
	}
	for _, protocol := range []string{"tcp", "udp"} {
		for _, suffix := range []string{" meta mark", " counter meta mark"} {
			variant := strings.Replace(rendered, " meta l4proto "+protocol+suffix, suffix, 1)
			if variant == rendered {
				continue
			}
			position = strings.Index(live, variant)
			if position > previous {
				return position
			}
		}
	}
	return -1
}

func expectedNftablesRules(content string) (string, string, []string, error) {
	var family, table string
	rules := make([]string, 0, 6)
	guardRules := make([]string, 0, 2)
	dnsRules := 0
	tproxyRules := 0
	firstTProxy := -1
	lastDNS := -1
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "table" {
			family, table = fields[1], fields[2]
		}
		normalized := normalizedNftables(line)
		isGuard := strings.Contains(normalized, "dport 53") &&
			nftablesRuleHasToken(normalized, "iifname") &&
			nftablesRuleHasToken(normalized, "drop")
		if isGuard {
			// The DNS LCP guard drops the duplicate copy that the Linux
			// control-plane tap receives; the VPP DNS adapter owns the real
			// query. It is a valid artifact on its own when no proxy capture
			// is configured.
			guardRules = append(guardRules, normalized)
			continue
		}
		isDNS := strings.Contains(normalized, "dport 53") &&
			(nftablesRuleHasToken(normalized, "redirect") || nftablesRuleHasToken(normalized, "return"))
		isTProxy := nftablesRuleHasToken(normalized, "tproxy")
		if isDNS || isTProxy {
			rules = append(rules, normalized)
			position := len(rules) - 1
			if isDNS {
				dnsRules++
				lastDNS = position
			}
			if isTProxy {
				tproxyRules++
				if firstTProxy < 0 {
					firstTProxy = position
				}
			}
		}
	}
	if family == "" || table == "" {
		return "", "", nil, fmt.Errorf("rendered nftables plan lacks a table declaration")
	}
	// A degraded proxy runtime knowingly renders an empty, policy-accept
	// capture table. That is fail-open, not a truncated plan, and is only
	// accepted for the managed proxy table.
	failOpen := strings.Contains(content, "ly-route-proxy-fail-open")
	if failOpen && (family != "inet" || table != "ly_route_proxy_capture") {
		return "", "", nil, fmt.Errorf("nftables fail-open marker is only valid for inet ly_route_proxy_capture")
	}
	if dnsRules == 0 && tproxyRules == 0 && len(guardRules) == 0 && !failOpen {
		return "", "", nil, fmt.Errorf("rendered nftables plan lacks capture rules")
	}
	if dnsRules == 0 && tproxyRules == 0 && len(guardRules) > 0 {
		rules = append(rules, guardRules...)
	}
	if dnsRules > 0 && dnsRules < 2 {
		return "", "", nil, fmt.Errorf("rendered nftables plan requires TCP and UDP DNS rules")
	}
	if tproxyRules > 0 && tproxyRules < 2 {
		return "", "", nil, fmt.Errorf("rendered nftables plan requires TCP and UDP TProxy rules")
	}
	if dnsRules > 0 && tproxyRules > 0 && lastDNS >= firstTProxy {
		return "", "", nil, fmt.Errorf("rendered nftables plan violates DNS-before-TProxy semantics")
	}
	return family, table, rules, nil
}

func nftablesRuleHasToken(rule, token string) bool {
	for _, field := range strings.Fields(rule) {
		if field == token {
			return true
		}
	}
	return false
}

// canonicalNftablesHostPrefix mirrors how the kernel prints a host prefix.
// `nft add ... ip daddr 255.255.255.255/32 ...` is listed as
// `ip daddr 255.255.255.255` and `::1/128` as `::1`, so the readback has to
// compare the canonical form or every generation containing a /32 bypass
// fails with "live table missing rule".
func canonicalNftablesHostPrefix(field string) string {
	if !strings.Contains(field, "/") {
		return field
	}
	prefix, err := netip.ParsePrefix(field)
	if err != nil || prefix.Bits() != prefix.Addr().BitLen() {
		return field
	}
	return prefix.Addr().String()
}

func normalizedNftables(value string) string {
	fields := strings.Fields(value)
	transport := ""
	if strings.Contains(value, "jhash") {
		for _, protocol := range []string{"tcp", "udp"} {
			if strings.Contains(value, protocol+" sport") {
				transport = protocol
				break
			}
		}
	}
	normalized := make([]string, 0, len(fields))
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if field == "counter" && index+4 < len(fields) && fields[index+1] == "packets" && fields[index+3] == "bytes" {
			normalized = append(normalized, field)
			index += 4
			continue
		}
		if field == "==" {
			continue
		}
		if transport != "" && index+2 < len(fields) && field == "meta" && fields[index+1] == "l4proto" && fields[index+2] == transport {
			index += 2
			continue
		}
		field = canonicalNftablesHostPrefix(field)
		if !strings.HasPrefix(field, "0x") {
			normalized = append(normalized, field)
			continue
		}
		parsed, err := strconv.ParseUint(field, 0, 64)
		if err == nil {
			field = fmt.Sprintf("0x%x", parsed)
		}
		normalized = append(normalized, field)
	}
	return strings.Join(normalized, " ")
}
