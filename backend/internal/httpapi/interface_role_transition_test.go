package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// seedInterfaceLineDocument stores an interface document that owns a complete
// line identity: a label, an address in two spellings, address-family plans and
// runtime readback residue. A role edit must retire all of it.
func seedInterfaceLineDocument(t *testing.T, server *Server, port, linux, role, cidr, display string) {
	t.Helper()
	payload := interfaceContractPayload(port, linux, role, cidr)
	payload["display_name"] = display
	payload["address"] = cidr
	payload["gateway"] = "10.1.18.1"
	payload["ipv4"] = map[string]any{"mode": "static", "address": cidr, "gateway": "10.1.18.1"}
	payload["ipv6"] = map[string]any{"mode": "delegated_prefix", "source_wan_id": "wan-main", "prefix_delegation": true}
	payload["dns_servers"] = []any{"192.168.89.1"}
	payload["nat"] = true
	payload["runtime_state"] = "applied"
	payload["route_ready"] = true
	payload["pppoe_state"] = "connected"
	payload["pppoe_interface"] = "pppoe-session0"
	payload["address_assignment_proven"] = true
	payload["addresses"] = []any{cidr}
	payload["capabilities"] = []any{map[string]any{"name": "vpp_runtime_apply", "state": "available"}}
	seedInterfaceContractDocument(t, server, port, payload)
}

func interfaceRoleTransitionPort(t *testing.T, server *Server) string {
	t.Helper()
	port := server.canonicalInterfaceID(withInterfaceIdentities(context.Background()), "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	return port
}

// A LAN line that becomes a WAN port must not leave the line behind: the label
// made /lan render a ghost row with an empty address, and the old IPv4/IPv6
// plans kept feeding runtime compilation.
func TestInterfaceRoleTransitionRetiresLineIdentity(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := interfaceRoleTransitionPort(t, server)
	seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":"wan"}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("role change status = %d, want 200: %s", response.Code, response.Body.String())
	}

	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after role change: ok=%v err=%v", found, err)
	}
	if item["gateway_role"] != "wan" {
		t.Fatalf("role = %v, want wan: %#v", item["gateway_role"], item)
	}
	for _, key := range []string{
		"display_name", "cidr", "ip_cidr", "address", "ip", "gateway",
		"ipv4_address", "ipv6_address", "ipv4", "ipv6",
		"dns_servers", "nat",
		"route_ready", "pppoe_state", "pppoe_interface", "addresses",
		"address_assignment_proven",
	} {
		if _, exists := item[key]; exists {
			t.Fatalf("role change kept line residue %q: %#v", key, item)
		}
	}
	if item["runtime_state"] == "applied" {
		t.Fatalf("role change kept the old applied runtime state: %#v", item)
	}
	if item["work_mode"] != "af_xdp_copy" || item["active_path"] != "af_xdp_copy" {
		t.Fatalf("role change altered takeover mode: work_mode=%v active_path=%v", item["work_mode"], item["active_path"])
	}

	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range items {
		if stringField(candidate, "id") != port {
			continue
		}
		if role, _ := interfaceDesiredRole(candidate); role == "lan" {
			t.Fatalf("the port is still rendered as a LAN line: %#v", candidate)
		}
		if strings.EqualFold(stringField(candidate, "display_name"), "lan-main") {
			t.Fatalf("the stale line label survived the role change: %#v", candidate)
		}
	}
}

