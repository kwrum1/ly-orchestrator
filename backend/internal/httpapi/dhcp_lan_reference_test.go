package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// A DHCP server whose port is not (or is no longer) a configured LAN must not
// fail the whole generation: it used to block every unrelated apply behind
// "DHCP service interface X is not a configured logical LAN".
func TestDHCPServerOnNonLANIsSkippedWithWarning(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	items, err := server.desiredItems(ctx, "dhcp_server")
	if err != nil || len(items) != 0 {
		t.Fatalf("clean fixture expected: %v %d", err, len(items))
	}
	// A LAN interface document without an address (the port is being moved to
	// WAN) plus a DHCP server that still points at it.
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "interface",
		ResourceID:   "lab30",
		Payload:      []byte(`{"id":"lab30","interface_id":"lab30","gateway_role":"wan","name":"eth2"}`),
		PayloadHash:  "fixture-interface",
	}); err != nil {
		t.Fatalf("seed interface: %v", err)
	}
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "dhcp_server",
		ResourceID:   "uiacc-dhcp-nonlan",
		Payload:      []byte(`{"id":"uiacc-dhcp-nonlan","interface_id":"uiacc-no-such-port","subnet":"192.168.30.0/24","pools":["192.168.30.100-192.168.30.110"],"enabled":true}`),
		PayloadHash:  "fixture-dhcp",
	}); err != nil {
		t.Fatalf("seed dhcp: %v", err)
	}

	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	plans, _, warnings, err := server.currentDHCPServers(ctx, assignments, true)
	if err != nil {
		t.Fatalf("an unusable DHCP binding must not fail the plan: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("expected the non-LAN server to be skipped, got %d plans", len(plans))
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "uiacc-dhcp-nonlan") || !strings.Contains(joined, "skipped") {
		t.Fatalf("expected a skip warning naming the service, got %q", joined)
	}
}

// A DHCP write that points at something that is not a port must be rejected with
// a message the operator can act on.
func TestDHCPServerWriteRejectsUnknownInterface(t *testing.T) {
	server, cookie := inventoryTestServer(t)
	response := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/dhcp/servers",
		`{"id":"uiacc-dhcp-bad","interface_id":"uiacc-no-such-port","name":"bad","subnet":"192.168.10.0/24","pools":["192.168.10.100-192.168.10.110"],"enabled":true}`, cookie)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "is not a port on this device") {
		t.Fatalf("message is not actionable: %s", body)
	}
}

// The canonical reference resolver accepts the product id, the kernel name and
// the logical display name for the same port.
func TestCanonicalInterfaceReferenceAcceptsEveryName(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())
	items := server.interfaceIdentities(ctx)
	if len(items) == 0 {
		t.Skip("host exposes no interfaces")
	}
	identity := items[0]
	for _, spelling := range []string{identity.ID, identity.LinuxInterface} {
		if spelling == "" {
			continue
		}
		if got := server.canonicalInterfaceReference(ctx, spelling); got != identity.ID {
			t.Fatalf("%q resolved to %q, want %q", spelling, got, identity.ID)
		}
	}
	// A logical name stored on the interface document is accepted as well.
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "interface",
		ResourceID:   identity.LinuxInterface,
		Payload:      []byte(`{"id":"` + identity.LinuxInterface + `","interface_id":"` + identity.LinuxInterface + `","display_name":"LAN10","name":"` + identity.ID + `","gateway_role":"lan","cidr":"192.0.2.1/24"}`),
		PayloadHash:  "fixture-interface-name",
	}); err != nil {
		t.Fatalf("seed interface: %v", err)
	}
	if got := server.canonicalInterfaceReference(ctx, "lan10"); got != identity.ID {
		t.Fatalf("logical name resolved to %q, want %q", got, identity.ID)
	}
}
