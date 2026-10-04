package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestHTTPRevisionTargetsUseFrozenWorkflowAndRewindSimilarityProducers(t *testing.T) {
	ctx := context.Background()
	// Serving configuration is Fake, while the persisted runs use generation
	// and historical revisions. Choices must come from each run's frozen graph.
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	body, _ := json.Marshal(map[string]string{"token": server.secret})
	exchange := httptest.NewRecorder()
	handler.ServeHTTP(exchange, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(body)))
	if exchange.Code != http.StatusOK {
		t.Fatalf("session: %d %s", exchange.Code, exchange.Body.String())
	}
	cookies := exchange.Result().Cookies()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CPGen-CSRF", "local-session")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for index, tc := range []struct {
		revision        string
		current, target domain.StageName
		want            []domain.StageName
	}{
		{workflow.ExecutedSamplesRevision, "similarity_decision", "idea", []domain.StageName{"idea", "statement", "similarity", "similarity_decision"}},
		{workflow.ExecutedSamplesRevision, "similarity_decision", "statement", []domain.StageName{"idea", "statement", "similarity", "similarity_decision"}},
		{workflow.FakeRevision, "checkpoint", "prepare", []domain.StageName{"checkpoint", "prepare", "exercise"}},
		{workflow.LegacySimilarityRevision, "similarity", "statement", []domain.StageName{"idea", "statement", "similarity"}},
	} {
		t.Run(tc.revision+"/"+string(tc.target), func(t *testing.T) {
			run := createRevisionTargetFixture(t, server, index, tc.revision, tc.current)
			path := "/api/runs/" + string(run.RunID)
			response := request(http.MethodGet, path, "")
			if response.Code != http.StatusOK {
				t.Fatalf("detail: %d %s", response.Code, response.Body.String())
			}
			var detail struct {
				Data struct {
					Targets []domain.StageName `json:"revision_targets"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(detail.Data.Targets, tc.want) {
				t.Fatalf("targets = %v, want %v", detail.Data.Targets, tc.want)
			}
			reviewBody := func(target domain.StageName) string {
				return fmt.Sprintf(`{"operation_key":"revise-%s-%s","expected_run_version":"%d","workflow_revision":%q,"kind":"REVISE","revision_target_stage":%q,"reviewer":"test","reason":"regenerate upstream content"}`, run.RunID, target, run.Version, run.WorkflowRevision, target)
			}
			for _, invalid := range []domain.StageName{"solution", "missing_stage"} {
				response = request(http.MethodPost, path+"/review", reviewBody(invalid))
				if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_revision_target") {
					t.Fatalf("invalid target %q: %d %s", invalid, response.Code, response.Body.String())
				}
				pending, err := server.app.Reviews.PendingReview(ctx, run.RunID)
				if err != nil || pending != nil {
					t.Fatalf("invalid target persisted review: %+v, %v", pending, err)
				}
			}
			response = request(http.MethodPost, path+"/review", reviewBody(tc.target))
			if response.Code != http.StatusCreated {
				t.Fatalf("revise %s: %d %s", tc.target, response.Code, response.Body.String())
			}
			decision, err := server.app.Reviews.PendingReview(ctx, run.RunID)
			if err != nil || decision == nil || decision.RevisionTargetStage == nil || *decision.RevisionTargetStage != tc.target {
				t.Fatalf("persisted target = %+v, %v", decision, err)
			}
			unchanged, err := server.app.Runtime.GetRun(ctx, run.RunID)
			if err != nil || unchanged.CurrentStage != tc.current || unchanged.State != domain.RunNeedsReview {
				t.Fatalf("review applied before resume: %+v, %v", unchanged, err)
			}
			definition, _ := workflow.DefinitionFor(tc.revision)
			sequence := definition.Stages()
			input := domain.SumBytes([]byte("review target input"))
			configJSON := []byte(`{}`)
			configDigest := domain.SumBytes(configJSON)
			revised, err := server.app.Reviews.ApplyReview(ctx, domain.ApplyReviewCommand{
				RunID: run.RunID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
				StageName: tc.current, StageInputDigest: decision.StageInputDigest, EvidenceDigest: decision.EvidenceDigest, PolicyDigest: decision.PolicyDigest,
				NewInputDigest: &input, NewConfigJSON: configJSON, NewConfigDigest: &configDigest,
				InvalidatedStages: sequence[slices.Index(sequence, tc.target):], IdempotencyKey: operationID("apply", string(run.RunID)), At: time.Now().UTC(),
			})
			if err != nil || revised.CurrentStage != tc.target || revised.State != domain.RunCreated {
				t.Fatalf("upstream rewind = %+v, %v", revised, err)
			}
			// A replay after the run has rewound still returns the original decision.
			response = request(http.MethodPost, path+"/review", reviewBody(tc.target))
			if response.Code != http.StatusCreated {
				t.Fatalf("replay after rewind: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func createRevisionTargetFixture(t *testing.T, server *Server, index int, revision string, current domain.StageName) domain.RunSnapshot {
	t.Helper()
	ctx := context.Background()
	definition, err := workflow.DefinitionFor(revision)
	if err != nil {
		t.Fatal(err)
	}
	sequence := definition.Stages()
	configJSON := []byte(`{}`)
	configDigest := domain.SumBytes(configJSON)
	input := domain.SumBytes([]byte("review target input"))
	limits := domain.BudgetLimits{MaxActiveTimeMilliseconds: 30000}
	submitted := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "revision target fixture", Tags: []string{}, NormalizedTags: []string{}, Language: "en", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}
	raw, err := json.Marshal(submitted)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := server.app.Runtime.CreateRun(ctx, domain.CreateRunRequest{
		RunID: domain.RunID(fmt.Sprintf("run_%032x", index+1)), SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw), EffectiveSeed: 7,
		RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: configDigest, WorkflowRevision: revision, SchemaVersion: domain.RequestSchemaV1,
		WorkflowDigest: domain.SumBytes([]byte(revision)), BudgetLimits: limits, StageSequence: sequence, CreatedAt: now, IdempotencyKey: operationID("create", fmt.Sprint(index)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for ordinal, stage := range sequence {
		attempt := domain.AttemptID(fmt.Sprintf("attempt_%032x", index*100+ordinal+1))
		_, err = server.app.Runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: stage, AttemptID: attempt, InputDigest: input, IdempotencyKey: operationID("begin", string(run.RunID)+string(stage)), At: now})
		if err != nil {
			t.Fatal(err)
		}
		run, err = server.app.Runtime.GetRun(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		finish := domain.FinishStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: stage, AttemptID: attempt, IdempotencyKey: operationID("finish", string(run.RunID)+string(stage)), At: now}
		if stage == current {
			finish.AttemptState, finish.RunState = domain.StageAttemptNeedsReview, domain.RunNeedsReview
			finish.ReviewEvidenceDigest, finish.ReviewPolicyDigest = &input, &configDigest
		} else {
			finish.AttemptState, finish.RunState = domain.StageAttemptSucceeded, domain.RunRunning
			finish.OutputDigest, finish.NextStage, finish.NextInputDigest = &input, sequence[ordinal+1], &input
		}
		run, err = server.app.Runtime.FinishStage(ctx, finish)
		if err != nil {
			t.Fatal(err)
		}
		if stage == current {
			return run
		}
	}
	t.Fatalf("review stage %q is absent", current)
	return run
}
