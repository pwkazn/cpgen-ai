package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrationRecordsVersionNameAndHashAndReopens catches migrations that
// cannot prove which immutable SQL bytes produced a database.
func TestMigrationRecordsVersionNameAndHashAndReopens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var version int
	var name, hash string
	if err := store.db.QueryRowContext(ctx, "SELECT version, name, sha256 FROM schema_migrations").Scan(&version, &name, &hash); err != nil {
		t.Fatalf("migration record: %v", err)
	}
	if version != 1 || name != "000001_workflow_core.sql" || hash != migrationOneHash(t) {
		t.Fatalf("migration record = (%d, %q, %q)", version, name, hash)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	reopened, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration count = %d, want 1", count)
	}
}

// TestMigrationRejectsChangedHash catches silently accepting a database whose
// applied migration bytes no longer match the compiled migration.
func TestMigrationRejectsChangedHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.ExecContext(ctx, "UPDATE schema_migrations SET sha256 = ? WHERE version = 1", "sha256:"+strings.Repeat("0", 64)); err != nil {
		t.Fatalf("corrupt migration hash: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}); !errors.Is(err, ErrMigrationDrift) {
		t.Fatalf("reopen changed hash = %v, want ErrMigrationDrift", err)
	}
}

// TestMigrationRejectsMissingVersion catches accepting a non-contiguous or
// future schema without the exact compiled predecessor chain.
func TestMigrationRejectsMissingVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE schema_migrations SET version = 2 WHERE version = 1"); err != nil {
		t.Fatalf("create migration gap: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}); !errors.Is(err, ErrMigrationGap) {
		t.Fatalf("reopen missing version = %v, want ErrMigrationGap", err)
	}
}

// TestMigrationLeavesHealthyDatabase catches DDL that disables referential
// checks or leaves structural corruption after a normal migration.
func TestMigrationLeavesHealthyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "workflow.db"), BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	rows, err := store.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	var integrity string
	if err := store.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
}

func migrationOneHash(t *testing.T) string {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("loaded %d migrations, want 1", len(migrations))
	}
	return migrations[0].hash
}
