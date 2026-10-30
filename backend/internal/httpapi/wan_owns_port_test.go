package httpapi

import (
	"context"
	"strings"
	"testing"

	"ly-route/backend/internal/persistence"
)

// A port bound to an enabled WAN line must not also emit its LAN assignment:
// two assignments for the same VPP interface made the plan contain
// "lyroute-<port>" twice and the reconciliation preflight rejected the apply
// with 'interface identity ... is duplicated'.
func TestRuntimeAddressAssignmentsWANLineOwnsItsPort(t *testing.T) {
	server, _ := inventoryTestServer(t)
	ctx := withInterfaceIdentities(context.Background())

	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "interface",
		ResourceID:   "uiacc-labx",
		Payload:      []byte(`{"id":"uiacc-labx","interface_id":"uiacc-labx","name":"eth9","display_name":"uiacc-lan","gateway_role":"lan","cidr":"192.0.2.1/24","kind":"interface","type":"interface"}`),
		PayloadHash:  "fixture-interface",
	}); err != nil {
		t.Fatalf("seed interface: %v", err)
	}
	if err := server.store.SaveConfig(ctx, persistence.ConfigDocument{
		ResourceType: "wan_link",
		ResourceID:   "uiacc-wan-x",
		Payload:      []byte(`{"id":"uiacc-wan-x","name":"uiacc","interface_id":"uiacc-labx","enabled":true,"wan_type":"static","type":"static","cidr":"198.51.100.2/24","gateway":"198.51.100.1","ipv4":{"mode":"static","address":"198.51.100.2/24","gateway":"198.51.100.1"},"ipv6":{"mode":"disabled"}}`),
		PayloadHash:  "fixture-wan",
	}); err != nil {
		t.Fatalf("seed wan: %v", err)
	}

	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	count := 0
	for _, assignment := range assignments {
		if strings.EqualFold(strings.TrimSpace(assignment.LinuxInterface), "uiacc-labx") {
			count++
			if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
				t.Fatalf("port owned by a WAN line must be assigned as wan, got %q", assignment.Role)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one assignment for the WAN-bound port, got %d", count)
	}

	// Removing the line gives the LAN assignment back (the stored config was kept).
	if err := server.store.DeleteConfig(ctx, "wan_link", "uiacc-wan-x"); err != nil {
		t.Fatalf("delete wan: %v", err)
	}
	assignments, err = server.runtimeAddressAssignments(ctx)
	if err != nil {
		t.Fatalf("assignments after delete: %v", err)
	}
	for _, assignment := range assignments {
		if strings.EqualFold(strings.TrimSpace(assignment.LinuxInterface), "uiacc-labx") && strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			return
		}
	}
	t.Fatal("LAN assignment did not come back after the WAN line was removed")
}
