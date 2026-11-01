package applystate

import (
	"testing"
	"time"
)

// A committed generation stays committed after its readback leaves the
// freshness window; only the receipt's own validity decides completeness.
func TestReceiptStatusViewKeepsCommittedAfterReadbackWindow(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	readbackAt := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	state, err := store.CommitWithReadback("txn-1", appliedReceipt("txn-1", 1, "hash-one"), readbackAt)
	if err != nil {
		t.Fatal(err)
	}
	window := 5 * time.Minute

	if got := state.ReceiptStatusView(readbackAt.Add(time.Minute), window); got != ReceiptStatusCommitted {
		t.Fatalf("fresh view = %q, want %q", got, ReceiptStatusCommitted)
	}
	if got := state.ReceiptStatusView(readbackAt.Add(window+time.Minute), window); got != ReceiptStatusReadbackStale {
		t.Fatalf("just-stale view = %q, want %q", got, ReceiptStatusReadbackStale)
	}
	if got := state.ReceiptStatusView(readbackAt.Add(72*time.Hour), window); got != ReceiptStatusReadbackStale {
		t.Fatalf("long-stale view = %q, want %q (never incomplete)", got, ReceiptStatusReadbackStale)
	}

	partial := state
	partial.ReceiptStatus = ReceiptStatusPartial
	partial.Partial = true
	if got := partial.ReceiptStatusView(readbackAt, window); got != ReceiptStatusIncomplete {
		t.Fatalf("partial view = %q, want %q", got, ReceiptStatusIncomplete)
	}

	failed := state
	failed.ReceiptStatus = ReceiptStatusFailed
	if got := failed.ReceiptStatusView(readbackAt, window); got != ReceiptStatusIncomplete {
		t.Fatalf("failed view = %q, want %q", got, ReceiptStatusIncomplete)
	}

	missing := state
	missing.ReceiptStatus = ""
	if got := missing.ReceiptStatusView(readbackAt, window); got != ReceiptStatusIncomplete {
		t.Fatalf("missing view = %q, want %q", got, ReceiptStatusIncomplete)
	}
}

// A no-op apply re-reads the live configuration, so its readback is fresh even
// though the committed receipt (and the generation) are old.
func TestMarkUnchangedRefreshesReadbackTimestamp(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	commitAt := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	if _, err := store.CommitWithReadback("txn-1", appliedReceipt("txn-1", 1, "hash-one"), commitAt); err != nil {
		t.Fatal(err)
	}
	// Move the clock past the freshness window, then serve a no-op.
	store.SetClock(func() time.Time { return commitAt.Add(6 * time.Hour) })
	state, err := store.MarkUnchanged("txn-2")
	if err != nil {
		t.Fatal(err)
	}
	if !state.ReadbackAt.Equal(commitAt.Add(6 * time.Hour)) {
		t.Fatalf("readback_at = %s, want the no-op verification time", state.ReadbackAt)
	}
	if got := state.ReceiptStatusView(commitAt.Add(6*time.Hour), 5*time.Minute); got != ReceiptStatusCommitted {
		t.Fatalf("no-op view = %q, want %q", got, ReceiptStatusCommitted)
	}
}

// A generation committed before readback timestamps were persisted is still
// committed: it was validated when it committed.
func TestReceiptStatusViewLegacyStateHasNoReadbackTimestamp(t *testing.T) {
	state := State{Status: StatusApplied, ReceiptStatus: ReceiptStatusApplied}
	if got := state.ReceiptStatusView(time.Now().UTC(), 5*time.Minute); got != ReceiptStatusCommitted {
		t.Fatalf("legacy view = %q, want %q", got, ReceiptStatusCommitted)
	}
}
