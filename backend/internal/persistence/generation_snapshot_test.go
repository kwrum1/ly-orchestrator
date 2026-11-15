package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// Config snapshots share the runtime_snapshots table. Reading them back as the
// committed generation degraded /runtime/status after a manual snapshot, so the
// generation query must ignore every non-runtime id.
func TestGenerationSnapshotsIgnoreConfigSnapshots(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "file:generation-snapshots?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	now := time.Now().UTC()
	payload := []byte(`{"schema_version":1}`)
	hash := hashPayloadForTest(payload)
	for id, at := range map[string]time.Time{
		"snapshot-runtime-abc": now.Add(-3 * time.Minute),
		"snapshot-txn-def":     now.Add(-2 * time.Minute),
		"manual-1":             now.Add(-1 * time.Minute),
		"pre-import-2":         now,
	} {
		if err := store.SaveRuntimeSnapshot(ctx, RuntimeSnapshot{
			ID:                  id,
			SourceTransactionID: id,
			Payload:             payload,
			PayloadHash:         hash,
			CreatedAt:           at,
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	all, err := store.RuntimeSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("RuntimeSnapshots = %d rows, want 4", len(all))
	}

	generations, err := store.GenerationSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 {
		t.Fatalf("GenerationSnapshots = %d rows, want 2: %+v", len(generations), generations)
	}
	if generations[0].ID != "snapshot-txn-def" {
		t.Fatalf("newest generation = %q, want snapshot-txn-def", generations[0].ID)
	}
	for _, snapshot := range generations {
		if snapshot.ID == "manual-1" || snapshot.ID == "pre-import-2" {
			t.Fatalf("config snapshot %q leaked into the generation evidence", snapshot.ID)
		}
	}

	limited, err := store.GenerationSnapshotsLimit(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].ID != "snapshot-txn-def" {
		t.Fatalf("GenerationSnapshotsLimit(1) = %+v", limited)
	}
}

func hashPayloadForTest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
