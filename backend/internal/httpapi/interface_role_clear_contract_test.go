package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

func seedInterfaceContractDocument(t *testing.T, server *Server, resourceID string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.SaveConfig(context.Background(), persistence.ConfigDocument{
		ResourceType: "interface",
		ResourceID:   resourceID,
		Payload:      raw,
		PayloadHash:  "fixture-interface-" + resourceID,
	}); err != nil {
		t.Fatalf("seed interface %s: %v", resourceID, err)
	}
}

func interfaceContractPayload(id, linux, role, cidr string) map[string]any {
	return map[string]any{
		"id":               id,
		"interface_id":     id,
		"name":             id,
		"system_name":      linux,
		"gateway_role":     role,
		"mode_role":        map[string]any{"gateway": role, "bridge": nil},
		"candidate_scopes": []string{role},
		"role_configured":  true,
		"cidr":             cidr,
		"work_mode":        "af_xdp_copy",
		"active_path":      "af_xdp_copy",
	}
}

func interfaceDocumentCount(t *testing.T, server *Server, port string) int {
	t.Helper()
	documents, err := server.store.Configs(context.Background(), "interface")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, document := range documents {
		if strings.EqualFold(server.canonicalInterfaceID(context.Background(), document.ResourceID), port) {
			count++
		}
	}
	return count
}

// A LAN clear must retire the address, the derived role metadata, the DHCP
// service/bindings that belonged to the port, and its runtime address. A
// repeated PATCH must be a no-op, not a second document or a new generation of
// residue.
func TestInterfaceRoleClearCascadesDHCPAndLeavesNoResidue(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	seedInterfaceContractDocument(t, server, port, interfaceContractPayload(port, "enp4s0", "lan", "192.168.89.1/24"))
	seedInterfaceRoleReference(t, server, "dhcp_server", "lan-dhcp", fmt.Sprintf(`{
		"id":"lan-dhcp","name":"lan-dhcp","interface_id":%q,"enabled":true,
		"subnet":"192.168.89.0/24","pools":["192.168.89.100-192.168.89.200"]
	}`, port))
	seedInterfaceRoleReference(t, server, "dhcp_static_binding", "lan-dhcp-reservation", fmt.Sprintf(`{
		"id":"lan-dhcp-reservation","interface_id":%q,"enabled":true,
		"mac":"00:50:56:aa:bb:cc","ip_address":"192.168.89.20"
	}`, port))

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":null,"cidr":null}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"cascade_deleted"`) {
		t.Fatalf("clear response did not report cascaded DHCP deletion: %s", response.Body.String())
	}

	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("cleared interface document missing: ok=%v err=%v", found, err)
	}
	for _, key := range []string{"gateway_role", "role", "mode_role", "candidate_scopes", "role_configured", "cidr", "ip_cidr", "address", "ip"} {
		if _, exists := item[key]; exists {
			t.Fatalf("cleared interface retained derived field %q: %#v", key, item)
		}
	}
	if item["work_mode"] != "af_xdp_copy" || item["active_path"] != "af_xdp_copy" {
		t.Fatalf("role clear changed takeover mode: work_mode=%v active_path=%v", item["work_mode"], item["active_path"])
	}
	if _, found, err := server.desiredItem(ctx, "dhcp_server", "lan-dhcp"); err != nil || found {
		t.Fatalf("DHCP service survived role clear: found=%v err=%v", found, err)
	}
	if _, found, err := server.desiredItem(ctx, "dhcp_static_binding", "lan-dhcp-reservation"); err != nil || found {
		t.Fatalf("DHCP static binding survived role clear: found=%v err=%v", found, err)
	}
	if count := interfaceDocumentCount(t, server, port); count != 1 {
		t.Fatalf("interface documents for %s = %d, want 1", port, count)
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assignments {
		if assignment.LinuxInterface == "enp4s0" {
			t.Fatalf("cleared port still has an address assignment: %#v", assignment)
		}
	}

	again := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":null,"cidr":null}`, cookie)
	if again.Code != http.StatusOK {
		t.Fatalf("repeated clear status = %d, want 200: %s", again.Code, again.Body.String())
	}
	if count := interfaceDocumentCount(t, server, port); count != 1 {
		t.Fatalf("repeated clear produced %d interface documents, want 1", count)
	}
}

