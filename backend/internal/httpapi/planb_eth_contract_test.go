package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// Plan B: the desired state stores and returns the product id, and the kernel
// netdev is only an internal attribute. These tests pin the contract for every
// resource that names a port.
func TestInterfaceWriteStoresProductIDAndKeepsKernelAsAttribute(t *testing.T) {
	installIdentityFixture(t)
	server, cookie := inventoryTestServer(t)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/interfaces",
		`{"id":"ens256","interface_id":"ens256","display_name":"LAN-A","gateway_role":"lan","cidr":"192.0.2.1/24","kind":"interface","type":"interface"}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create by kernel name: %d %s", created.Code, created.Body.String())
	}
	body := created.Body.String()
	for _, want := range []string{`"id":"eth2"`, `"interface_id":"eth2"`, `"name":"eth2"`, `"system_name":"ens256"`, `"display_name":"LAN-A"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stored interface missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"id":"ens256"`) {
		t.Fatalf("kernel name leaked into the document key: %s", body)
	}

	stored, ok, err := server.desiredItem(context.Background(), "interface", "eth2")
	if err != nil || !ok {
		t.Fatalf("eth2 document not found: ok=%v err=%v", ok, err)
	}
	if stringField(stored, "id") != "eth2" || stringField(stored, "system_name") != "ens256" {
		t.Fatalf("stored document = %#v", stored)
	}

	// Every other spelling of the same port addresses the same document.
	for _, reference := range []string{"eth2", "ens256", "00:50:56:3a:14:03", "0000:1b:00.0", "LAN-A"} {
		item := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/interfaces/"+reference, "", cookie)
		if item.Code != http.StatusOK {
			t.Fatalf("read by %q: %d %s", reference, item.Code, item.Body.String())
		}
		if !strings.Contains(item.Body.String(), `"id":"eth2"`) {
			t.Fatalf("read by %q did not return eth2: %s", reference, item.Body.String())
		}
	}
}

func TestInterfaceWriteRejectsMissingPortWithActionableMessage(t *testing.T) {
	installIdentityFixture(t)
	server, cookie := inventoryTestServer(t)

	for _, reference := range []string{"eth9", "ens999", "00:50:56:aa:bb:cc", "0000:99:00.0"} {
		res := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/interfaces",
			`{"id":"`+reference+`","gateway_role":"lan","cidr":"198.18.9.1/24"}`, cookie)
		if res.Code != http.StatusUnprocessableEntity {
			t.Fatalf("write by %q = %d, want 422: %s", reference, res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), "is not a port on this device") || !strings.Contains(res.Body.String(), "interface list") {
			t.Fatalf("write by %q message not actionable: %s", reference, res.Body.String())
		}
	}

	// A logical assignment keeps its own id: it is not a port reference.
	logical := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/interfaces",
		`{"id":"lan-bridge-1","name":"lan-bridge-1","gateway_role":"lan","cidr":"198.18.10.1/24","kind":"lan_bridge"}`, cookie)
	if logical.Code != http.StatusOK {
		t.Fatalf("logical bridge rejected: %d %s", logical.Code, logical.Body.String())
	}
}

func TestWANLinkWriteStoresProductIDAndReadbackKeepsKernelAttribute(t *testing.T) {
	installIdentityFixture(t)
	server, cookie := inventoryTestServer(t)

	created := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/gateway/wan-links",
		`{"id":"uiacc-wan-eth","interface_id":"ens224","wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, cookie)
	if created.Code != http.StatusOK {
		t.Fatalf("create WAN by kernel name: %d %s", created.Code, created.Body.String())
	}
	body := created.Body.String()
	if !strings.Contains(body, `"interface_id":"eth1"`) || !strings.Contains(body, `"system_name":"ens224"`) {
		t.Fatalf("WAN link did not store the product id: %s", body)
	}

	readback := authenticatedJSONRequest(t, server, http.MethodGet, "/api/v1/gateway/wan-links/uiacc-wan-eth", "", cookie)
	if !strings.Contains(readback.Body.String(), `"interface_id":"eth1"`) || !strings.Contains(readback.Body.String(), `"system_name":"ens224"`) {
		t.Fatalf("WAN readback = %s", readback.Body.String())
	}

	// Re-binding by the product id must move the line to the other port.
	rebound := authenticatedJSONRequest(t, server, http.MethodPatch, "/api/v1/gateway/wan-links/uiacc-wan-eth",
		`{"id":"uiacc-wan-eth","interface_id":"eth2","wan_type":"dhcp4","type":"dhcp4","ipv4":{"mode":"dhcp4"},"ipv6":{"mode":"disabled"}}`, cookie)
	if rebound.Code != http.StatusOK || !strings.Contains(rebound.Body.String(), `"interface_id":"eth2"`) || !strings.Contains(rebound.Body.String(), `"system_name":"ens256"`) {
		t.Fatalf("WAN rebind by product id = %d %s", rebound.Code, rebound.Body.String())
	}
}

func TestDHCPAndPortMapAcceptAnyPortSpelling(t *testing.T) {
	installIdentityFixture(t)
	server, cookie := inventoryTestServer(t)

	dhcp := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/dhcp/servers",
		`{"id":"uiacc-dhcp","name":"uiacc","interface_id":"00:50:56:3a:14:02","subnet":"192.0.2.0/24","pools":["192.0.2.10-192.0.2.99"]}`, cookie)
	if dhcp.Code != http.StatusOK || !strings.Contains(dhcp.Body.String(), `"interface_id":"eth1"`) {
		t.Fatalf("DHCP by MAC = %d %s", dhcp.Code, dhcp.Body.String())
	}

	unknown := authenticatedJSONRequest(t, server, http.MethodPost, "/api/v1/dhcp/servers",
		`{"id":"uiacc-dhcp-bad","name":"uiacc","interface_id":"eth9","subnet":"192.0.2.0/24","pools":["192.0.2.10-192.0.2.99"]}`, cookie)
	if unknown.Code != http.StatusUnprocessableEntity || !strings.Contains(unknown.Body.String(), "is not a port on this device") {
		t.Fatalf("DHCP unknown port = %d %s", unknown.Code, unknown.Body.String())
	}
}

