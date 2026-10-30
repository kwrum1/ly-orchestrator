package applystate

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "state", "apply-state.json"))
	store.SetClock(func() time.Time { return time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC) })
	return store
}

func appliedReceipt(transactionID string, generation int64, hash string) Receipt {
	return Receipt{
		SchemaVersion:     SchemaVersion,
		TransactionID:     transactionID,
		Status:            StatusApplied,
		DesiredGeneration: generation,
		AppliedGeneration: generation,
		DesiredHash:       hash,
		Steps: []Step{{
			Command:      "set interface state lan0 up",
			Status:       "applied",
			Attempts:     1,
			Compensation: &Compensation{Status: "not-required"},
		}},
	}
}

func TestFirstApplyGenerationAndRepeatIsUnchanged(t *testing.T) {
	store := testStore(t)
	first, err := store.Begin("hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if first.State.DesiredGeneration != 1 || first.Unchanged {
		t.Fatalf("first begin = %#v, want desired generation 1 and not unchanged", first)
	}
	state, err := store.Commit("txn-1", appliedReceipt("txn-1", 1, "hash-one"))
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.DesiredGeneration != 1 || state.Status != StatusApplied {
		t.Fatalf("after first commit = %#v", state)
	}
	repeat, err := store.Begin("hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.Unchanged || repeat.State.DesiredGeneration != 1 || repeat.State.AppliedGeneration != 1 {
		t.Fatalf("repeat begin = %#v, want unchanged generation 1", repeat)
	}
	state, err = store.MarkUnchanged("txn-2")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusUnchanged || state.AppliedGeneration != 1 || state.DesiredGeneration != 1 {
		t.Fatalf("unchanged state = %#v", state)
	}
}

func TestDesiredChangeAdvancesGeneration(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("txn-1", appliedReceipt("txn-1", 1, "hash-one")); err != nil {
		t.Fatal(err)
	}
	second, err := store.Begin("hash-two")
	if err != nil {
		t.Fatal(err)
	}
	if second.State.DesiredGeneration != 2 || second.Unchanged {
		t.Fatalf("changed begin = %#v, want desired 2", second)
	}
	if second.State.AppliedGeneration != 1 {
		t.Fatalf("applied generation advanced before commit: %#v", second.State)
	}
	state, err := store.Commit("txn-2", appliedReceipt("txn-2", 2, "hash-two"))
	if err != nil {
		t.Fatal(err)
	}
	if state.DesiredGeneration != 2 || state.AppliedGeneration != 2 || state.AppliedDesiredHash != "hash-two" {
		t.Fatalf("after second commit = %#v", state)
	}
}

