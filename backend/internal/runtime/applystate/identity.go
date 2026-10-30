package applystate

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	IdentitySchemaVersion = 1
	// DefaultIdentityTTL bounds how long an out-of-process helper replay may
	// borrow the caller identity. The control plane removes the file when the
	// transaction settles; the TTL only protects against a crash or a very late
	// boot replay.
	DefaultIdentityTTL = 30 * time.Minute
)

// Identity is the caller transaction identity published for out-of-process
// helpers (for example a systemd-triggered vpp-apply replay during VPP
// restart). Environment variables still win; this file lets a helper that the
// control plane did not spawn directly echo the same transaction.
type Identity struct {
	SchemaVersion     int       `json:"schema_version"`
	TransactionID     string    `json:"transaction_id"`
	DesiredGeneration int64     `json:"desired_generation"`
	DesiredHash       string    `json:"desired_hash,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	CreatedAtEpoch    int64     `json:"created_at_epoch"`
}

func WriteIdentity(path string, identity Identity) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("apply identity: path is empty")
	}
	if strings.TrimSpace(identity.TransactionID) == "" {
		return fmt.Errorf("apply identity: transaction_id is empty")
	}
	identity.SchemaVersion = IdentitySchemaVersion
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = time.Now().UTC()
	}
	identity.CreatedAtEpoch = identity.CreatedAt.Unix()
	payload, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeAtomic(path, payload)
}

func ReadIdentity(path string, now time.Time, ttl time.Duration) (Identity, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Identity{}, fmt.Errorf("apply identity: path is empty")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	if err := json.Unmarshal(payload, &identity); err != nil {
		return Identity{}, fmt.Errorf("apply identity: decode %s: %w", path, err)
	}
	if identity.SchemaVersion != IdentitySchemaVersion {
		return Identity{}, fmt.Errorf("apply identity: unsupported schema_version %d", identity.SchemaVersion)
	}
	if strings.TrimSpace(identity.TransactionID) == "" {
		return Identity{}, fmt.Errorf("apply identity: transaction_id is empty")
	}
	if ttl <= 0 {
		ttl = DefaultIdentityTTL
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	created := identity.CreatedAt
	if created.IsZero() && identity.CreatedAtEpoch > 0 {
		created = time.Unix(identity.CreatedAtEpoch, 0).UTC()
	}
	if created.IsZero() {
		return Identity{}, fmt.Errorf("apply identity: created_at is empty")
	}
	if now.Sub(created) > ttl {
		return Identity{}, fmt.Errorf("apply identity: transaction %s expired after %s", identity.TransactionID, ttl)
	}
	return identity, nil
}

func RemoveIdentity(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
