// Package applystate persists the desired/applied configuration generation of
// the control plane.
//
// The state file is the audit authority for "was this generation actually
// applied?". It is deliberately independent from a process-liveness check: a
// running VPP with a failed apply receipt is a failed generation, not a healthy
// one.
package applystate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SchemaVersion versions the on-disk document. A reader that does not
	// understand the version must refuse to treat it as authoritative.
	SchemaVersion = 1

	StatusPending   = "pending"
	StatusApplied   = "applied"
	StatusPartial   = "partial"
	StatusUnchanged = "unchanged"
	StatusFailed    = "failed"

	ReceiptStatusMissing   = "missing"
	ReceiptStatusInvalid   = "invalid"
	ReceiptStatusFailed    = "failed"
	ReceiptStatusApplied   = "applied"
	ReceiptStatusPartial   = "partial"
	ReceiptStatusUnchanged = "unchanged"
)

var (
	ErrStateDisabled      = errors.New("apply state: store is not configured")
	ErrStateInvalid       = errors.New("apply state: persisted document is invalid")
	ErrGenerationMismatch = errors.New("apply state: generation mismatch")
)

// ErrorRecord is the operator-facing reason the current desired generation is
// not the applied one. It always names the transaction that failed, so a stale
// error can never be attributed to a later apply.
type ErrorRecord struct {
	TransactionID string    `json:"transaction_id"`
	Reason        string    `json:"reason"`
	ReceiptStatus string    `json:"receipt_status,omitempty"`
	At            time.Time `json:"at"`
}

