package applystate

import (
	"path/filepath"
	"testing"
	"time"
)

func TestIdentityRoundTripAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "apply-identity.json")
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	identity := Identity{
		TransactionID:     "runtime-caller",
		DesiredGeneration: 6,
		DesiredHash:       "hash-six",
		CreatedAt:         now,
	}
	if err := WriteIdentity(path, identity); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadIdentity(path, now.Add(time.Minute), DefaultIdentityTTL)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TransactionID != "runtime-caller" || loaded.DesiredGeneration != 6 || loaded.DesiredHash != "hash-six" {
		t.Fatalf("identity = %#v", loaded)
	}
	if _, err := ReadIdentity(path, now.Add(DefaultIdentityTTL+time.Second), DefaultIdentityTTL); err == nil {
		t.Fatal("expired identity was accepted")
	}
	if err := RemoveIdentity(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIdentity(path, now, DefaultIdentityTTL); err == nil {
		t.Fatal("removed identity was readable")
	}
}
