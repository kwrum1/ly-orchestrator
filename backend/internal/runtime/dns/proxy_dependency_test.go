package dns

import (
	"errors"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
)

func proxyDependencyTestCompiled(t *testing.T, rules []Rule) CompiledPolicy {
	t.Helper()
	compiled, err := CompilePolicy(NewPolicy(Reject(), rules), []proxy.Egress{
		proxy.NewProxyEgress("proxy-a", "xray-tproxy-outbound"),
		proxy.NewProxyEgress("proxy-b", "xray-tproxy-outbound"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestCheckProxyDNSDependencyCyclesDetectsSelfDependency(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{{
		ID:      "proxy-node",
		Domains: []string{"node.proxy-a.example"},
		Outcome: Proxy("proxy-a"),
	}})
	err := CheckProxyDNSDependencyCycles(compiled, []ProxyDNSDependency{{
		ProxyEgressID: "proxy-a",
		Domains:       []string{"node.proxy-a.example"},
	}})
	var cycle *ProxyDNSDependencyCycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("error = %v, want a dependency cycle", err)
	}
	if len(cycle.ProxyEgressIDs) != 2 || cycle.ProxyEgressIDs[0] != "proxy-a" || cycle.ProxyEgressIDs[1] != "proxy-a" {
		t.Fatalf("cycle path = %#v, want proxy-a -> proxy-a", cycle.ProxyEgressIDs)
	}
	if cycle.Domain != "node.proxy-a.example" {
		t.Fatalf("cycle domain = %q", cycle.Domain)
	}
	if !strings.Contains(err.Error(), "docs/DNS-PROXY-SEMANTICS.md") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestCheckProxyDNSDependencyCyclesIgnoresDirectAndFixedDependencies(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{
		{ID: "direct-node", Domains: []string{"node.proxy-a.example"}, Outcome: Direct()},
		{ID: "fixed-node", Domains: []string{"fixed.proxy-a.example"}, Outcome: FixedAnswer("203.0.113.10")},
	})
	if err := CheckProxyDNSDependencyCycles(compiled, []ProxyDNSDependency{{
		ProxyEgressID: "proxy-a",
		Domains:       []string{"node.proxy-a.example", "fixed.proxy-a.example", "203.0.113.20"},
	}}); err != nil {
		t.Fatalf("non-proxy dependencies reported a cycle: %v", err)
	}
}

func TestCheckProxyDNSDependencyCyclesDetectsCrossEgressCycle(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{
		{ID: "proxy-b-node", Domains: []string{"node.proxy-b.example"}, Outcome: Proxy("proxy-a")},
		{ID: "proxy-a-node", Domains: []string{"node.proxy-a.example"}, Outcome: Proxy("proxy-b")},
	})
	err := CheckProxyDNSDependencyCycles(compiled, []ProxyDNSDependency{
		{ProxyEgressID: "proxy-a", Domains: []string{"node.proxy-a.example"}},
		{ProxyEgressID: "proxy-b", Domains: []string{"node.proxy-b.example"}},
	})
	var cycle *ProxyDNSDependencyCycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("error = %v, want a cross-egress cycle", err)
	}
	if len(cycle.ProxyEgressIDs) != 3 || cycle.ProxyEgressIDs[0] != cycle.ProxyEgressIDs[2] {
		t.Fatalf("cycle path = %#v, want a -> b -> a", cycle.ProxyEgressIDs)
	}
}

func TestCheckProxyDNSDependencyCyclesAllowsOneDirectionalDependency(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{
		{ID: "proxy-b-node", Domains: []string{"node.proxy-b.example"}, Outcome: Proxy("proxy-a")},
		{ID: "proxy-a-node", Domains: []string{"node.proxy-a.example"}, Outcome: Direct()},
	})
	if err := CheckProxyDNSDependencyCycles(compiled, []ProxyDNSDependency{
		{ProxyEgressID: "proxy-a", Domains: []string{"node.proxy-a.example"}},
		{ProxyEgressID: "proxy-b", Domains: []string{"node.proxy-b.example"}},
	}); err != nil {
		t.Fatalf("one-directional dependency reported a cycle: %v", err)
	}
}

func TestCheckProxyDNSDependencyCyclesHandlesMissingMetadata(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{{
		ID:      "proxy-node",
		Domains: []string{"node.proxy-a.example"},
		Outcome: Proxy("proxy-a"),
	}})
	if err := CheckProxyDNSDependencyCycles(compiled, nil); err != nil {
		t.Fatalf("empty dependency metadata should be a no-op: %v", err)
	}
}

func TestCheckProxyDNSDependencyCyclesFromIgnoresUnreferencedEgressCycles(t *testing.T) {
	compiled := proxyDependencyTestCompiled(t, []Rule{
		{ID: "proxy-b-node", Domains: []string{"node.proxy-b.example"}, Outcome: Proxy("proxy-a")},
		{ID: "proxy-a-node", Domains: []string{"node.proxy-a.example"}, Outcome: Proxy("proxy-b")},
	})
	dependencies := []ProxyDNSDependency{
		{ProxyEgressID: "proxy-a", Domains: []string{"node.proxy-a.example"}},
		{ProxyEgressID: "proxy-b", Domains: []string{"node.proxy-b.example"}},
	}
	if err := CheckProxyDNSDependencyCycles(compiled, dependencies); err == nil {
		t.Fatal("all-egress check did not report the a <-> b cycle")
	}
	// The active policy only references proxy-b. Starting from proxy-b still
	// reaches proxy-a, so the cycle remains real.
	if err := CheckProxyDNSDependencyCyclesFrom(compiled, dependencies, []string{"proxy-b"}); err == nil {
		t.Fatal("active cycle starting at proxy-b was missed")
	}
	// An unrelated egress with no edge must not be treated as a start point.
	compiledDirect := proxyDependencyTestCompiled(t, []Rule{
		{ID: "direct-a-node", Domains: []string{"node.proxy-a.example"}, Outcome: Direct()},
		{ID: "proxy-b-to-a", Domains: []string{"node.proxy-b.example"}, Outcome: Proxy("proxy-a")},
		{ID: "proxy-a-to-b", Domains: []string{"other.proxy-a.example"}, Outcome: Proxy("proxy-b")},
	})
	dependencies = []ProxyDNSDependency{
		{ProxyEgressID: "proxy-a", Domains: []string{"node.proxy-a.example"}},
		{ProxyEgressID: "proxy-b", Domains: []string{"node.proxy-b.example", "other.proxy-a.example"}},
	}
	if err := CheckProxyDNSDependencyCyclesFrom(compiledDirect, dependencies, []string{"proxy-a"}); err != nil {
		t.Fatalf("unreferenced cycle was reported for proxy-a: %v", err)
	}
}
