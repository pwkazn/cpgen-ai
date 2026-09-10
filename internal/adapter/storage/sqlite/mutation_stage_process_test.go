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

func TestFinishMutationStageSurvivesProcessDeath(t *testing.T) {
	for _, boundary := range []string{"before_commit", "after_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, command := mutationStageFixture(t)
			configuration := f.store.config
			beforeBudget, err := f.store.BudgetSnapshot(context.Background(), f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			commandPath := filepath.Join(t.TempDir(), "finish-mutation.json")
			if err := os.WriteFile(commandPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			runChild := func(mode string, expected int) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFinishMutationStageProcessHelper$", "-test.timeout=25s")
				child.Env = append(os.Environ(), "CPGEN_MUTATION_STAGE_HELPER=1", "CPGEN_MUTATION_STAGE_DB="+configuration.Path,
					"CPGEN_MUTATION_STAGE_COMMAND="+commandPath, "CPGEN_MUTATION_STAGE_BOUNDARY="+mode)
				output, err := child.CombinedOutput()
				if ctx.Err() != nil || child.ProcessState == nil || child.ProcessState.ExitCode() != expected {
					t.Fatalf("mutation stage helper %s: context=%v err=%v %s", mode, ctx.Err(), err, output)
				}
			}
			verify := func(want int) {
				t.Helper()
				probe, err := Open(context.Background(), configuration)
				if err != nil {
					t.Fatal(err)
				}
				defer probe.Close()
				for _, table := range []string{"artifact_occurrences", "mutation_records", "mutation_record_operations", "mutation_record_reservations", "mutation_record_output_occurrences"} {
					var count int
					if err := probe.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != want {
						t.Fatalf("%s after process exit=%d want=%d err=%v", table, count, want, err)
					}
				}
				run, err := probe.GetRun(context.Background(), f.runID)
				stage := f.stage
				if want == 1 {
					stage = "exercise"
				}
				if err != nil || run.Version != 2+int64(want) || run.CurrentStage != stage {
					t.Fatalf("output/record/progress disagree after exit: %+v %v", run, err)
				}
				afterBudget, err := probe.BudgetSnapshot(context.Background(), f.runID)
				if err != nil || afterBudget.Version != beforeBudget.Version || afterBudget.Remaining[domain.BudgetArtifactPhysicalNewBytes] != beforeBudget.Remaining[domain.BudgetArtifactPhysicalNewBytes] {
					t.Fatalf("mutation stage recovery changed settled budget: %+v %v", afterBudget, err)
				}
				if want == 0 {
					f.store = probe
					assertMutationStageAbsent(t, f)
				}
			}
			runChild(boundary, 73)
			if boundary == "before_commit" {
				verify(0)
			} else {
				verify(1)
			}
			runChild("recover", 0)
			runChild("recover", 0)
			verify(1)
		})
	}
}

func TestFinishMutationStageProcessHelper(t *testing.T) {
	if os.Getenv("CPGEN_MUTATION_STAGE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(os.Getenv("CPGEN_MUTATION_STAGE_COMMAND"))
	if err != nil {
		t.Fatal(err)
	}
	var command domain.FinishMutationStageCommand
	if err := json.Unmarshal(raw, &command); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), Config{Path: os.Getenv("CPGEN_MUTATION_STAGE_DB"), BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("CPGEN_MUTATION_STAGE_BOUNDARY") == "before_commit" {
		store.endTransactionHook = func(operation string) error {
			if operation == "commit" {
				os.Exit(73)
			}
			return nil
		}
	}
	if _, err := store.FinishMutationStage(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CPGEN_MUTATION_STAGE_BOUNDARY") == "after_commit" {
		os.Exit(73)
	}
}
