package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A database created before the ledger existed must be recorded at its declared
// baseline and must not re-run anything.
func TestSchemaLedgerRecordsBaselineAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "baseline.db")
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.appliedMigrationVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("baseline ledger version = %d, want %d", version, SchemaVersion)
	}
	store.Close()

	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer reopened.Close()
	version, err = reopened.appliedMigrationVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("after reopen ledger version = %d, want %d", version, SchemaVersion)
	}
}

// A pending migration runs exactly once, in order, inside its own transaction.
func TestSchemaMigrationAppliesOnce(t *testing.T) {
	ctx := context.Background()
	original := schemaMigrations
	t.Cleanup(func() { schemaMigrations = original })
	schemaMigrations = []SchemaMigration{{
		Version:    SchemaVersion + 1,
		Name:       "test-create-marker",
		Statements: []string{`CREATE TABLE migration_marker (id INTEGER PRIMARY KEY)`},
	}}

	dsn := filepath.Join(t.TempDir(), "apply.db")
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.appliedMigrationVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion+1 {
		t.Fatalf("ledger version = %d, want %d", version, SchemaVersion+1)
	}
	if _, err := store.db.ExecContext(ctx, `SELECT COUNT(*) FROM migration_marker`); err != nil {
		t.Fatalf("migration did not create its table: %v", err)
	}
	store.Close()

	// The plain CREATE TABLE would fail on a second run: reopening proves the
	// ledger, not luck, keeps migrations idempotent.
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen ran the migration twice: %v", err)
	}
	reopened.Close()
}

// A database written by a newer build must be refused, not silently opened.
func TestSchemaNewerDatabaseIsRefused(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "newer.db")
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// product_metadata is immutable by contract, so the test has to drop the
	// guard to simulate a database left behind by a future release.
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS product_metadata_immutable_update`,
		`DROP TRIGGER IF EXISTS product_metadata_immutable_delete`,
		`UPDATE product_metadata SET schema_version = 99 WHERE singleton = 1`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	store.Close()

	if _, err := Open(ctx, dsn); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Open of a newer database = %v, want ErrSchemaNewer", err)
	}
}
