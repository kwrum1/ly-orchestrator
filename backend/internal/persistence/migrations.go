package persistence

// Schema migrations.
//
// product_metadata.schema_version is immutable by contract (its triggers forbid
// UPDATE), and it declares the *baseline* structure a database was created
// with. Everything after that baseline is tracked here, so a future column or
// table change becomes an ordered, replayable step instead of manual surgery.
//
// To change the schema:
//  1. append a SchemaMigration with the next version number and its statements;
//  2. write a test that opens a database at the previous version and asserts the
//     migration ran exactly once (see migration_test.go).

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSchemaNewer means the database was written by a newer build.
var ErrSchemaNewer = errors.New("persistence schema is newer than this build")

// SchemaMigration is one ordered, idempotent structural change.
type SchemaMigration struct {
	Version    int
	Name       string
	Statements []string
}

// schemaMigrations is applied in order after the baseline schema exists.
var schemaMigrations = []SchemaMigration{
	// (next migration goes here)
}

const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
);`

func (s *Store) ensureSchemaMigrations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("initialize schema migration ledger: %w", err)
	}
	applied, err := s.appliedMigrationVersion(ctx)
	if err != nil {
		return err
	}
	if applied == 0 {
		// A database created before the ledger existed already carries the
		// baseline schema declared in product_metadata.
		baseline, err := s.declaredSchemaVersion(ctx)
		if err != nil {
			return err
		}
		if baseline <= 0 {
			baseline = SchemaVersion
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations (version, name, applied_at) VALUES (?, 'baseline', ?)`,
			baseline, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record baseline schema version: %w", err)
		}
		applied = baseline
	}
	for _, migration := range schemaMigrations {
		if migration.Version <= applied {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", migration.Version, err)
		}
		for _, statement := range migration.Statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply schema migration %d (%s): %w", migration.Version, migration.Name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			migration.Version, migration.Name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", migration.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

func (s *Store) appliedMigrationVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read applied schema version: %w", err)
	}
	return version, nil
}

func (s *Store) declaredSchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT schema_version FROM product_metadata WHERE singleton = 1`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read declared schema version: %w", err)
	}
	return version, nil
}
