package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

func proxySNATSaveTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:proxy-snat-save?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "eth0")
	if err := store.SaveConfig(ctx, configDocument(t, "wan_link", "wan-main-static4", map[string]any{
		"id": "wan-main-static4", "name": "WAN", "enabled": true, "type": "static",
		"interface_id": "eth4", "cidr": "10.1.18.25/24", "gateway": "10.1.18.1",
	}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	return New(WithStore(store), WithClock(fixedClock()))
}

func proxySNATSavePayload(snat ...string) map[string]any {
	payload := map[string]any{
		"id": "egress-proxy", "name": "Proxy", "enabled": true,
		"semantic_type": "proxy_egress", "runtime_profile": "xray-tproxy-outbound",
		"underlay_wan_id": "wan-main-static4", "node_id": "node-kurumi",
	}
	if len(snat) > 0 {
		payload["snat_addresses"] = snat
	}
	return payload
}

// The real VPP at 10.1.18.46 rejects "nat44 add address <wan-ip> tenant-vrf N"
// with "NAT address already in use". Saving such an egress must fail as a
// field-level 422 instead of committing a generation that cannot apply.
func TestProxyEgressSaveRequiresSNATForStaticWAN(t *testing.T) {
	server := proxySNATSaveTestServer(t)
	err := server.validateDesiredRuntimePayload(context.Background(), "proxy_egress", "create", proxySNATSavePayload(), false, nil)
	if err == nil {
		t.Fatal("static WAN egress without snat_addresses was accepted")
	}
	var coded *desiredValidationError
	if !errors.As(err, &coded) || coded.Code != "proxy_snat_address_required" {
		t.Fatalf("error = %#v, want proxy_snat_address_required", err)
	}
	for _, required := range []string{"snat_addresses", "10.1.18.0/24", "10.1.18.25", "tenant VRF", `"snat_addresses": ["10.1.18.254"]`} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("save error is not actionable, missing %q: %s", required, err.Error())
		}
	}
}

func TestProxyEgressSaveAcceptsExplicitSNATForStaticWAN(t *testing.T) {
	server := proxySNATSaveTestServer(t)
	if err := server.validateDesiredRuntimePayload(context.Background(), "proxy_egress", "create", proxySNATSavePayload("10.1.18.250"), false, nil); err != nil {
		t.Fatalf("explicit snat_addresses was rejected: %v", err)
	}
}

// A draft egress has no data-path generation yet; binding a node later is the
// normal UI flow and must not require inventing an address up front.
func TestProxyEgressSaveAllowsUnboundDraftWithoutSNAT(t *testing.T) {
	server := proxySNATSaveTestServer(t)
	payload := proxySNATSavePayload()
	delete(payload, "node_id")
	if err := server.validateDesiredRuntimePayload(context.Background(), "proxy_egress", "create", payload, false, nil); err != nil {
		t.Fatalf("unbound draft was rejected: %v", err)
	}
}

// Dynamic/PPPoE/WAN-group underlays keep the legacy global-FIB handoff: they
// have no static prefix whose interface address could be reused in a tenant
// VRF, so an empty snat_addresses remains valid.
func TestProxyEgressSaveAllowsLegacyUnderlayWithoutSNAT(t *testing.T) {
	server := proxySNATSaveTestServer(t)
	ctx := context.Background()
	if err := server.store.SaveConfig(ctx, configDocument(t, "wan_link", "wan-dhcp", map[string]any{
		"id": "wan-dhcp", "name": "DHCP WAN", "enabled": true, "type": "dhcp",
		"interface_id": "eth5",
	}, fixedClock()())); err != nil {
		t.Fatal(err)
	}
	payload := proxySNATSavePayload()
	payload["underlay_wan_id"] = "wan-dhcp"
	if err := server.validateDesiredRuntimePayload(ctx, "proxy_egress", "create", payload, false, nil); err != nil {
		t.Fatalf("legacy DHCP underlay without snat_addresses was rejected: %v", err)
	}
}
