package applystate

import "testing"

// The apply state is the durable generation authority, so a successful
// self-heal must be visible there, not only in the HTTP response. A new desired
// generation or a failed apply must not inherit the previous heal record.
func TestCommitRecordsSelfHealedAndNewOrFailedGenerationClearsIt(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	receipt := appliedReceipt("txn-1", 1, "hash-one")
	receipt.SelfHealed = []string{"nftables", "abf", "nftables"}
	state, err := store.Commit("txn-1", receipt)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SelfHealed) != 2 || state.SelfHealed[0] != "abf" || state.SelfHealed[1] != "nftables" {
		t.Fatalf("self_healed = %#v, want sorted unique [abf nftables]", state.SelfHealed)
	}

	// A same-generation reconvergence that does not heal keeps the evidence of
	// the generation that actually converged.
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.MarkUnchanged("txn-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.SelfHealed) != 2 {
		t.Fatalf("unchanged lost self_healed: %#v", unchanged.SelfHealed)
	}

	// A new desired generation starts clean.
	if _, err := store.Begin("hash-two"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SelfHealed) != 0 {
		t.Fatalf("new generation inherited self_healed: %#v", loaded.SelfHealed)
	}
	if _, err := store.Fail(Failure{TransactionID: "txn-3", Reason: "boom"}); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SelfHealed) != 0 {
		t.Fatalf("failed apply kept self_healed: %#v", loaded.SelfHealed)
	}
}
