package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// twoDataPorts returns two distinct product port ids from the fake inventory
// used by interfaceRoleTestServer.
func twoDataPorts(t *testing.T, server *Server) (string, string) {
	t.Helper()
	ctx := withInterfaceIdentities(context.Background())
	first := server.canonicalInterfaceID(ctx, "enp4s0")
	second := server.canonicalInterfaceID(ctx, "enp3s0")
	if !isProductInterfaceID(first) || !isProductInterfaceID(second) || strings.EqualFold(first, second) {
		t.Skipf("test inventory did not expose two data ports, got %q and %q", first, second)
	}
	return first, second
}

func postTestLANLine(t *testing.T, server *Server, cookie *http.Cookie, port, displayName, cidr string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"interface_id":%q,"name":%q,"display_name":%q,"gateway_role":"lan","cidr":%q,"kind":"interface","type":"interface"}`,
		port, port, port, displayName, cidr)
	return authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/interfaces", body, cookie)
}

func postTestLANBridge(t *testing.T, server *Server, cookie *http.Cookie, id, displayName, cidr string, members ...string) *httptest.ResponseRecorder {
	t.Helper()
	quoted := make([]string, 0, len(members))
	for _, member := range members {
		quoted = append(quoted, fmt.Sprintf("%q", member))
	}
	body := fmt.Sprintf(`{"id":%q,"interface_id":%q,"name":%q,"display_name":%q,"gateway_role":"lan","kind":"lan_bridge","type":"lan_bridge","bridge_members":[%s],"cidr":%q}`,
		id, id, id, displayName, strings.Join(quoted, ","), cidr)
	return authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/interfaces", body, cookie)
}

func postTestWANLink(t *testing.T, server *Server, cookie *http.Cookie, id, name, port, cidr string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"name":%q,"interface_id":%q,"gateway_role":"wan","wan_type":"static","type":"static","cidr":%q,"gateway":%q,"ipv4":{"mode":"static","address":%q,"gateway":%q},"ipv6":{"mode":"disabled"}}`,
		id, name, port, cidr, staticGateway(cidr), cidr, staticGateway(cidr))
	return authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links", body, cookie)
}

func staticGateway(cidr string) string {
	parts := strings.SplitN(cidr, "/", 2)
	octets := strings.Split(parts[0], ".")
	if len(octets) != 4 {
		return parts[0]
	}
	return octets[0] + "." + octets[1] + "." + octets[2] + ".1"
}

// TestInterfaceRoleChangeRejectedWhileLANLineReferencesPort is the J1900
// scenario: a named LAN line still owns the port's LAN address, so moving the
// port to WAN must be rejected instead of clearing the LAN and leaving a
// contradictory desired generation behind.
func TestInterfaceRoleChangeRejectedWhileLANLineReferencesPort(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	port, other := twoDataPorts(t, server)

	lan := postTestLANBridge(t, server, cookie, "lan-bridge-1", "lan-main", "192.168.88.1/24", port, other)
	if lan.Code != http.StatusOK {
		t.Fatalf("create LAN line status = %d: %s", lan.Code, lan.Body.String())
	}

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"wan","cidr":"10.1.18.99/24"}`, port, port), cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"invalid_desired_resource", "referenced by LAN line lan-main", "update or delete it first"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rejection missing %q: %s", want, body)
		}
	}

	// The rejected write must not persist anything.
	item, found, err := server.desiredItem(context.Background(), "interface", port)
	if err != nil {
		t.Fatal(err)
	}
	if found && stringField(item, "gateway_role") == "wan" {
		t.Fatalf("rejected role change was persisted: %#v", item)
	}
}

func TestInterfaceRoleChangeRejectedWhileWANLineStillOwnsPort(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	port, _ := twoDataPorts(t, server)
	seedInterfaceRoleReference(t, server, "wan_link", "wan-pub", fmt.Sprintf(`{
		"id":"wan-pub","name":"wan-pub","interface_id":%q,"enabled":true,
		"wan_type":"static","type":"static","cidr":"10.1.18.24/24","gateway":"10.1.18.1"
	}`, port))

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"lan","cidr":"192.168.88.1/24"}`, port, port), cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "referenced by wan link wan-pub") || !strings.Contains(body, "update or delete it first") {
		t.Fatalf("rejection is not actionable: %s", body)
	}
}