func TestHardFailureKeepsAppliedGenerationAndNamesTransaction(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("txn-1", appliedReceipt("txn-1", 1, "hash-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin("hash-two"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Fail(Failure{
		TransactionID: "txn-hard-failure",
		Reason:        "vppctl command failed for operation interfaces: set interface state wan0 up",
		RecordStatus:  ReceiptStatusFailed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusFailed || state.AppliedGeneration != 1 || state.DesiredGeneration != 2 {
		t.Fatalf("failed state = %#v", state)
	}
	if state.CurrentGeneration != 1 || state.LastError == nil || state.LastError.TransactionID != "txn-hard-failure" {
		t.Fatalf("failure audit = %#v", state)
	}
	if state.LastError.Reason == "" || !contains(state.LastError.Reason, "txn-hard-failure") {
		t.Fatalf("failure reason does not identify transaction: %#v", state.LastError)
	}
}

func TestBestEffortFailureIsAppliedButPartialDegraded(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	receipt := Receipt{
		SchemaVersion:     SchemaVersion,
		TransactionID:     "txn-best-effort",
		Status:            StatusApplied,
		DesiredGeneration: 1,
		AppliedGeneration: 1,
		DesiredHash:       "hash-one",
		Partial:           true,
		DegradedReasons:   []string{"best-effort command failed: ip route add table 100 192.0.2.0/24"},
		Steps: []Step{
			{Command: "set interface state lan0 up", Status: "applied", Attempts: 1, Compensation: &Compensation{Status: "available", Command: "set interface state lan0 down"}},
			{Command: "ip route add table 100 192.0.2.0/24 via 10.0.0.1", Status: "ignored-failure", Attempts: 1, Compensation: &Compensation{Status: "not-required"}, Reason: "best-effort command failed"},
		},
	}
	state, err := store.Commit("txn-best-effort", receipt)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusPartial || !state.Partial || state.AppliedGeneration != 1 {
		t.Fatalf("partial state = %#v", state)
	}
	if len(state.DegradedReasons) != 1 {
		t.Fatalf("degraded reasons = %#v", state.DegradedReasons)
	}
}

func TestReceiptMissingNeverAdvancesGeneration(t *testing.T) {
	store := testStore(t)
	receiptPath := filepath.Join(t.TempDir(), "missing-receipt.json")
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReceipt(receiptPath); !errors.Is(err, ErrReceiptMissing) {
		t.Fatalf("LoadReceipt missing error = %v, want ErrReceiptMissing", err)
	}
	state, err := store.Fail(Failure{TransactionID: "txn-missing", Reason: "apply receipt is missing", RecordStatus: ReceiptStatusMissing})
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 0 || state.Status != StatusFailed || state.ReceiptStatus != ReceiptStatusMissing {
		t.Fatalf("missing receipt state = %#v", state)
	}
}

func TestCorruptReceiptNeverAdvancesGeneration(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	_, err := LoadReceipt(path)
	if !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("LoadReceipt error = %v, want ErrReceiptInvalid", err)
	}
	state, err := store.Fail(Failure{TransactionID: "txn-invalid", Reason: err.Error(), RecordStatus: ReceiptStatusInvalid})
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 0 || state.ReceiptStatus != ReceiptStatusInvalid {
		t.Fatalf("invalid receipt state = %#v", state)
	}
}

func TestProcessAliveDoesNotOverrideFailedReceipt(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	receipt := appliedReceipt("txn-alive", 1, "hash-one")
	receipt.Status = StatusFailed
	receipt.ProcessAlive = true
	receipt.Error = "vpp is running but the command failed"
	if err := receipt.Validate(Expectation{TransactionID: "txn-alive", DesiredGeneration: 1, DesiredHash: "hash-one"}); !errors.Is(err, ErrReceiptFailed) {
		t.Fatalf("validation error = %v, want ErrReceiptFailed", err)
	}
	if _, err := store.Commit("txn-alive", receipt); !errors.Is(err, ErrReceiptFailed) {
		t.Fatalf("commit error = %v, want ErrReceiptFailed", err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 0 || state.CurrentGeneration != 0 {
		t.Fatalf("live process advanced generation: %#v", state)
	}
}

func TestGenerationSameButReceiptMissingReconverges(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("txn-1", appliedReceipt("txn-1", 1, "hash-one")); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the desired document was written but before the
	// receipt was committed: same generation and hash, no usable receipt.
	if _, err := store.Fail(Failure{TransactionID: "txn-crash", Reason: "receipt lost", RecordStatus: ReceiptStatusMissing}); err != nil {
		t.Fatal(err)
	}
	begin, err := store.Begin("hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if begin.Unchanged {
		t.Fatalf("missing receipt reported unchanged: %#v", begin)
	}
	if !begin.Reconverge || begin.State.DesiredGeneration != 1 {
		t.Fatalf("begin = %#v, want reconverge at generation 1", begin)
	}
}

func TestStaleTransactionCannotCommit(t *testing.T) {
	store := testStore(t)
	if _, err := store.Begin("hash-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("txn-1", appliedReceipt("txn-1", 1, "hash-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin("hash-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("txn-stale", appliedReceipt("txn-stale", 1, "hash-one")); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale commit error = %v, want ErrGenerationMismatch", err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.DesiredGeneration != 2 {
		t.Fatalf("stale commit changed state: %#v", state)
	}
}

func TestConcurrentAppliesNeverInterleaveStateWrites(t *testing.T) {
	store := testStore(t)
	store.SetClock(time.Now)
	const workers = 16
	var group sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			begin, err := store.Begin("hash-one")
			if err != nil {
				errs <- err
				return
			}
			if begin.State.DesiredGeneration != 1 {
				errs <- errors.New("unexpected generation")
				return
			}
			receipt := appliedReceipt("txn-concurrent", 1, "hash-one")
			if _, err := store.Commit("txn-concurrent", receipt); err != nil {
				errs <- err
			}
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent apply: %v", err)
	}
	payload, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("state file was interleaved/corrupt: %v\n%s", err, payload)
	}
	if state.SchemaVersion != SchemaVersion || state.DesiredGeneration != 1 || state.AppliedGeneration != 1 {
		t.Fatalf("state = %#v", state)
	}
}

// TestFakeVPPApplyReceiptContract drives a fake vpp-apply script and commits
// its receipt. This is the offline stand-in for the shipped python adapter:
// the state store only trusts a parseable, generation-bound receipt.
func TestFakeVPPApplyReceiptContract(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-vpp-apply")
	body := `#!/bin/bash
set -eu
receipt="$1"; transaction="$2"; generation="$3"; hash="$4"
cat >"$receipt" <<EOF
{
  "schema_version": 1,
  "transaction_id": "$transaction",
  "status": "applied",
  "desired_generation": $generation,
  "applied_generation": $generation,
  "desired_hash": "$hash",
  "partial": false,
  "steps": [
    {"command": "set interface state lan0 up", "status": "applied", "attempts": 1,
     "compensation": {"status": "available", "command": "set interface state lan0 down"}}
  ]
}
EOF
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(dir, "receipt.json")
	if output, err := exec.Command("bash", script, receiptPath, "txn-script", "1", "hash-script").CombinedOutput(); err != nil {
		t.Fatalf("fake vpp-apply: %v\n%s", err, output)
	}
	store := testStore(t)
	if _, err := store.Begin("hash-script"); err != nil {
		t.Fatal(err)
	}
	receipt, err := LoadReceipt(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Commit("txn-script", receipt)
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != 1 || state.Status != StatusApplied {
		t.Fatalf("state = %#v", state)
	}
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
