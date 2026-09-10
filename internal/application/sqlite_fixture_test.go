package application_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

var applicationSchemaFixture struct {
	once sync.Once
	data []byte
	err  error
}

// Each business fixture gets an independent copy of an empty, fully migrated
// database. The template is built by the real migrator once per test process;
// normal OpenWithClock still validates every copied schema. Storage migration,
// production Bootstrap and reopen tests continue to use their original paths.
func openFreshApplicationSQLite(t *testing.T, config sqlite.Config, testClock clock.Clock) (*sqlite.Store, error) {
	t.Helper()
	applicationSchemaFixture.once.Do(func() {
		path := filepath.Join(t.TempDir(), "empty-schema.db")
		store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2},
			clock.NewFake(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)))
		if err != nil {
			applicationSchemaFixture.err = err
			return
		}
		// Closing the last connection checkpoints the WAL before copying the
		// main file. No live database or per-test row is shared with callers.
		if err := store.Close(); err != nil {
			applicationSchemaFixture.err = err
			return
		}
		applicationSchemaFixture.data, applicationSchemaFixture.err = os.ReadFile(path)
	})
	if applicationSchemaFixture.err != nil {
		return nil, applicationSchemaFixture.err
	}
	file, err := os.OpenFile(config.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	n, writeErr := file.Write(applicationSchemaFixture.data)
	if writeErr == nil && n != len(applicationSchemaFixture.data) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return nil, err
	}
	return sqlite.OpenWithClock(context.Background(), config, testClock)
}

func TestApplicationSchemaFixtureKeepsDatabasesIndependent(t *testing.T) {
	ctx := context.Background()
	limits := domain.BudgetLimits{MaxLLMCalls: 1, MaxActiveTimeMilliseconds: 1000}
	first := newCoordinatorFixture(t, "e9", limits)
	second := newCoordinatorFixture(t, "e9", limits)
	control, err := first.store.RequestCancel(ctx, domain.CancelRequest{ID: "control_000000000000000000000000000000e9", RunID: first.runID,
		ExpectedRunVersion: 2, Reason: "fixture isolation", IdempotencyKey: coordinatorID("cancel", "fixture isolation"), At: first.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := second.store.PendingCancel(ctx, second.runID); err != nil || pending != nil {
		t.Fatalf("independent database inherited control state: %+v %v", pending, err)
	}
	configuration := sqlite.Config{Path: first.path, BusyTimeout: time.Second, MaxReaders: 4}
	if _, err := openFreshApplicationSQLite(t, configuration, first.clock); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fixture helper could overwrite an existing database: %v", err)
	}
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenWithClock(ctx, configuration, first.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if pending, err := reopened.PendingCancel(ctx, first.runID); err != nil || pending == nil || pending.ID != control.ID {
		t.Fatalf("ordinary reopen lost committed control: %+v %v", pending, err)
	}
}
