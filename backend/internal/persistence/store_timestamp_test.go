package persistence

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A foreign or corrupted row used to panic inside scanConfig (and could take
// the whole control plane down through the dataplane watcher). Listing must
// skip the bad row with a warning; a direct read must return a clear error.
func TestStoreSkipsConfigRowsWithInvalidTimestamp(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "file:lyroute-bad-timestamp?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	payload, hash, err := MarshalPayload(map[string]any{"id": "good"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, ConfigDocument{
		ResourceType: "proxy_egress", ResourceID: "good",
		Payload: payload, PayloadHash: hash, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// SQLite's datetime() format, not RFC3339: an external writer can leave
	// this in the database.
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO config_documents (resource_type, resource_id, payload_json, payload_hash, updated_at)
VALUES (?, ?, ?, ?, ?)`, "proxy_egress", "bad", `{"id":"bad"}`, hash, "2026-10-31 17:23:21"); err != nil {
		t.Fatal(err)
	}

	items, err := store.Configs(ctx, "proxy_egress")
	if err != nil {
		t.Fatalf("Configs returned %v, want the valid rows and no error", err)
	}
	if len(items) != 1 || items[0].ResourceID != "good" {
		t.Fatalf("Configs = %#v, want only the valid document", items)
	}

	if _, err := store.Config(ctx, "proxy_egress", "bad"); err == nil || !strings.Contains(err.Error(), "invalid persisted timestamp") {
		t.Fatalf("Config on a bad timestamp = %v, want a clear decode error", err)
	}
}
