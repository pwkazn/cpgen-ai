package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestMutationRecordSurvivesProcessDeathAtCommitBoundary(t *testing.T) {
	for _, boundary := range []string{"before_commit", "after_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, record := mutationRecordFixture(t, domain.ArtifactOutput)
			configuration := f.store.config
			beforeBudget, err := f.store.BudgetSnapshot(context.Background(), f.runID)
			if err != nil {
				t.Fatal(err)
			}
			beforeRun, err := f.store.GetRun(context.Background(), f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			commandPath := filepath.Join(t.TempDir(), "record.json")
			if err := os.WriteFile(commandPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			runChild := func(mode string, expected int) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMutationRecordProcessHelper$", "-test.timeout=25s")
				child.Env = append(os.Environ(), "CPGEN_MUTATION_RECORD_HELPER=1", "CPGEN_MUTATION_RECORD_DB="+configuration.Path,
					"CPGEN_MUTATION_RECORD_COMMAND="+commandPath, "CPGEN_MUTATION_RECORD_BOUNDARY="+mode)
				output, err := child.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("record helper timeout: %v %s", ctx.Err(), output)
				}
				if child.ProcessState == nil || child.ProcessState.ExitCode() != expected {
					t.Fatalf("record helper %s: %v %s", mode, err, output)
				}
			}
			runChild(boundary, 73)
			probe, err := Open(context.Background(), configuration)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := probe.db.QueryRow(`SELECT count(*) FROM mutation_records`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if boundary == "after_commit" {
				want = 1
			}
			if count != want {
				t.Fatalf("%s retained %d records, want %d", boundary, count, want)
			}
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
			runChild("recover", 0)
			runChild("recover", 0)
			reopened, err := Open(context.Background(), configuration)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for _, table := range []string{"mutation_claims", "mutation_records", "mutation_record_operations", "mutation_record_reservations", "mutation_record_output_occurrences"} {
				if err := reopened.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s after restart=%d %v", table, count, err)
				}
			}
			afterBudget, err := reopened.BudgetSnapshot(context.Background(), f.runID)
			if err != nil || afterBudget.Version != beforeBudget.Version || afterBudget.Remaining[domain.BudgetArtifactPhysicalNewBytes] != beforeBudget.Remaining[domain.BudgetArtifactPhysicalNewBytes] {
				t.Fatalf("record restart changed budget: %+v %v", afterBudget, err)
			}
			afterRun, err := reopened.GetRun(context.Background(), f.runID)
			if err != nil || afterRun.Version != beforeRun.Version || afterRun.CurrentStage != beforeRun.CurrentStage {
				t.Fatalf("record restart changed stage: %+v %v", afterRun, err)
			}
		})
	}
}

func TestMutationRecordProcessHelper(t *testing.T) {
	if os.Getenv("CPGEN_MUTATION_RECORD_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(os.Getenv("CPGEN_MUTATION_RECORD_COMMAND"))
	if err != nil {
		t.Fatal(err)
	}
	var command domain.MutationRecordRequest
	if err := json.Unmarshal(raw, &command); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), Config{Path: os.Getenv("CPGEN_MUTATION_RECORD_DB"), BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("CPGEN_MUTATION_RECORD_BOUNDARY") == "before_commit" {
		store.endTransactionHook = func(operation string) error {
			if operation == "commit" {
				os.Exit(73)
			}
			return nil
		}
	}
	if err := store.RecordMutation(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CPGEN_MUTATION_RECORD_BOUNDARY") == "after_commit" {
		os.Exit(73)
	}
}
