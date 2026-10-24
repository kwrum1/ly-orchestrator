package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
	serviceRuntime "ly-route/backend/internal/runtime/service"
)

func newIPv6PlanTestServer(t *testing.T, documents ...persistence.ConfigDocument) *Server {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-ipv6-plan-"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, document := range documents {
		if err := store.SaveConfig(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	return New(WithStore(store), WithClock(fixedClock()))
}

func dnsmasqArtifact(t *testing.T, plan RuntimePlan) serviceRuntime.RenderedArtifact {
	t.Helper()
	for _, artifact := range plan.RuntimeArtifacts {
		if artifact.Service == serviceRuntime.Dnsmasq {
			return artifact
		}
	}
	t.Fatalf("runtime plan has no dnsmasq artifact: %#v", plan.RuntimeArtifacts)
	return serviceRuntime.RenderedArtifact{}
}

func TestRuntimePlanPPPoELiveDelegatedPrefixFeedsDnsmasqIPv6(t *testing.T) {
	now := fixedClock()()
	statusDir := t.TempDir()
	status := `{"state":"connected","interface":"pppoe_session_ly0","session":{"delegated_prefix":"2001:db8:100::/56"}}`
	if err := os.WriteFile(filepath.Join(statusDir, "wan0.json"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LY_ROUTE_PPPOE_STATUS_DIR", statusDir)
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "wan_link", "wan0", map[string]any{
			"id": "wan0", "type": "pppoe", "interface_id": "eth1", "username": "subscriber", "password": "secret",
			"ipv6": map[string]any{"mode": "dhcpv6_pd", "prefix_delegation": true},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-pppoe")
	if err != nil {
		t.Fatal(err)
	}
	content := dnsmasqArtifact(t, plan).Content
	for _, want := range []string{
		"enable-ra\n",
		"interface=lylan-lan0\n",
		"dhcp-range=set:lyroute-lan0,2001:db8:100::,ra-stateless,64,12h\n",
		"dhcp-option=tag:lyroute-lan0,option6:dns-server,[2001:db8:100::1]\n",
		"ra-param=lylan-lan0,30,0\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("PPPoE PD dnsmasq config missing %q:\n%s", want, content)
		}
	}
	for _, artifact := range plan.RuntimeArtifacts {
		if artifact.Service == serviceRuntime.IPv6RA {
			t.Fatalf("gateway path still renders radvd: %#v", artifact)
		}
	}
}

func TestRuntimePlanStaticIPv6LANFeedsDnsmasqIPv6(t *testing.T) {
	now := fixedClock()()
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "static", "address": "2001:db8:20::1/64"},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-static")
	if err != nil {
		t.Fatal(err)
	}
	content := dnsmasqArtifact(t, plan).Content
	for _, want := range []string{
		"dhcp-range=set:lyroute-lan0,2001:db8:20::,ra-stateless,64,12h",
		"dhcp-option=tag:lyroute-lan0,option6:dns-server,[2001:db8:20::1]",
		"ra-param=lylan-lan0,30,1800",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("static IPv6 dnsmasq config missing %q:\n%s", want, content)
		}
	}
}

func TestRuntimePlanStaticSameInterfaceWANPrefixFeedsDnsmasqIPv6(t *testing.T) {
	now := fixedClock()()
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "wan_link", "wan0", map[string]any{
			"id": "wan0", "type": "static6", "interface_id": "eth1",
			"ipv6": map[string]any{"mode": "static", "address": "2001:db8:30::2/64", "gateway": "fe80::1"},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-same-interface")
	if err != nil {
		t.Fatal(err)
	}
	content := dnsmasqArtifact(t, plan).Content
	for _, want := range []string{
		"dhcp-range=set:lyroute-lan0,2001:db8:30::,ra-stateless,64,12h",
		"ra-param=lylan-lan0,30,1800",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("same-interface static prefix config missing %q:\n%s", want, content)
		}
	}
}

func TestRuntimePlanNoIPv6PrefixKeepsDnsmasqWithoutRA(t *testing.T) {
	now := fixedClock()()
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "wan_link", "wan0", map[string]any{
			"id": "wan0", "type": "pppoe", "interface_id": "eth1", "username": "subscriber", "password": "secret",
			"ipv6": map[string]any{"mode": "dhcpv6_pd"},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-no-prefix")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "no delegated or static prefix") {
		t.Fatalf("missing no-prefix warning: %#v", plan.Warnings)
	}
	for _, artifact := range plan.RuntimeArtifacts {
		if artifact.Service != serviceRuntime.Dnsmasq {
			continue
		}
		if strings.Contains(artifact.Content, "enable-ra") || strings.Contains(artifact.Content, "option6:") {
			t.Fatalf("no-prefix dnsmasq config contains IPv6 state:\n%s", artifact.Content)
		}
	}
}

func TestRuntimePlanMultipleLANsGetDistinctDelegated64s(t *testing.T) {
	now := fixedClock()()
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "interface", "lan1", map[string]any{
			"id": "lan1", "interface_id": "lan1", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "wan_link", "wan0", map[string]any{
			"id": "wan0", "type": "pppoe", "interface_id": "eth1",
			"ipv6": map[string]any{"delegated_prefix": "2001:db8:100::/56"},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-multi-lan")
	if err != nil {
		t.Fatal(err)
	}
	content := dnsmasqArtifact(t, plan).Content
	for _, want := range []string{
		"interface=lylan-lan0\n",
		"interface=lylan-lan1\n",
		"dhcp-range=set:lyroute-lan0,2001:db8:100::,ra-stateless,64,12h",
		"dhcp-range=set:lyroute-lan1,2001:db8:100:1::,ra-stateless,64,12h",
		"ra-param=lylan-lan0,30,0",
		"ra-param=lylan-lan1,30,0",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("multi-LAN dnsmasq config missing %q:\n%s", want, content)
		}
	}
}

func TestRuntimePlanInvalidIPv6PrefixWarnsAndOmitsIPv6State(t *testing.T) {
	now := fixedClock()()
	server := newIPv6PlanTestServer(t,
		configDocument(t, "interface", "lan0", map[string]any{
			"id": "lan0", "interface_id": "lan0", "gateway_role": "lan",
			"ipv6": map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan0"},
		}, now),
		configDocument(t, "wan_link", "wan0", map[string]any{
			"id": "wan0", "type": "pppoe", "interface_id": "eth1", "username": "subscriber", "password": "secret",
			"ipv6": map[string]any{"delegated_prefix": "2001:db8:100::/65"},
		}, now),
	)

	plan, err := server.buildRuntimePlan(context.Background(), "txn-ipv6-invalid")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "too small") {
		t.Fatalf("invalid prefix warning missing: %#v", plan.Warnings)
	}
	for _, artifact := range plan.RuntimeArtifacts {
		if artifact.Service == serviceRuntime.Dnsmasq && strings.Contains(artifact.Content, "enable-ra") {
			t.Fatalf("invalid prefix produced dnsmasq RA:\n%s", artifact.Content)
		}
		if artifact.Service == serviceRuntime.IPv6RA {
			t.Fatalf("invalid prefix produced radvd artifact: %#v", artifact)
		}
	}
}
