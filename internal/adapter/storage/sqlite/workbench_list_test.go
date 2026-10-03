package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func seedWorkbenchRun(t *testing.T, store *Store, id domain.RunID, state domain.RunState, at time.Time) {
	t.Helper()
	request := testCreateRunRequest(id, at, time.Minute)
	request.IdempotencyKey = "create_" + strings.TrimPrefix(string(id), "run_")
	mustCreateRun(t, store, request)
	if state == domain.RunCreated {
		return
	}
	var summary any
	if state == domain.RunCancelled {
		summary = "cancelled by list fixture"
	}
	if _, err := store.db.Exec(`UPDATE runs SET state=?,cancel_summary=? WHERE run_id=?`, state, summary, id); err != nil {
		t.Fatal(err)
	}
}

func TestWorkbenchRunsFiltersBeforeLimitAndUsesIndexes(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "list.db"), clock.NewFake(testNow))
	seedWorkbenchRun(t, store, testRunID, domain.RunBlocked, testNow)
	// More than the old UI's 1000-row window separates the matching row from
	// the first page. Filtering must happen before any page/window limit.
	for i := 2; i <= 1003; i++ {
		seedWorkbenchRun(t, store, domain.RunID(fmt.Sprintf("run_%032x", i)), domain.RunCreated, testNow.Add(time.Second))
	}
	page, err := store.WorkbenchRuns(context.Background(), port.WorkbenchRunQuery{State: "blocked", Limit: 1})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].RunID != testRunID || page.NextCursor != "" {
		t.Fatalf("old filtered row: %+v, %v", page, err)
	}
	state := domain.RunBlocked
	legacy, err := store.WorkbenchRecentRuns(context.Background(), domain.RunFilter{State: &state, Limit: 1})
	if err != nil || !reflect.DeepEqual(legacy, page.Runs) {
		t.Fatalf("legacy projection changed: %+v, %v", legacy, err)
	}
	legacy, err = store.WorkbenchRecentRuns(context.Background(), domain.RunFilter{})
	if err != nil || legacy == nil || len(legacy) != 0 {
		t.Fatalf("legacy zero-limit behavior changed: %+v, %v", legacy, err)
	}
	assertWorkbenchListPlans(t, store)
}