// A stored document that still uses the kernel name as its key must be served
// under the product id and must not be duplicated in the list.
func TestLegacyKernelKeyedInterfaceDocumentIsServedOnce(t *testing.T) {
	installIdentityFixture(t)
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-planb-legacy?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithClock(fixedClock()))

	legacy := map[string]any{"id": "ens256", "interface_id": "ens256", "system_name": "ens256", "name": "ens256",
		"display_name": "LAN-A", "gateway_role": "lan", "cidr": "192.0.2.1/24"}
	raw, hash, err := persistence.MarshalPayload(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, persistence.ConfigDocument{ResourceType: "interface", ResourceID: "ens256", Payload: raw, PayloadHash: hash, UpdatedAt: fixedClock()()}); err != nil {
		t.Fatal(err)
	}

	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range items {
		if stringField(item, "system_name") == "ens256" || stringField(item, "id") == "eth2" {
			count++
			if stringField(item, "id") != "eth2" || stringField(item, "interface_id") != "eth2" || stringField(item, "system_name") != "ens256" {
				t.Fatalf("legacy document not canonicalized: %#v", item)
			}
		}
	}
	if count != 1 {
		t.Fatalf("legacy port listed %d times, want 1: %#v", count, items)
	}
}

// A telemetry item arrives keyed by the kernel netdev; the desired document is
// keyed by the product id. Both must collapse into one item, otherwise the
// interface list shows every port twice and the role column is empty.
func TestDesiredMetadataMatchesTelemetryByKernelName(t *testing.T) {
	installIdentityFixture(t)
	ctx := context.Background()
	store, err := persistence.Open(ctx, "file:httpapi-planb-overlay?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := New(WithStore(store), WithClock(fixedClock()))

	doc := map[string]any{"id": "eth2", "interface_id": "eth2", "system_name": "ens256", "name": "eth2",
		"display_name": "LAN-A", "gateway_role": "lan", "cidr": "192.0.2.1/24"}
	raw, hash, err := persistence.MarshalPayload(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, persistence.ConfigDocument{ResourceType: "interface", ResourceID: "eth2", Payload: raw, PayloadHash: hash, UpdatedAt: fixedClock()()}); err != nil {
		t.Fatal(err)
	}

	runtimeItems := []map[string]any{
		{"id": "ens256", "name": "ens256", "link_state": "up"},
		{"id": "ens224", "name": "ens224", "link_state": "up"},
	}
	merged := server.overlayInterfaceDesiredMetadata(ctx, runtimeItems)
	if len(merged) != 2 {
		t.Fatalf("merged has %d items, want 2 (duplicate port listed?): %#v", len(merged), merged)
	}
	for _, item := range merged {
		if stringField(item, "name") == "ens256" {
			if stringField(item, "gateway_role") != "lan" || stringField(item, "display_name") != "LAN-A" {
				t.Fatalf("desired metadata not applied to the telemetry row: %#v", item)
			}
		}
	}
}