// Clearing one LAN must be strictly per-port: the other LAN keeps its role,
// address, DHCP service and runtime assignment.
func TestInterfaceRoleClearKeepsOtherLANUntouched(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	first := server.canonicalInterfaceID(ctx, "enp3s0")
	second := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(first) || !isProductInterfaceID(second) || first == second {
		t.Skipf("test inventory did not expose two data ports: %q %q", first, second)
	}
	seedInterfaceContractDocument(t, server, first, interfaceContractPayload(first, "enp3s0", "lan", "192.168.88.1/24"))
	seedInterfaceContractDocument(t, server, second, interfaceContractPayload(second, "enp4s0", "lan", "192.168.89.1/24"))
	for _, fixture := range []struct {
		id, port, subnet string
	}{{"dhcp-88", first, "192.168.88.0/24"}, {"dhcp-89", second, "192.168.89.0/24"}} {
		seedInterfaceRoleReference(t, server, "dhcp_server", fixture.id, fmt.Sprintf(`{
			"id":%q,"interface_id":%q,"enabled":true,"subnet":%q,
			"pools":[%q]
		}`, fixture.id, fixture.port, fixture.subnet, strings.TrimSuffix(fixture.subnet, ".0/24")+".100-"+strings.TrimSuffix(fixture.subnet, ".0/24")+".199"))
	}

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+first,
		`{"gateway_role":null}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200: %s", response.Code, response.Body.String())
	}

	other, found, err := server.desiredItem(ctx, "interface", second)
	if err != nil || !found {
		t.Fatalf("second LAN missing: ok=%v err=%v", found, err)
	}
	if other["gateway_role"] != "lan" || other["cidr"] != "192.168.89.1/24" {
		t.Fatalf("second LAN was modified by clearing the first: %#v", other)
	}
	if _, found, err := server.desiredItem(ctx, "dhcp_server", "dhcp-89"); err != nil || !found {
		t.Fatalf("second LAN DHCP service was removed: found=%v err=%v", found, err)
	}
	if _, found, err := server.desiredItem(ctx, "dhcp_server", "dhcp-88"); err != nil || found {
		t.Fatalf("first LAN DHCP service survived: found=%v err=%v", found, err)
	}
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawFirst, sawSecond bool
	for _, assignment := range assignments {
		switch assignment.LinuxInterface {
		case "enp3s0":
			sawFirst = true
		case "enp4s0":
			sawSecond = true
		}
	}
	if sawFirst || !sawSecond {
		t.Fatalf("address assignments after per-port clear: first=%v second=%v %#v", sawFirst, sawSecond, assignments)
	}
}

// A direction change is not a takeover request. The stored address belongs to
// the old direction and is retired, but work_mode/active_path must survive.
func TestInterfaceRoleChangeDoesNotChangeTakeoverMode(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	seedInterfaceContractDocument(t, server, port, interfaceContractPayload(port, "enp4s0", "lan", "192.168.89.1/24"))

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		`{"gateway_role":"wan"}`, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("role change status = %d, want 200: %s", response.Code, response.Body.String())
	}
	item, found, err := server.desiredItem(ctx, "interface", port)
	if err != nil || !found {
		t.Fatalf("interface missing: ok=%v err=%v", found, err)
	}
	if item["gateway_role"] != "wan" {
		t.Fatalf("role = %v, want wan", item["gateway_role"])
	}
	if _, exists := item["cidr"]; exists {
		t.Fatalf("old LAN address followed the port into WAN: %#v", item)
	}
	if item["work_mode"] != "af_xdp_copy" || item["active_path"] != "af_xdp_copy" {
		t.Fatalf("role change altered takeover mode: work_mode=%v active_path=%v", item["work_mode"], item["active_path"])
	}
}

// Explicit WAN-side objects are not silently deleted by a role clear. They
// keep the port as WAN and the operator gets an actionable 422; no half-state
// is persisted.
func TestInterfaceRoleClearRejectedWhileWANResourcesReferencePort(t *testing.T) {
	cases := []struct {
		resourceType string
		id           string
		payload      string
		kind         string
	}{
		{"wan_link", "wan-pub", `{"id":"wan-pub","interface_id":%q,"enabled":true,"wan_type":"static","cidr":"10.1.18.24/24","gateway":"10.1.18.1"}`, "wan link"},
		{"port_map", "port-map-pub", `{"id":"port-map-pub","interface_id":%q,"enabled":true,"protocol":"tcp","external_port":8080,"internal_host":"192.168.89.20","internal_port":80}`, "port map"},
		{"nat_static", "nat-pub", `{"id":"nat-pub","interface_id":%q,"enabled":true,"internal_address":"192.168.89.20","external_address":"10.1.18.24"}`, "static NAT mapping"},
	}
	for _, testCase := range cases {
		t.Run(testCase.resourceType, func(t *testing.T) {
			server, cookie := interfaceRoleTestServer(t)
			ctx := withInterfaceIdentities(context.Background())
			port := server.canonicalInterfaceID(ctx, "enp4s0")
			if !isProductInterfaceID(port) {
				t.Skipf("test inventory did not expose a product port, got %q", port)
			}
			seedInterfaceContractDocument(t, server, port, interfaceContractPayload(port, "enp4s0", "wan", "10.1.18.24/24"))
			seedInterfaceRoleReference(t, server, testCase.resourceType, testCase.id, fmt.Sprintf(testCase.payload, port))

			response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
				`{"gateway_role":null}`, cookie)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("clear status = %d, want 422: %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "still referenced by "+testCase.kind) {
				t.Fatalf("rejection is not actionable: %s", response.Body.String())
			}
			item, found, err := server.desiredItem(ctx, "interface", port)
			if err != nil || !found {
				t.Fatalf("interface missing after rejected clear: ok=%v err=%v", found, err)
			}
			if item["gateway_role"] != "wan" || item["cidr"] != "10.1.18.24/24" {
				t.Fatalf("rejected clear changed the document: %#v", item)
			}
		})
	}
}

// Two documents for one physical port (legacy name + product id) collapse to
// exactly one row; a repeated PATCH cannot recreate the duplicate.
func TestInterfaceRolePatchCollapsesDuplicateDocuments(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a product port, got %q", port)
	}
	payload := interfaceContractPayload(port, "enp4s0", "lan", "192.168.89.1/24")
	seedInterfaceContractDocument(t, server, port, payload)
	seedInterfaceContractDocument(t, server, "enp4s0", payload)
	if count := interfaceDocumentCount(t, server, port); count != 2 {
		t.Fatalf("fixture documents = %d, want 2", count)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
			`{"gateway_role":"lan","cidr":"192.168.89.1/24"}`, cookie)
		if response.Code != http.StatusOK {
			t.Fatalf("patch %d status = %d: %s", attempt, response.Code, response.Body.String())
		}
		if count := interfaceDocumentCount(t, server, port); count != 1 {
			t.Fatalf("after patch %d interface documents for %s = %d, want 1", attempt, port, count)
		}
	}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, item := range items {
		if item["id"] == port {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("desired interface items for %s = %d, want 1", port, matches)
	}
}