func TestInterfaceRoleChangeAllowedWithoutReferences(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	port, _ := twoDataPorts(t, server)

	response := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"wan","cidr":"10.1.18.99/24"}`, port, port), cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("unreferenced role change status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestInterfaceRoleChangeAllowedAfterReferenceDeleted(t *testing.T) {
	server, cookie := interfaceRoleTestServer(t)
	port, other := twoDataPorts(t, server)

	lan := postTestLANBridge(t, server, cookie, "lan-bridge-1", "lan-main", "192.168.88.1/24", port, other)
	if lan.Code != http.StatusOK {
		t.Fatalf("create LAN line status = %d: %s", lan.Code, lan.Body.String())
	}
	rejected := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"wan","cidr":"10.1.18.99/24"}`, port, port), cookie)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("role change status = %d, want 422: %s", rejected.Code, rejected.Body.String())
	}
	if deleted := authenticatedJSONRequest(t, server, http.MethodDelete, "/api/v1/interfaces/lan-bridge-1", "", cookie); deleted.Code != http.StatusOK {
		t.Fatalf("delete LAN line status = %d: %s", deleted.Code, deleted.Body.String())
	}
	accepted := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/interfaces/"+port,
		fmt.Sprintf(`{"id":%q,"interface_id":%q,"gateway_role":"wan","cidr":"10.1.18.99/24"}`, port, port), cookie)
	if accepted.Code != http.StatusOK {
		t.Fatalf("role change after delete status = %d, want 200: %s", accepted.Code, accepted.Body.String())
	}
}

func TestDesiredResourceNameUniqueness(t *testing.T) {
	t.Run("duplicate LAN line name is rejected", func(t *testing.T) {
		server, cookie := interfaceRoleTestServer(t)
		port, other := twoDataPorts(t, server)
		if res := postTestLANBridge(t, server, cookie, "lan-bridge-1", "lan-main", "192.168.88.1/24", port, other); res.Code != http.StatusOK {
			t.Fatalf("create first LAN line: %d %s", res.Code, res.Body.String())
		}
		duplicate := postTestLANBridge(t, server, cookie, "lan-bridge-2", "lan-main", "192.168.89.1/24", other)
		if duplicate.Code != http.StatusUnprocessableEntity {
			t.Fatalf("duplicate name status = %d, want 422: %s", duplicate.Code, duplicate.Body.String())
		}
		if body := duplicate.Body.String(); !strings.Contains(body, "invalid_desired_resource") || !strings.Contains(body, "already exists") {
			t.Fatalf("duplicate name rejection is not actionable: %s", body)
		}
	})

	t.Run("distinct LAN line name is accepted", func(t *testing.T) {
		server, cookie := interfaceRoleTestServer(t)
		port, other := twoDataPorts(t, server)
		if res := postTestLANBridge(t, server, cookie, "lan-bridge-1", "lan-main", "192.168.88.1/24", port, other); res.Code != http.StatusOK {
			t.Fatalf("create first LAN line: %d %s", res.Code, res.Body.String())
		}
		accepted := postTestLANBridge(t, server, cookie, "lan-bridge-2", "lan-other", "192.168.89.1/24", other)
		if accepted.Code != http.StatusOK {
			t.Fatalf("distinct name status = %d, want 200: %s", accepted.Code, accepted.Body.String())
		}
	})

	t.Run("duplicate WAN line name is rejected", func(t *testing.T) {
		server, cookie := interfaceRoleTestServer(t)
		port, other := twoDataPorts(t, server)
		if res := postTestWANLink(t, server, cookie, "wan-a", "WAN Primary", port, "198.51.100.2/24"); res.Code != http.StatusOK {
			t.Fatalf("create first WAN line: %d %s", res.Code, res.Body.String())
		}
		duplicate := postTestWANLink(t, server, cookie, "wan-b", "WAN Primary", other, "203.0.113.2/24")
		if duplicate.Code != http.StatusUnprocessableEntity {
			t.Fatalf("duplicate WAN name status = %d, want 422: %s", duplicate.Code, duplicate.Body.String())
		}
		if body := duplicate.Body.String(); !strings.Contains(body, "already exists") {
			t.Fatalf("duplicate WAN name rejection is not actionable: %s", body)
		}
	})
}

