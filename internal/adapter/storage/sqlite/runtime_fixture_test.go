package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpgen/internal/clock"
)

// Runtime tests need independent databases at the current schema, not another
// replay of every historical table rebuild. Keep only the closed database's
// bytes: no writable database or connection is shared between tests. Migration
// and first-open tests call Open directly and still exercise real migrations.
var runtimeDatabaseTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "cpgen-sqlite-test-template-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	store, err := OpenWithClock(context.Background(), Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock.NewFake(testNow))
	if err != nil {
		return nil, err
	}
	// Closing the last connection checkpoints the WAL before we copy the main
	// file. Propagate errors so an incomplete template cannot seed fixtures.
	if err := store.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

func seedRuntimeDatabase(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		// Reopen and crash-recovery tests must retain their actual database.
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat runtime database: %v", err)
	}
	contents, err := runtimeDatabaseTemplate()
	if err != nil {
		t.Fatalf("build runtime database template: %v", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatalf("create runtime database: %v", err)
	}
	_, writeErr := file.Write(contents)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatalf("copy runtime database template: %v", err)
	}
}

func TestRuntimeDatabaseTemplateIncludesCommittedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	seedRuntimeDatabase(t, path)
	// Bypass Open so a missing WAL checkpoint cannot be hidden by rerunning
	// migrations when opening the copied fixture.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	assertMigrationHistory(t, &Store{db: db}, len(migrations))
}

func TestRuntimeStoreFixturesAreIsolatedAndPreserveReopens(t *testing.T) {
	for _, name := range []string{"first", "second", "third", "fourth"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "fixture.db")
			store := openRuntimeStore(t, path, clock.NewFake(testNow))
			// Identical table names and different values expose either shared
			// connections or leaked writes between template copies.
			if _, err := store.db.Exec(`CREATE TABLE fixture_marker(value TEXT NOT NULL) STRICT`); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`INSERT INTO fixture_marker(value) VALUES(?)`, name); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openRuntimeStore(t, path, clock.NewFake(testNow.Add(time.Hour)))
			var got string
			if err := reopened.db.QueryRow(`SELECT value FROM fixture_marker`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != name {
				t.Fatalf("reopened marker = %q, want %q", got, name)
			}
		})
	}
}
