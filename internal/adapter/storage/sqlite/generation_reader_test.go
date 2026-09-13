package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestRuntimeCreateRunAdmitsBlankRandomGenerationRequest(t *testing.T) {
	create, expected := generationReaderCreate(t, true, nil, 9007199254740993)
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "random.db"), clock.NewFake(testNow))
	if _, err := store.CreateRun(context.Background(), create); err != nil {
		t.Fatalf("valid random snapshot cannot be persisted: %v", err)
	}
	raw, _, err := store.RunViewDocuments(context.Background(), create.RunID)
	if err != nil || !bytes.Equal(raw, create.SubmittedRequestJSON) || domain.SumBytes(raw) != expected.RequestDigest {
		t.Fatalf("random request was rewritten: %v", err)
	}
}

func TestGenerationSnapshotReaderRestoresExactRequestAndEffectiveSeed(t *testing.T) {
	negative := int64(-9007199254740993)
	for _, scenario := range []struct {
		name      string
		random    bool
		seed      *int64
		effective int64
	}{
		{"explicit", false, &negative, negative},
		{"derived", false, nil, 9007199254740993},
		{"random", true, nil, -9007199254740993},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			create, expected := generationReaderCreate(t, scenario.random, scenario.seed, scenario.effective)
			path := filepath.Join(t.TempDir(), "snapshot.db")
			store := openRuntimeStore(t, path, clock.NewFake(testNow))
			if _, err := store.CreateRun(context.Background(), create); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openRuntimeStore(t, path, clock.NewFake(testNow.Add(time.Hour)))
			actual, err := store.ReadGenerationSnapshot(context.Background(), create.RunID)
			if err != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("snapshot=%+v want=%+v err=%v", actual, expected, err)
			}
			actual.Request.Tags[0] = "changed"
			if actual.Request.Seed != nil {
				*actual.Request.Seed = 0
			}
			actual, err = store.ReadGenerationSnapshot(context.Background(), create.RunID)
			if err != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatal("returned snapshot aliased persisted content")
			}
			var attempts, calls int
			if err := store.db.QueryRow(`SELECT count(*) FROM stage_attempts`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT count(*) FROM call_records`).Scan(&calls); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 || calls != 0 {
				t.Fatal("snapshot read started execution")
			}
		})
	}
}

func TestGenerationSnapshotReaderRejectsIncompatibleOrCorruptBindings(t *testing.T) {
	for _, scenario := range []string{"request-hash", "request-schema", "effective-seed", "legacy-mode"} {
		t.Run(scenario, func(t *testing.T) {
			seed := int64(7)
			create, snapshot := generationReaderCreate(t, false, &seed, seed)
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "corrupt.db"), clock.NewFake(testNow))
			if scenario == "legacy-mode" {
				create = testCreateRunRequest(testRunID, testNow, time.Second)
			} else if scenario == "request-schema" {
				snapshot.Request.SchemaVersion = "cpgen.request/v2"
				raw, err := snapshot.Request.CanonicalJSON()
				if err != nil {
					t.Fatal(err)
				}
				create.SubmittedRequestJSON, create.SubmittedRequestDigest = raw, domain.SumBytes(raw)
				create.SchemaVersion = "cpgen.request/v2"
			}
			if _, err := store.CreateRun(context.Background(), create); err != nil {
				t.Fatal(err)
			}
			query := ""
			switch scenario {
			case "request-hash":
				query = `UPDATE runs SET submitted_request_json=CAST(json_set(CAST(submitted_request_json AS TEXT),'$.brief','changed') AS BLOB) WHERE run_id=?`
			case "effective-seed":
				query = `UPDATE runs SET effective_seed=99 WHERE run_id=?`
			}
			if query != "" {
				if _, err := store.db.Exec(query, create.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ReadGenerationSnapshot(context.Background(), create.RunID); !errors.Is(err, ErrConsistency) {
				t.Fatalf("%s accepted: %v", scenario, err)
			}
		})
	}
}

func generationReaderCreate(t *testing.T, random bool, seed *int64, effective int64) (domain.CreateRunRequest, domain.GenerationRequestSnapshotV1) {
	t.Helper()
	create := testCreateRunRequest(testRunID, testNow, time.Minute)
	request := domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: " Cafe\u0301 graphs ", Tags: []string{" Trees ", "graphs"}, NormalizedTags: []string{"graphs", "trees"}, Language: "en", Difficulty: "hard", RequiredFeatures: []string{}, ForbiddenFeatures: nil, TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", Seed: seed, VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: create.BudgetLimits}
	if random {
		request.Mode, request.Brief = domain.RequestModeRandom, ""
	}
	snapshot, err := domain.NewGenerationRequestSnapshotV1(request, effective)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	create.SubmittedRequestJSON, create.SubmittedRequestDigest, create.EffectiveSeed = raw, snapshot.RequestDigest, effective
	create.WorkflowRevision = "slice2.idea.statement.similarity.v1"
	create.WorkflowDigest = domain.SumBytes([]byte(create.WorkflowRevision))
	create.StageSequence = []domain.StageName{"idea", "statement", "similarity"}
	return create, snapshot
}