func TestDesiredAddressValidation(t *testing.T) {
	cases := []struct {
		name       string
		cidr       string
		wantStatus int
		wantBody   string
	}{
		{name: "invalid IPv4 octet", cidr: "192.168.88.999/24", wantStatus: http.StatusUnprocessableEntity, wantBody: "not a valid IPv4/IPv6 address"},
		{name: "invalid prefix length", cidr: "192.168.88.1/33", wantStatus: http.StatusUnprocessableEntity, wantBody: "invalid prefix length"},
		{name: "default route", cidr: "0.0.0.0/0", wantStatus: http.StatusUnprocessableEntity, wantBody: "not a usable subnet"},
		{name: "overlaps existing LAN", cidr: "192.168.88.1/24", wantStatus: http.StatusUnprocessableEntity, wantBody: "overlaps existing"},
		{name: "valid unused subnet", cidr: "192.168.90.1/24", wantStatus: http.StatusOK},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, cookie := interfaceRoleTestServer(t)
			port, other := twoDataPorts(t, server)
			if seed := postTestLANLine(t, server, cookie, port, "lan-main", "192.168.88.1/24"); seed.Code != http.StatusOK {
				t.Fatalf("seed LAN line: %d %s", seed.Code, seed.Body.String())
			}
			response := postTestLANLine(t, server, cookie, other, "lan-second", testCase.cidr)
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, testCase.wantStatus, response.Body.String())
			}
			if testCase.wantBody != "" && !strings.Contains(response.Body.String(), testCase.wantBody) {
				t.Fatalf("body missing %q: %s", testCase.wantBody, response.Body.String())
			}
			if testCase.wantStatus != http.StatusOK {
				// Dirty data must not be persisted.
				if _, found, err := server.desiredItem(context.Background(), "interface", other); err != nil {
					t.Fatal(err)
				} else if found {
					t.Fatalf("rejected LAN line was persisted for %s", other)
				}
			}
		})
	}
}

func TestDesiredWANAddressValidation(t *testing.T) {
	cases := []struct {
		name       string
		cidr       string
		wantStatus int
		wantBody   string
	}{
		{name: "invalid prefix length", cidr: "198.51.100.2/33", wantStatus: http.StatusUnprocessableEntity, wantBody: "invalid prefix length"},
		{name: "overlaps existing WAN", cidr: "198.51.100.10/24", wantStatus: http.StatusUnprocessableEntity, wantBody: "overlaps existing"},
		{name: "valid unused WAN subnet", cidr: "203.0.113.2/24", wantStatus: http.StatusOK},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, cookie := interfaceRoleTestServer(t)
			port, other := twoDataPorts(t, server)
			if seed := postTestWANLink(t, server, cookie, "wan-a", "WAN Primary", port, "198.51.100.2/24"); seed.Code != http.StatusOK {
				t.Fatalf("seed WAN line: %d %s", seed.Code, seed.Body.String())
			}
			response := postTestWANLink(t, server, cookie, "wan-b", "WAN Backup", other, testCase.cidr)
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, testCase.wantStatus, response.Body.String())
			}
			if testCase.wantBody != "" && !strings.Contains(response.Body.String(), testCase.wantBody) {
				t.Fatalf("body missing %q: %s", testCase.wantBody, response.Body.String())
			}
		})
	}
}
