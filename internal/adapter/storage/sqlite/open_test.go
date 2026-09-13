package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestOpenAppliesPragmasToEveryPooledConnection catches configuring only the
// first SQLite connection and silently losing foreign keys or busy handling as
// database/sql expands the pool.
func TestOpenAppliesPragmasToEveryPooledConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(ctx, Config{
		Path:        filepath.Join(t.TempDir(), "workflow.db"),
		BusyTimeout: 5 * time.Second,
		MaxReaders:  4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	connections := make([]*sql.Conn, 4)
	for index := range connections {
		connection, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire pooled connection %d: %v", index, err)
		}
		connections[index] = connection
		t.Cleanup(func() { _ = connection.Close() })
	}

	for index, connection := range connections {
		var foreignKeys int
		var journalMode string
		var busyTimeout int
		if err := connection.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("connection %d foreign_keys: %v", index, err)
		}
		if err := connection.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("connection %d journal_mode: %v", index, err)
		}
		if err := connection.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("connection %d busy_timeout: %v", index, err)
		}
		if foreignKeys != 1 || journalMode != "wal" || busyTimeout != 5000 {
			t.Fatalf("connection %d pragmas = foreign_keys:%d journal_mode:%q busy_timeout:%d", index, foreignKeys, journalMode, busyTimeout)
		}
	}
}

// TestImmediateReturnsStorageBusyWithinBound catches an implicit read-then-write
// fallback or unbounded SQLite busy wait when another process owns the writer.
func TestImmediateReturnsStorageBusyWithinBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	open := func() *Store {
		store, err := Open(ctx, Config{Path: path, BusyTimeout: 75 * time.Millisecond, MaxReaders: 2})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	first := open()
	second := open()

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- first.immediate(ctx, func(tx *immediateTx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	started := time.Now()
	err := second.immediate(ctx, func(*immediateTx) error { return nil })
	elapsed := time.Since(started)
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatalf("first immediate: %v", firstErr)
	}
	if !errors.Is(err, ErrStorageBusy) {
		t.Fatalf("second immediate = %v, want ErrStorageBusy", err)
	}
	if elapsed > 750*time.Millisecond {
		t.Fatalf("busy result took %v, configured bound was 75ms", elapsed)
	}
}

// TestImmediateDiscardsConnectionAfterEndUncertainty catches returning a
// connection with an unknown transaction state to the pool after COMMIT or
// ROLLBACK could not be confirmed.
func TestImmediateDiscardsConnectionAfterEndUncertainty(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"commit", "rollback"} {
		t.Run(end, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "workflow.db"), BusyTimeout: time.Second, MaxReaders: 1})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			injected := errors.New("injected transaction-end uncertainty")
			var once sync.Once
			store.endTransactionHook = func(operation string) error {
				var result error
				once.Do(func() {
					if operation == end {
						result = injected
					}
				})
				return result
			}
			callbackErr := error(nil)
			if end == "rollback" {
				callbackErr = errors.New("force rollback")
			}
			err = store.immediate(ctx, func(tx *immediateTx) error {
				_, execErr := tx.ExecContext(ctx, "CREATE TABLE uncertain_connection(value INTEGER)")
				if execErr != nil {
					return execErr
				}
				return callbackErr
			})
			if !errors.Is(err, injected) {
				t.Fatalf("immediate uncertainty = %v, want injected error", err)
			}
			store.endTransactionHook = nil
			if err := store.immediate(ctx, func(tx *immediateTx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE connection_recovered(value INTEGER)")
				return err
			}); err != nil {
				t.Fatalf("new transaction after uncertain %s: %v", end, err)
			}
		})
	}
}