func TestWorkbenchRunsStablePagesAcrossStatesAndNanoseconds(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "list.db"), clock.NewFake(testNow))
	states := []domain.RunState{domain.RunCreated, domain.RunRunning, domain.RunFailed, domain.RunCancelled, domain.RunBlocked, domain.RunNeedsReview}
	for i := 1; i <= 18; i++ {
		// Equal timestamps exercise the TEXT run ID tie-breaker; nanosecond
		// differences would disappear if cursors were rebuilt from milliseconds.
		at := testNow.Add(time.Duration(i/6) * time.Nanosecond)
		seedWorkbenchRun(t, store, domain.RunID(fmt.Sprintf("run_%032x", i)), states[(i-1)%len(states)], at)
	}
	seedWorkbenchRun(t, store, "run_ffffffffffffffffffffffffffffffff", domain.RunCreated, testNow.Add(3*time.Nanosecond))
	for _, filter := range []string{"", "active", "ended", "CREATED", "RUNNING", "FAILED", "CANCELLED", "BLOCKED", "blocked", "NEEDS_REVIEW", "READY", "ready"} {
		t.Run(filter, func(t *testing.T) {
			wantedStates, err := (port.WorkbenchRunQuery{State: filter}).States()
			if err != nil {
				t.Fatal(err)
			}
			query := `SELECT run_id FROM runs`
			var args []any
			if len(wantedStates) != 0 {
				var placeholders []string
				for _, state := range wantedStates {
					placeholders = append(placeholders, "?")
					args = append(args, state)
				}
				query += " WHERE state IN (" + strings.Join(placeholders, ",") + ")"
			}
			rows, err := store.db.Query(query+" ORDER BY updated_at DESC,run_id DESC", args...)
			if err != nil {
				t.Fatal(err)
			}
			want := []domain.RunID{}
			for rows.Next() {
				var id domain.RunID
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				want = append(want, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			got := []domain.RunID{}
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > 20 {
					t.Fatal("cursor failed to make progress")
				}
				page, err := store.WorkbenchRuns(context.Background(), port.WorkbenchRunQuery{State: filter, Limit: 2, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if page.Runs == nil || len(page.Runs) > 2 || (page.NextCursor != "" && len(page.Runs) != 2) {
					t.Fatalf("invalid page: %+v", page)
				}
				for _, run := range page.Runs {
					if run.Brief != "test request" || run.Version != 1 {
						t.Fatalf("summary projection lost fields: %+v", run)
					}
					got = append(got, run.RunID)
				}
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("paged IDs=%v; SQL order=%v", got, want)
			}
		})
	}
}

func TestWorkbenchRunsReadyAlias(t *testing.T) {
	store, command := packageCommitFixture(t)
	if _, err := store.FinalizeVerifiedPackage(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"ready", "READY"} {
		page, err := store.WorkbenchRuns(context.Background(), port.WorkbenchRunQuery{State: state, Limit: 1})
		if err != nil || len(page.Runs) != 1 || page.Runs[0].RunID != command.Finish.RunID || page.Runs[0].State != domain.RunReady || page.NextCursor != "" {
			t.Fatalf("ready alias: %+v, %v", page, err)
		}
	}
}

func TestWorkbenchRunsRejectsInvalidQueryAndCursor(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "list.db"), clock.NewFake(testNow))
	for _, q := range []port.WorkbenchRunQuery{{Limit: 0}, {Limit: -1}, {Limit: 1001}, {State: "ALL", Limit: 1}, {State: "running", Limit: 1}, {State: "CREATED,RUNNING", Limit: 1}} {
		if _, err := store.WorkbenchRuns(context.Background(), q); !errors.Is(err, port.ErrInvalidWorkbenchRunQuery) {
			t.Fatalf("accepted query %+v: %v", q, err)
		}
	}
	base := workbenchRunCursor{Version: "1", Filter: "BLOCKED", UpdatedAt: formatTime(testNow.Add(123 * time.Nanosecond)), RunID: "run_ffffffffffffffffffffffffffffffff"}
	encode := func(c workbenchRunCursor) string {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	valid := encode(base)
	for _, filter := range []string{"blocked", "BLOCKED"} {
		page, err := store.WorkbenchRuns(context.Background(), port.WorkbenchRunQuery{State: filter, Limit: 1000, Cursor: valid})
		if err != nil || page.Runs == nil || len(page.Runs) != 0 || page.NextCursor != "" {
			t.Fatalf("valid empty tail/alias cursor rejected: %+v %v", page, err)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(valid)
	bad := []string{"!", valid + "=", valid + "\n", strings.Repeat("x", 513), base64.RawURLEncoding.EncodeToString(append(raw, raw...)), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"version":"1"`, `"version":"1","version":"1"`, 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"version":"1"`, `"unknown":"x","version":"1"`, 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"run_id":"run_ffffffffffffffffffffffffffffffff"`, `"run_id":9223372036854775807`, 1)))}
	for _, edit := range []func(*workbenchRunCursor){
		func(c *workbenchRunCursor) { c.Version = "2" },
		func(c *workbenchRunCursor) { c.Filter = "" },
		func(c *workbenchRunCursor) { c.Filter = "READY" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "2026-09-01T08:00:00.000000123+01:00" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "2026-09-01T08:00:00.0000001234Z" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "2026-09-01T08:00:00Z" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "0001-01-01T00:00:00.000000000Z" },
		func(c *workbenchRunCursor) { c.UpdatedAt = "2026-13-01T08:00:00.000000000Z" },
		func(c *workbenchRunCursor) { c.RunID = "" },
		func(c *workbenchRunCursor) { c.RunID = "9223372036854775807" },
	} {
		changed := base
		edit(&changed)
		bad = append(bad, encode(changed))
	}
	for i, cursor := range bad {
		if _, err := store.WorkbenchRuns(context.Background(), port.WorkbenchRunQuery{State: "blocked", Limit: 1, Cursor: cursor}); !errors.Is(err, port.ErrInvalidWorkbenchRunQuery) {
			t.Errorf("invalid cursor %d was accepted: %v", i, err)
		}
	}
}

func TestWorkbenchRunIndexMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m31.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:31] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	seedWorkbenchRun(t, store, testRunID, domain.RunCreated, testNow)
	before, err := store.ReadWorkbenchRun(ctx, testRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadWorkbenchRun(ctx, testRunID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("migration changed persisted run/documents/stages/budgets/events: %v", err)
	}
	assertMigrationHistory(t, store, 32)
	assertWorkbenchListPlans(t, store)
}

func assertWorkbenchListPlans(t *testing.T, store *Store) {
	t.Helper()
	cursor := &workbenchRunCursor{UpdatedAt: formatTime(testNow.Add(time.Hour)), RunID: testRunID}
	// Compound aliases use exactly these per-state indexed queries, inside a
	// single read transaction, followed by a bounded merge in Go.
	for _, state := range []string{"", "CREATED", "RUNNING", "FAILED", "CANCELLED", "READY", "BLOCKED", "NEEDS_REVIEW"} {
		for _, boundary := range []*workbenchRunCursor{nil, cursor} {
			query, args := workbenchRunPageSQL(state, boundary, 51)
			rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			var plans []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			plan := strings.Join(plans, "; ")
			index := "runs_updated_at_run_id"
			if state != "" {
				index = "runs_state_updated_at_run_id"
			}
			if !strings.Contains(plan, "USING INDEX "+index) || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("state=%q cursor=%t: expected %s without temporary sort; got %s", state, boundary != nil, index, plan)
			}
			if (state != "" || boundary != nil) && !strings.Contains(plan, "SEARCH runs") {
				t.Fatalf("filtered/cursor query must seek its index: %s", plan)
			}
			t.Logf("state=%q cursor=%t: %s", state, boundary != nil, plan)
		}
	}
}