// A direction change is a cross-resource event: every object written against
// the old line must be moved or deleted first, and the rejected PATCH must not
// persist a half-cleared document.
func TestInterfaceRoleTransitionRejectedWhileLineIsReferenced(t *testing.T) {
	cases := []struct {
		name         string
		resourceType string
		id           string
		kind         string
		payload      string
	}{
		{"port map", "port_map", "port-map-lan", "port map",
			`{"id":"port-map-lan","interface_id":%q,"enabled":true,"protocol":"tcp","external_port":8080,"internal_host":"192.168.89.20","internal_port":80}`},
		{"static NAT", "nat_static", "nat-lan", "static NAT mapping",
			`{"id":"nat-lan","interface_id":%q,"enabled":true,"internal_address":"192.168.89.20","external_address":"10.1.18.24"}`},
		{"proxy egress", "proxy_egress", "egress-lan", "proxy egress underlay",
			`{"id":"egress-lan","underlay_wan_id":%q,"enabled":true,"kind":"egress"}`},
		{"DHCP service", "dhcp_server", "dhcp-lan", "DHCP service",
			`{"id":"dhcp-lan","interface_id":%q,"enabled":true,"subnet":"192.168.89.0/24","pools":["192.168.89.100-192.168.89.200"]}`},
		{"DHCP binding", "dhcp_static_binding", "dhcp-binding-lan", "DHCP static binding",
			`{"id":"dhcp-binding-lan","interface_id":%q,"enabled":true,"mac":"00:50:56:aa:bb:cc","ip_address":"192.168.89.20"}`},
		{"DNS upstream", "dns_upstream", "dns-lan", "DNS upstream",
			`{"id":"dns-lan","interface_id":%q,"enabled":true,"address":"192.168.89.1"}`},
		{"bond", "interface_bond", "bond-lan", "interface bond",
			`{"id":"bond-lan","enabled":true,"members":[%q]}`},
		{"route policy ingress", "route_policy", "route-lan", "route policy",
			`{"id":"route-lan","enabled":true,"ingress_interface":%q,"action":"route"}`},
		{"traffic policy ingress", "traffic_control", "traffic-lan", "traffic control policy",
			`{"id":"traffic-lan","enabled":true,"lan_interface":%q,"action":"route"}`},
		{"WAN link", "wan_link", "wan-lan", "wan link",
			`{"id":"wan-lan","interface_id":%q,"enabled":true,"wan_type":"static","cidr":"10.1.18.24/24"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, cookie := interfaceRoleTestServer(t)
			ctx := withInterfaceIdentities(context.Background())
			port := interfaceRoleTransitionPort(t, server)
			seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")
			seedInterfaceRoleReference(t, server, testCase.resourceType, testCase.id, fmt.Sprintf(testCase.payload, port))

			response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
				`{"gateway_role":"wan"}`, cookie)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("role change status = %d, want 422: %s", response.Code, response.Body.String())
			}
			if body := response.Body.String(); !strings.Contains(body, "referenced by "+testCase.kind) || !strings.Contains(body, "update or delete it first") {
				t.Fatalf("rejection is not actionable: %s", body)
			}
			item, found, err := server.desiredItem(ctx, "interface", port)
			if err != nil || !found {
				t.Fatalf("interface missing after rejected role change: ok=%v err=%v", found, err)
			}
			if item["gateway_role"] != "lan" || item["display_name"] != "lan-main" || item["cidr"] != "192.168.89.1/24" {
				t.Fatalf("rejected role change mutated the stored line: %#v", item)
			}
		})
	}
}

// Re-saving the current role is a no-op: it must not clear the line or rewrite
// anything the caller did not change.
func TestInterfaceRoleRepeatSaveKeepsDocumentStable(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := interfaceRoleTransitionPort(t, server)
	seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")

	body := `{"gateway_role":"lan","cidr":"192.168.89.1/24"}`
	first := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port, body, cookie)
	if first.Code != http.StatusOK {
		t.Fatalf("first save status = %d: %s", first.Code, first.Body.String())
	}
	firstItem, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after first save: ok=%v err=%v", found, err)
	}
	second := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port, body, cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("second save status = %d: %s", second.Code, second.Body.String())
	}
	secondItem, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after second save: ok=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(firstItem, secondItem) {
		t.Fatalf("repeated identical role save changed the document:\nfirst:  %#v\nsecond: %#v", firstItem, secondItem)
	}
	if secondItem["display_name"] != "lan-main" || secondItem["cidr"] != "192.168.89.1/24" {
		t.Fatalf("idempotent save lost the LAN line: %#v", secondItem)
	}
}

// The reverse transition must work in one save: the caller supplies the new LAN
// label and address, and no WAN residue shadows them.
func TestInterfaceRoleTransitionBackToLANWithNewAddress(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := interfaceRoleTransitionPort(t, server)
	seedInterfaceLineDocument(t, server, port, "enp4s0", "wan", "10.1.18.24/24", "wan-main")

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":"lan","cidr":"192.168.89.1/24","address":"192.168.89.1/24","display_name":"lan-new","ipv4":{"mode":"static","address":"192.168.89.1/24"},"ipv6":{"mode":"disabled"}}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("role change status = %d, want 200: %s", response.Code, response.Body.String())
	}
	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing: ok=%v err=%v", found, err)
	}
	if item["gateway_role"] != "lan" || item["display_name"] != "lan-new" {
		t.Fatalf("new LAN identity was not applied: %#v", item)
	}
	if item["cidr"] != "192.168.89.1/24" || item["address"] != "192.168.89.1/24" {
		t.Fatalf("new LAN address was not applied: %#v", item)
	}
	if strings.Contains(fmt.Sprint(item), "10.1.18.24/24") {
		t.Fatalf("old WAN address survived the transition: %#v", item)
	}
	ipv4, _ := item["ipv4"].(map[string]any)
	if ipv4["address"] != "192.168.89.1/24" {
		t.Fatalf("new IPv4 plan was not applied: %#v", item["ipv4"])
	}
}

// Clearing a role uses the same atomic retirement, not only the address.
func TestInterfaceRoleClearRetiresLineIdentity(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := interfaceRoleTransitionPort(t, server)
	seedInterfaceLineDocument(t, server, port, "enp4s0", "lan", "192.168.89.1/24", "lan-main")

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":null}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("role clear status = %d, want 200: %s", response.Code, response.Body.String())
	}
	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing after clear: ok=%v err=%v", found, err)
	}
	for _, key := range []string{"display_name", "cidr", "address", "ipv4", "ipv6", "route_ready", "pppoe_state"} {
		if _, exists := item[key]; exists {
			t.Fatalf("role clear kept line residue %q: %#v", key, item)
		}
	}
}