// State is the durable apply-generation document.
//
// DesiredGeneration advances whenever the canonical desired hash changes.
// AppliedGeneration only advances when a validated receipt proves that the
// dataplane converged for that generation. CurrentGeneration mirrors the last
// applied generation; a failed apply never advances it.
type State struct {
	SchemaVersion      int          `json:"schema_version"`
	DesiredGeneration  int64        `json:"desired_generation"`
	DesiredHash        string       `json:"desired_hash,omitempty"`
	AppliedGeneration  int64        `json:"applied_generation"`
	AppliedDesiredHash string       `json:"applied_desired_hash,omitempty"`
	CurrentGeneration  int64        `json:"current_generation"`
	CurrentHash        string       `json:"current_hash,omitempty"`
	TransactionID      string       `json:"transaction_id,omitempty"`
	Status             string       `json:"status"`
	Partial            bool         `json:"partial"`
	DegradedReasons    []string     `json:"degraded_reasons,omitempty"`
	ReceiptStatus      string       `json:"receipt_status,omitempty"`
	LastError          *ErrorRecord `json:"last_error,omitempty"`
	// SelfHealed records the bounded, product-owned dataplane scopes this
	// generation had to rebuild before it converged (for example "nftables" or
	// "abf"). It is cleared for a new desired generation and for a failed one.
	SelfHealed []string  `json:"self_healed,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Snapshot is a read-only copy returned to the API. Slices are cloned so a
// caller cannot mutate the cached document.
func (state State) Clone() State {
	clone := state
	clone.DegradedReasons = append([]string(nil), state.DegradedReasons...)
	clone.SelfHealed = append([]string(nil), state.SelfHealed...)
	if state.LastError != nil {
		record := *state.LastError
		clone.LastError = &record
	}
	return clone
}

// Store serialises every read-modify-write of one state document.
type Store struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewStore returns a disabled store for an empty path. Callers that have no
// state path must keep the legacy behaviour instead of silently reporting a
// successful generation.
func NewStore(path string) *Store {
	return &Store{path: strings.TrimSpace(path), now: func() time.Time { return time.Now().UTC() }}
}

func (store *Store) Enabled() bool { return store != nil && store.path != "" }

func (store *Store) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

// SetClock is used by tests; production always uses UTC wall clock.
func (store *Store) SetClock(now func() time.Time) {
	if store == nil || now == nil {
		return
	}
	store.now = func() time.Time { return now().UTC() }
}

func (store *Store) timestamp() time.Time {
	if store == nil || store.now == nil {
		return time.Now().UTC()
	}
	return store.now().UTC()
}

// Snapshot reads the current document. A missing file is an empty generation,
// not an error; an unreadable or malformed file is an error so the control
// plane never invents a healthy generation from corruption.
func (store *Store) Snapshot() (State, error) {
	if !store.Enabled() {
		return State{}, ErrStateDisabled
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockFile()
	if err != nil {
		return State{}, err
	}
	defer unlock()
	return store.loadLocked()
}

func (store *Store) loadLocked() (State, error) {
	payload, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{SchemaVersion: SchemaVersion, Status: ""}, nil
		}
		return State{}, fmt.Errorf("apply state: read %s: %w", store.path, err)
	}
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		return State{}, fmt.Errorf("%w: decode %s: %v", ErrStateInvalid, store.path, err)
	}
	if state.SchemaVersion != 0 && state.SchemaVersion != SchemaVersion {
		return State{}, fmt.Errorf("%w: unsupported schema_version %d", ErrStateInvalid, state.SchemaVersion)
	}
	if state.DesiredGeneration < 0 || state.AppliedGeneration < 0 || state.CurrentGeneration < 0 {
		return State{}, fmt.Errorf("%w: negative generation", ErrStateInvalid)
	}
	if state.DesiredGeneration > 0 && state.DesiredGeneration < state.AppliedGeneration {
		return State{}, fmt.Errorf("%w: applied %d is ahead of desired %d", ErrStateInvalid, state.AppliedGeneration, state.DesiredGeneration)
	}
	state.SchemaVersion = SchemaVersion
	if state.CurrentGeneration == 0 {
		state.CurrentGeneration = state.AppliedGeneration
		state.CurrentHash = state.AppliedDesiredHash
	}
	return state.Clone(), nil
}

func (store *Store) saveLocked(state State) (State, error) {
	state.SchemaVersion = SchemaVersion
	state.UpdatedAt = store.timestamp()
	state.DegradedReasons = uniqueStrings(state.DegradedReasons)
	if state.LastError != nil && state.LastError.TransactionID == "" {
		return State{}, fmt.Errorf("apply state: refusing to persist an error without a transaction id")
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return State{}, fmt.Errorf("apply state: encode: %w", err)
	}
	payload = append(payload, '\n')
	if err := writeAtomic(store.path, payload); err != nil {
		return State{}, fmt.Errorf("apply state: write %s: %w", store.path, err)
	}
	return state.Clone(), nil
}

// BeginResult describes what the caller may do with this apply request.
type BeginResult struct {
	State State `json:"state"`
	// Unchanged is true only when the desired hash matches the applied hash,
	// the generations match, and the last validated receipt is usable.
	Unchanged bool `json:"unchanged"`
	// Reconverge is true when the generation number is unchanged but there is
	// no usable receipt. The caller must re-run convergence instead of
	// assuming the old generation is still live.
	Reconverge bool `json:"reconverge"`
}

// Begin advances the desired generation when the canonical desired hash
// changed and reports whether the request can be served as a no-op.
func (store *Store) Begin(desiredHash string) (BeginResult, error) {
	if !store.Enabled() {
		return BeginResult{}, ErrStateDisabled
	}
	desiredHash = strings.TrimSpace(desiredHash)
	if desiredHash == "" {
		return BeginResult{}, fmt.Errorf("apply state: desired hash is empty")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockFile()
	if err != nil {
		return BeginResult{}, err
	}
	defer unlock()

	state, err := store.loadLocked()
	if err != nil {
		return BeginResult{}, err
	}
	changed := state.DesiredHash != desiredHash
	if changed {
		state.DesiredGeneration++
		state.DesiredHash = desiredHash
		state.Status = StatusPending
		state.SelfHealed = nil
	} else if state.DesiredGeneration == 0 {
		// A state file written before generations existed still has a desired
		// hash. The first transactional apply starts at generation 1.
		state.DesiredGeneration = 1
		state.DesiredHash = desiredHash
		state.Status = StatusPending
	}
	if state.CurrentGeneration == 0 {
		state.CurrentGeneration = state.AppliedGeneration
		state.CurrentHash = state.AppliedDesiredHash
	}
	usableReceipt := state.ReceiptStatus == ReceiptStatusApplied ||
		state.ReceiptStatus == ReceiptStatusPartial ||
		state.ReceiptStatus == ReceiptStatusUnchanged
	unchanged := !changed &&
		state.AppliedGeneration > 0 &&
		state.AppliedGeneration == state.DesiredGeneration &&
		state.AppliedDesiredHash == desiredHash &&
		usableReceipt
	reconverge := !unchanged &&
		state.AppliedGeneration == state.DesiredGeneration &&
		state.AppliedDesiredHash == desiredHash
	saved, err := store.saveLocked(state)
	if err != nil {
		return BeginResult{}, err
	}
	return BeginResult{State: saved, Unchanged: unchanged, Reconverge: reconverge}, nil
}

// Commit records a validated applied receipt. A receipt whose generations do
// not match the current desired generation is rejected: a stale transaction
// can never advance the applied generation.
func (store *Store) Commit(transactionID string, receipt Receipt) (State, error) {
	if !store.Enabled() {
		return State{}, ErrStateDisabled
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockFile()
	if err != nil {
		return State{}, err
	}
	defer unlock()

	state, err := store.loadLocked()
	if err != nil {
		return State{}, err
	}
	if state.DesiredGeneration == 0 {
		return State{}, fmt.Errorf("%w: apply state has no desired generation; call Begin first", ErrGenerationMismatch)
	}
	if err := receipt.Validate(Expectation{
		TransactionID:     transactionID,
		DesiredGeneration: state.DesiredGeneration,
		DesiredHash:       state.DesiredHash,
	}); err != nil {
		return State{}, err
	}
	state.AppliedGeneration = receipt.AppliedGeneration
	state.AppliedDesiredHash = receipt.DesiredHash
	state.CurrentGeneration = receipt.AppliedGeneration
	state.CurrentHash = receipt.DesiredHash
	state.TransactionID = transactionID
	state.Partial = receipt.Partial
	state.DegradedReasons = append([]string(nil), receipt.DegradedReasons...)
	state.SelfHealed = uniqueStrings(receipt.SelfHealed)
	state.Status = StatusApplied
	state.ReceiptStatus = ReceiptStatusApplied
	if receipt.Partial {
		state.Status = StatusPartial
		state.ReceiptStatus = ReceiptStatusPartial
	}
	state.LastError = nil
	return store.saveLocked(state)
}

// MarkUnchanged records a no-op apply whose live state was re-verified as
// converged. It never advances either generation and clears any inherited
// partial flag: a no-op is not the previous generation's degradation.
func (store *Store) MarkUnchanged(transactionID string) (State, error) {
	return store.MarkUnchangedEvaluated(transactionID, false, nil)
}

// MarkUnchangedEvaluated records a no-op apply together with the evaluation of
// the current live components and receipt. partial is only honoured when at
// least one concrete degraded reason is supplied; a bare flag is exactly the
// placeholder this contract forbids.
func (store *Store) MarkUnchangedEvaluated(transactionID string, partial bool, degradedReasons []string) (State, error) {
	if !store.Enabled() {
		return State{}, ErrStateDisabled
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockFile()
	if err != nil {
		return State{}, err
	}
	defer unlock()

	state, err := store.loadLocked()
	if err != nil {
		return State{}, err
	}
	if state.AppliedGeneration == 0 || state.AppliedGeneration != state.DesiredGeneration || state.AppliedDesiredHash != state.DesiredHash {
		return State{}, fmt.Errorf("%w: cannot report unchanged desired=%d applied=%d", ErrGenerationMismatch, state.DesiredGeneration, state.AppliedGeneration)
	}
	reasons := uniqueStrings(degradedReasons)
	if !partial || len(reasons) == 0 {
		partial = false
		reasons = nil
	}
	state.Status = StatusUnchanged
	state.ReceiptStatus = ReceiptStatusUnchanged
	state.TransactionID = transactionID
	state.CurrentGeneration = state.AppliedGeneration
	state.CurrentHash = state.AppliedDesiredHash
	state.Partial = partial
	state.DegradedReasons = reasons
	state.LastError = nil
	return store.saveLocked(state)
}

// Failure is the audit record of a rejected or failed apply.
type Failure struct {
	TransactionID string
	Reason        string
	RecordStatus  string
	// Partial means some best-effort steps were applied but the transaction
	// still converged. A partial transaction does not use Failure; this field
	// exists for the rare case where a rollback reports mixed results.
	Partial bool
}

// Fail records a failed apply without advancing the applied generation.
func (store *Store) Fail(failure Failure) (State, error) {
	if !store.Enabled() {
		return State{}, ErrStateDisabled
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockFile()
	if err != nil {
		return State{}, err
	}
	defer unlock()

	state, err := store.loadLocked()
	if err != nil {
		return State{}, err
	}
	recordStatus := strings.TrimSpace(failure.RecordStatus)
	if recordStatus == "" {
		recordStatus = ReceiptStatusFailed
	}
	reason := strings.TrimSpace(failure.Reason)
	if reason == "" {
		reason = "apply failed"
	}
	if transactionID := strings.TrimSpace(failure.TransactionID); transactionID != "" && !strings.Contains(reason, transactionID) {
		reason = "transaction " + transactionID + ": " + reason
	}
	state.Status = StatusFailed
	state.Partial = failure.Partial
	state.SelfHealed = nil
	state.TransactionID = failure.TransactionID
	state.CurrentGeneration = state.AppliedGeneration
	state.CurrentHash = state.AppliedDesiredHash
	state.ReceiptStatus = recordStatus
	state.LastError = &ErrorRecord{
		TransactionID: failure.TransactionID,
		Reason:        reason,
		ReceiptStatus: recordStatus,
		At:            store.timestamp(),
	}
	return store.saveLocked(state)
}

func writeAtomic(path string, payload []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// HashBytes is the canonical short hash used by tests and callers that need to
// derive a desired identity from a byte payload.
func HashBytes(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
