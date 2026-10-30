package applystate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	ErrReceiptMissing    = errors.New("apply receipt is missing")
	ErrReceiptInvalid    = errors.New("apply receipt is invalid")
	ErrReceiptIncomplete = errors.New("apply receipt is incomplete")
	ErrReceiptFailed     = errors.New("apply receipt reports a failed transaction")
)

// Compensation is the per-step undo record. It is deliberately data, not an
// implicit "run this next time" instruction: the control-plane transaction owns
// rollback, while vpp-apply records whether an inverse exists and whether it
// was executed.
type Compensation struct {
	Status  string `json:"status"`
	Command string `json:"command,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Step is one executed (or skipped) command/action in a transaction receipt.
type Step struct {
	Command      string        `json:"command"`
	Status       string        `json:"status"`
	Attempts     int           `json:"attempts"`
	Compensation *Compensation `json:"compensation,omitempty"`
	Reason       string        `json:"reason,omitempty"`
}

// Receipt is the transactional contract between the executor (vpp-apply or the
// typed control-plane transaction) and the generation store.
type Receipt struct {
	SchemaVersion int    `json:"schema_version"`
	TransactionID string `json:"transaction_id"`
	// ExecutorTransactionID preserves a nested helper identity (for example
	// vpp-apply-<ts>-<pid>) without letting it replace the caller transaction.
	ExecutorTransactionID string   `json:"executor_transaction_id,omitempty"`
	Status                string   `json:"status"`
	DesiredGeneration     int64    `json:"desired_generation"`
	AppliedGeneration     int64    `json:"applied_generation"`
	DesiredHash           string   `json:"desired_hash"`
	Partial               bool     `json:"partial"`
	DegradedReasons       []string `json:"degraded_reasons,omitempty"`
	// SelfHealed carries the bounded, product-owned repairs performed before
	// this generation converged. It is evidence, never a substitute for the
	// applied-generation contract.
	SelfHealed []string `json:"self_healed,omitempty"`
	Steps      []Step   `json:"steps"`
	Error      string   `json:"error,omitempty"`
	// ProcessAlive is informational only. A live process with a failed
	// receipt is still a failed apply; nobody may infer success from it.
	ProcessAlive bool `json:"process_alive,omitempty"`
}

// Expectation is the generation the caller is trying to commit.
type Expectation struct {
	TransactionID     string
	DesiredGeneration int64
	DesiredHash       string
}

// Validate enforces the "data plane converged" contract:
//   - the receipt exists and is parseable (checked by LoadReceipt),
//   - it belongs to this transaction,
//   - it was produced for the current desired generation and hash,
//   - its applied generation equals the desired generation,
//   - it has an explicit per-step compensation record.
//
// A status other than applied (including a live-process failed receipt) never
// advances the applied generation.
func (receipt Receipt) Validate(expected Expectation) error {
	if receipt.SchemaVersion != 0 && receipt.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrReceiptIncomplete, receipt.SchemaVersion)
	}
	if strings.TrimSpace(receipt.TransactionID) == "" {
		return fmt.Errorf("%w: transaction_id is empty", ErrReceiptIncomplete)
	}
	if expected.TransactionID != "" && receipt.TransactionID != expected.TransactionID {
		return fmt.Errorf("%w: transaction_id %q does not belong to %q", ErrReceiptIncomplete, receipt.TransactionID, expected.TransactionID)
	}
	if strings.TrimSpace(receipt.Status) != StatusApplied {
		return fmt.Errorf("%w: status=%q", ErrReceiptFailed, receipt.Status)
	}
	if expected.DesiredGeneration <= 0 {
		return fmt.Errorf("%w: expected desired generation is not positive", ErrReceiptIncomplete)
	}
	if receipt.DesiredGeneration != expected.DesiredGeneration {
		return fmt.Errorf("%w: desired_generation receipt=%d state=%d", ErrGenerationMismatch, receipt.DesiredGeneration, expected.DesiredGeneration)
	}
	if receipt.AppliedGeneration != receipt.DesiredGeneration {
		return fmt.Errorf("%w: applied_generation=%d desired_generation=%d; data plane did not converge",
			ErrGenerationMismatch, receipt.AppliedGeneration, receipt.DesiredGeneration)
	}
	expectedHash := strings.TrimSpace(expected.DesiredHash)
	if expectedHash == "" || strings.TrimSpace(receipt.DesiredHash) != expectedHash {
		return fmt.Errorf("%w: desired_hash receipt=%q state=%q", ErrGenerationMismatch, receipt.DesiredHash, expectedHash)
	}
	if len(receipt.Steps) == 0 {
		return fmt.Errorf("%w: steps[] is empty", ErrReceiptIncomplete)
	}
	degraded := false
	for index, step := range receipt.Steps {
		command := strings.TrimSpace(step.Command)
		if command == "" {
			return fmt.Errorf("%w: step %d has no command", ErrReceiptIncomplete, index)
		}
		if strings.TrimSpace(step.Status) == "" {
			return fmt.Errorf("%w: step %d (%s) has no status", ErrReceiptIncomplete, index, command)
		}
		if step.Attempts < 1 {
			return fmt.Errorf("%w: step %d (%s) has invalid attempts %d", ErrReceiptIncomplete, index, command, step.Attempts)
		}
		if step.Compensation == nil || strings.TrimSpace(step.Compensation.Status) == "" {
			return fmt.Errorf("%w: step %d (%s) has no compensation record", ErrReceiptIncomplete, index, command)
		}
		switch step.Status {
		case "failed":
			return fmt.Errorf("%w: step %d (%s) failed", ErrReceiptFailed, index, command)
		case "ignored-failure", "needs-repair":
			degraded = true
		}
	}
	if degraded {
		if !receipt.Partial {
			return fmt.Errorf("%w: degraded steps present but partial=false", ErrReceiptIncomplete)
		}
		if len(receipt.DegradedReasons) == 0 {
			return fmt.Errorf("%w: degraded steps present without degraded_reasons[]", ErrReceiptIncomplete)
		}
	}
	if receipt.Partial && len(receipt.DegradedReasons) == 0 {
		return fmt.Errorf("%w: partial=true without degraded_reasons[]", ErrReceiptIncomplete)
	}
	return nil
}

// LoadReceipt reads a receipt from disk. Missing and malformed receipts are
// typed errors so callers can record "missing"/"invalid" instead of silently
// treating the apply as successful.
func LoadReceipt(path string) (Receipt, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Receipt{}, ErrReceiptMissing
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Receipt{}, fmt.Errorf("%w: %s", ErrReceiptMissing, path)
		}
		return Receipt{}, fmt.Errorf("%w: read %s: %v", ErrReceiptMissing, path, err)
	}
	var receipt Receipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("%w: decode %s: %v", ErrReceiptInvalid, path, err)
	}
	return receipt, nil
}

// WriteReceipt atomically publishes a receipt (used by tests and by the
// control plane's transaction recorder).
func WriteReceipt(path string, receipt Receipt) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("apply receipt: path is empty")
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeAtomic(path, payload)
}
