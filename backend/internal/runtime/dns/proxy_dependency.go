package dns

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// ProxyDNSDependency names the hostnames a proxy egress must resolve before it
// can be established. The caller derives them from proxy node addresses and
// subscription URLs; the DNS compiler uses them to prove that the DNS policy
// does not depend on a proxy that itself depends on the DNS answer.
type ProxyDNSDependency struct {
	ProxyEgressID string   `json:"proxy_egress_id"`
	Domains       []string `json:"domains,omitempty"`
}

// ProxyDNSDependencyCycleError is returned when a proxy egress needs a
// hostname whose DNS answer is routed through a proxy egress that needs it
// (directly or through a chain).
type ProxyDNSDependencyCycleError struct {
	ProxyEgressIDs []string
	Domain         string
}

func (failure *ProxyDNSDependencyCycleError) Error() string {
	if failure == nil {
		return ""
	}
	path := strings.Join(failure.ProxyEgressIDs, " -> ")
	domain := strings.TrimSpace(failure.Domain)
	if domain == "" {
		domain = "a proxy node hostname"
	}
	return fmt.Sprintf("DNS proxy dependency cycle detected: proxy egress path %s cannot resolve %q because resolving it requires the proxy to be established first; configure a direct upstream (or bootstrap resolver) for the proxy node/subscription hostname, route it through a non-circular egress, or add a fixed answer. See docs/DNS-PROXY-SEMANTICS.md", path, domain)
}

type proxyDNSDependencyEdge struct {
	to     string
	domain string
}

// CheckProxyDNSDependencyCycles performs a static dependency check over a
// compiled DNS policy. It only follows proxy outcomes; direct, reject and
// fixed-answer rules terminate a branch. A missing dependency list is a no-op,
// so callers that do not have proxy metadata keep the previous behavior.
func CheckProxyDNSDependencyCycles(compiled CompiledPolicy, dependencies []ProxyDNSDependency) error {
	return CheckProxyDNSDependencyCyclesFrom(compiled, dependencies, nil)
}

// CheckProxyDNSDependencyCyclesFrom limits cycle discovery to paths that start
// at an active proxy egress. Dependencies for every egress are still used as
// graph edges so a chain such as a -> b -> a is detected even when b itself has
// no active DNS rule. An empty start list keeps the all-egress behavior.
func CheckProxyDNSDependencyCyclesFrom(compiled CompiledPolicy, dependencies []ProxyDNSDependency, startEgressIDs []string) error {
	if len(dependencies) == 0 {
		return nil
	}
	edges := make(map[string][]proxyDNSDependencyEdge)
	seenEdge := make(map[string]struct{})
	for _, dependency := range dependencies {
		egressID := strings.TrimSpace(dependency.ProxyEgressID)
		if egressID == "" {
			continue
		}
		for _, raw := range dependency.Domains {
			domain := normalizeProxyDNSDependencyDomain(raw)
			if domain == "" {
				continue
			}
			decision := Decide(compiled, domain, nil)
			if decision.Outcome.Kind != ResolverOutcomeProxyResolution {
				continue
			}
			target := strings.TrimSpace(decision.Outcome.ProxyEgressID)
			if target == "" {
				continue
			}
			key := egressID + "\x00" + target + "\x00" + domain
			if _, exists := seenEdge[key]; exists {
				continue
			}
			seenEdge[key] = struct{}{}
			edges[egressID] = append(edges[egressID], proxyDNSDependencyEdge{to: target, domain: domain})
		}
	}
	if len(edges) == 0 {
		return nil
	}

	nodes := make([]string, 0, len(edges))
	seenNode := make(map[string]struct{})
	addNode := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seenNode[id]; exists {
			return
		}
		seenNode[id] = struct{}{}
		nodes = append(nodes, id)
	}
	if len(startEgressIDs) > 0 {
		for _, id := range startEgressIDs {
			addNode(id)
		}
	} else {
		for _, dependency := range dependencies {
			addNode(dependency.ProxyEgressID)
		}
		for from, list := range edges {
			addNode(from)
			for _, edge := range list {
				addNode(edge.to)
			}
		}
	}
	sort.Strings(nodes)

	state := make(map[string]int, len(nodes)) // 0 unvisited, 1 visiting, 2 done
	stack := make([]string, 0, len(nodes))
	var visit func(string) error
	visit = func(node string) error {
		switch state[node] {
		case 1:
			start := 0
			for index, id := range stack {
				if id == node {
					start = index
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), node)
			domain := ""
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				for _, edge := range edges[parent] {
					if edge.to == node {
						domain = edge.domain
						break
					}
				}
			}
			return &ProxyDNSDependencyCycleError{ProxyEgressIDs: cycle, Domain: domain}
		case 2:
			return nil
		}
		state[node] = 1
		stack = append(stack, node)
		for _, edge := range edges[node] {
			if err := visit(edge.to); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
		return nil
	}
	for _, node := range nodes {
		if state[node] == 0 {
			if err := visit(node); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeProxyDNSDependencyDomain(value string) string {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if value == "" {
		return ""
	}
	if address, err := netip.ParseAddr(value); err == nil {
		// Literal addresses need no resolution.
		_ = address
		return ""
	}
	return value
}
