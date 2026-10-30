package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// TestInterfaceRoleChangeRejectedWhileWANLinkReferencesPort pins the
// cross-resource guard for the J1900 role swap. Setting the WAN port to LAN
// while the WAN line still points at it produced a desired generation that
// could never converge (WAN and LAN addresses on one VPP interface).
func TestInterfaceRoleChangeRejectedWhileWANLinkReferencesPort(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a data port, got %q", port)
	}
	seedInterfaceRoleReference(t, server, "wan_link", "j1900-pub", fmt.Sprintf(`{
		"id":"j1900-pub","name":"j1900-pub","interface_id":%q,"enabled":true,
		"wan_type":"static","type":"static","cidr":"10.1.18.24/24","gateway":"10.1.18.1",
		"ipv4":{"mode":"static","address":"10.1.18.24/24","gateway":"10.1.18.1"},
		"ipv6":{"mode":"disabled"}
	}`, port))

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"lan","cidr":"192.168.88.1/24"}`, port, port), cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "referenced by wan link j1900-pub") || !strings.Contains(body, "update or delete it first") {
		t.Fatalf("rejection is not actionable: %s", body)
	}
}

// TestInterfaceRoleChangeRejectedWhileDHCPServiceReferencesPort covers the
// opposite direction: a DHCP service needs a LAN port, so turning that port
// into a WAN must be rejected before it is persisted.
func TestInterfaceRoleChangeRejectedWhileDHCPServiceReferencesPort(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	port := server.canonicalInterfaceID(ctx, "enp4s0")
	if !isProductInterfaceID(port) {
		t.Skipf("test inventory did not expose a data port, got %q", port)
	}
	seedInterfaceRoleReference(t, server, "dhcp_server", "lan-dhcp", fmt.Sprintf(`{
		"id":"lan-dhcp","name":"lan-dhcp","interface_id":%q,"enabled":true,
		"subnet":"192.168.88.0/24","pools":["192.168.88.100-192.168.88.200"]
	}`, port))

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"wan","cidr":"10.1.18.99/24"}`, port, port), cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "referenced by DHCP service lan-dhcp") || !strings.Contains(body, "update or delete it first") {
		t.Fatalf("rejection is not actionable: %s", body)
	}
}

func interfaceRoleTestServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	previous := hostInterfaceInventory
	hostInterfaceInventory = func() []map[string]any {
		return []map[string]any{
			{"id": "enp1s0", "name": "enp1s0", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:01"},
			{"id": "enp3s0", "name": "enp3s0", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:02"},
			{"id": "enp4s0", "name": "enp4s0", "admin_state": "up", "link_state": "up", "mac": "00:50:56:cc:00:03"},
		}
	}
	t.Cleanup(func() { hostInterfaceInventory = previous })
	t.Setenv("LY_ROUTE_MANAGEMENT_INTERFACE", "enp1s0")
	return inventoryTestServer(t)
}

func seedInterfaceRoleReference(t *testing.T, server *Server, resourceType, id, payload string) {
	t.Helper()
	if err := server.store.SaveConfig(context.Background(), persistence.ConfigDocument{
		ResourceType: resourceType,
		ResourceID:   id,
		Payload:      []byte(payload),
		PayloadHash:  "fixture-" + resourceType,
	}); err != nil {
		t.Fatalf("seed %s: %v", resourceType, err)
	}
}
