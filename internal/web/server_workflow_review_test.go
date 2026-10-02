package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

func TestHTTPCreateReplayUsesOnePersistedRun(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	sessionBody, _ := json.Marshal(map[string]string{"token": server.secret})
	exchange := httptest.NewRecorder()
	exchangeRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(sessionBody))
	exchangeRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(exchange, exchangeRequest)
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange: HTTP %d", exchange.Code)
	}
	cookies := exchange.Result().Cookies()
	doRequest := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CPGen-CSRF", "local-session")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// Large seeds and budget values are strings at the Web boundary. The real
	// Fake workflow exercises persistence and execution without a paid provider.
	const body = `{"operation_key":"http-create-replay-review","request":{"schema_version":"cpgen.request/v1","mode":"manual","brief":"HTTP review fixture","tags":[],"normalized_tags":[],"language":"en","difficulty":"easy","required_features":[],"forbidden_features":[],"time_limit_milliseconds":"1000","memory_limit_megabytes":"64","solution_language":"cpp","seed":"9007199254740993","verification_profile":"default","export_targets":["internal"],"budget_limits":{"max_llm_calls":"0","max_similarity_calls":"0","max_llm_input_tokens":"0","max_llm_output_tokens":"0","max_llm_cost_usd":"0","max_similarity_cost_usd":"0","max_sandbox_creates":"0","max_artifact_bytes":"0","max_package_bytes":"0","max_mutations_per_stage":"0","max_active_time_milliseconds":"5000"}}}`
	created := doRequest(http.MethodPost, "/api/runs/create", body)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create: HTTP %d: %s", created.Code, created.Body.String())
	}
	var result struct {
		Data struct {
			RunID string `json:"run_id"`
			Run   struct {
				RunID string `json:"run_id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	id := result.Data.RunID
	if id == "" {
		id = result.Data.Run.RunID
	}
	if id == "" {
		t.Fatal("create response contains no run identity")
	}
	deadline := time.Now().Add(5 * time.Second)
	for server.manager.isActive(domain.RunID(id)) {
		if time.Now().After(deadline) {
			t.Fatal("Fake executor did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if before.State != domain.RunNeedsReview {
		t.Fatalf("Fake workflow did not reach its review checkpoint: %s", before.State)
	}
	detail := doRequest(http.MethodGet, "/api/runs/"+id, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: HTTP %d: %s", detail.Code, detail.Body.String())
	}
	var detailResult struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailResult); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run", "stages", "budget", "execution", "review", "available_actions", "artifacts", "recent_events"} {
		if len(detailResult.Data[key]) == 0 {
			t.Errorf("aggregate detail missing %s", key)
		}
	}
	replayed := doRequest(http.MethodPost, "/api/runs/create", body)
	if replayed.Code != http.StatusOK && replayed.Code != http.StatusAccepted {
		t.Fatalf("idempotent replay: HTTP %d: %s", replayed.Code, replayed.Body.String())
	}
	if server.manager.isActive(domain.RunID(id)) {
		t.Fatal("replaying create started another executor")
	}
	after, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.State != before.State {
		t.Fatalf("replaying create mutated the run: before=%+v after=%+v", before, after)
	}
	conflict := doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "HTTP review fixture", "changed request", 1))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same identity with changed request must conflict: HTTP %d: %s", conflict.Code, conflict.Body.String())
	}
	rows, err := server.app.Runtime.ListRuns(context.Background(), domain.RunFilter{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("create/replay/conflict persisted unexpected runs: count=%d err=%v", len(rows), err)
	}
	reviewBody := fmt.Sprintf(`{"operation_key":"reject-once","expected_run_version":"%d","workflow_revision":%q,"kind":"REJECT","reviewer":"test","reason":"not accepted"}`, after.Version, after.WorkflowRevision)
	for i := 0; i < 2; i++ {
		response := doRequest(http.MethodPost, "/api/runs/"+id+"/review", reviewBody)
		if response.Code != 201 {
			t.Fatalf("review %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	current, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.RunNeedsReview {
		t.Fatal("review must not automatically execute")
	}
	if response := doRequest(http.MethodGet, "/api/runs/"+id, ""); response.Code != 200 {
		t.Fatalf("pending detail: %d %s", response.Code, response.Body.String())
	}
	resumeBody := fmt.Sprintf(`{"operation_key":"resume-once","expected_run_version":"%d"}`, current.Version)
	response := doRequest(http.MethodPost, "/api/runs/"+id+"/resume", resumeBody)
	if response.Code != 202 {
		t.Fatalf("resume: %d %s", response.Code, response.Body.String())
	}
	awaitManagerEmpty(t, server.manager)
	finished, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != domain.RunFailed {
		t.Fatalf("reject not applied: %+v; execution=%+v", finished, server.manager.observe(domain.RunID(id)))
	}
	// Durable receipts remain effective after the serving process is replaced.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	server, err = New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler = server.Handler()
	sessionBody, _ = json.Marshal(map[string]string{"token": server.secret})
	exchange = httptest.NewRecorder()
	handler.ServeHTTP(exchange, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(sessionBody)))
	cookies = exchange.Result().Cookies()
	for _, pair := range [][2]string{{"resume", resumeBody}, {"review", reviewBody}} {
		response = doRequest(http.MethodPost, "/api/runs/"+id+"/"+pair[0], pair[1])
		if response.Code != 201 && response.Code != 202 {
			t.Fatalf("restart replay %s: %d %s", pair[0], response.Code, response.Body.String())
		}
	}
	if server.manager.isActive(domain.RunID(id)) {
		t.Fatal("replay restarted executor")
	}
	// REVISE records its producer target and the next explicit resume applies
	// the exact suffix from that producer before the Fake workflow reruns.
	reviseCreate := doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "http-create-replay-review", "revision-target-review", 1))
	if reviseCreate.Code != http.StatusAccepted {
		t.Fatalf("revision fixture create: HTTP %d %s", reviseCreate.Code, reviseCreate.Body.String())
	}
	if err := json.Unmarshal(reviseCreate.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	reviseID := result.Data.RunID
	if reviseID == "" {
		reviseID = result.Data.Run.RunID
	}
	awaitManagerEmpty(t, server.manager)
	reviseRun, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(reviseID))
	if err != nil || reviseRun.State != domain.RunNeedsReview || reviseRun.CurrentStage != "checkpoint" {
		t.Fatalf("revision fixture did not reach review checkpoint: %+v %v", reviseRun, err)
	}
	revisionBody := fmt.Sprintf(`{"operation_key":"revise-from-prepare","expected_run_version":"%d","workflow_revision":%q,"kind":"REVISE","revision_target_stage":"prepare","reviewer":"test","reason":"repair producer and rerun checks"}`, reviseRun.Version, reviseRun.WorkflowRevision)
	if response := doRequest(http.MethodPost, "/api/runs/"+reviseID+"/review", revisionBody); response.Code != http.StatusCreated {
		t.Fatalf("REVISE review: HTTP %d %s", response.Code, response.Body.String())
	}
	pendingRevision, err := server.app.Reviews.PendingReview(context.Background(), domain.RunID(reviseID))
	if err != nil || pendingRevision == nil || pendingRevision.RevisionTargetStage == nil || *pendingRevision.RevisionTargetStage != "prepare" {
		t.Fatalf("persisted REVISE target = %+v, %v", pendingRevision, err)
	}
	reviseResumeBody := fmt.Sprintf(`{"operation_key":"resume-revised-run","expected_run_version":"%d"}`, reviseRun.Version+1)
	if response := doRequest(http.MethodPost, "/api/runs/"+reviseID+"/resume", reviseResumeBody); response.Code != http.StatusAccepted {
		t.Fatalf("REVISE resume: HTTP %d %s", response.Code, response.Body.String())
	}
	awaitManagerEmpty(t, server.manager)
	revisedRun, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(reviseID))
	if err != nil || revisedRun.State != domain.RunNeedsReview || revisedRun.CurrentStage != "checkpoint" {
		t.Fatalf("revised Fake run did not repeat downstream review: %+v %v", revisedRun, err)
	}
	attemptReader, ok := server.app.Runtime.(interface {
		CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
	})
	if !ok {
		t.Fatal("runtime does not expose current stage attempts")
	}
	prepareAttempt, err := attemptReader.CurrentStageAttempt(context.Background(), domain.RunID(reviseID), "prepare")
	if err != nil || prepareAttempt.Ordinal != 2 {
		t.Fatalf("REVISE did not restart its selected producer: %+v %v", prepareAttempt, err)
	}
	// A budget-patched RETRY applies only when the explicit Web resume reaches
	// the shared LocalRunService/SQLite application path.
	retryCreate := doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "http-create-replay-review", "retry-budget-review", 1))
	if retryCreate.Code != http.StatusAccepted {
		t.Fatalf("retry fixture create: HTTP %d %s", retryCreate.Code, retryCreate.Body.String())
	}
	if err := json.Unmarshal(retryCreate.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	retryID := result.Data.RunID
	if retryID == "" {
		retryID = result.Data.Run.RunID
	}
	awaitManagerEmpty(t, server.manager)
	retryRun, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(retryID))
	if err != nil || retryRun.State != domain.RunNeedsReview {
		t.Fatalf("retry fixture did not reach review: %+v %v", retryRun, err)
	}
	retryReviewBody := fmt.Sprintf(`{"operation_key":"approve-budget","expected_run_version":"%d","workflow_revision":%q,"kind":"RETRY","reviewer":"test","reason":"approved retry allowance","budget_increase":{"max_similarity_cost_usd":"0.25"}}`, retryRun.Version, retryRun.WorkflowRevision)
	if response := doRequest(http.MethodPost, "/api/runs/"+retryID+"/review", retryReviewBody); response.Code != http.StatusCreated {
		t.Fatalf("budget review: HTTP %d %s", response.Code, response.Body.String())
	}
	retryPending, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(retryID))
	if err != nil {
		t.Fatal(err)
	}
	resumeRetryBody := fmt.Sprintf(`{"operation_key":"resume-budget-retry","expected_run_version":"%d"}`, retryPending.Version)
	if response := doRequest(http.MethodPost, "/api/runs/"+retryID+"/resume", resumeRetryBody); response.Code != http.StatusAccepted {
		t.Fatalf("budget retry resume: HTTP %d %s", response.Code, response.Body.String())
	}
	awaitManagerEmpty(t, server.manager)
	budgetReader, ok := server.app.Runtime.(interface {
		BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
	})
	if !ok {
		t.Fatal("runtime does not expose authoritative budget snapshots")
	}
	retryBudget, err := budgetReader.BudgetSnapshot(context.Background(), domain.RunID(retryID))
	if err != nil {
		t.Fatal(err)
	}
	if retryBudget.Limits.MaxSimilarityCostMicroUSD != 250000 {
		t.Fatalf("Web resume did not apply the approved budget: %+v", retryBudget.Limits)
	}
	// A separate task can be cancelled while waiting for review, then retried safely.
	response = doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "http-create-replay-review", "cancel-fixture", 1))
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	cancelID := result.Data.RunID
	awaitManagerEmpty(t, server.manager)
	cancelRun, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(cancelID))
	if err != nil {
		t.Fatal(err)
	}
	cancelBody := fmt.Sprintf(`{"operation_key":"cancel-once","expected_run_version":"%d","reason":"test complete"}`, cancelRun.Version)
	for i := 0; i < 2; i++ {
		response = doRequest(http.MethodPost, "/api/runs/"+cancelID+"/cancel", cancelBody)
		if response.Code != 202 {
			t.Fatalf("cancel: %d %s", response.Code, response.Body.String())
		}
		awaitManagerEmpty(t, server.manager)
	}
	cancelled, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(cancelID))
	if err != nil || cancelled.State != domain.RunCancelled {
		t.Fatalf("cancelled: %+v %v", cancelled, err)
	}
}

func TestHTTPRetryReviewPersistsBudgetGrantWithoutApplyingItEarly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(stateRoot) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(effective.Paths.Locks, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, sqlite.Config{Path: effective.Paths.Database, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locks, err := runlock.NewManager(effective.Paths.Locks, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{app: &application.Application{Runtime: store, Reviews: store, Locks: locks}, cfg: cfg, manager: newTaskManager(1), secret: "web-review-test", sessions: map[string]time.Time{}}
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	sessionBody, _ := json.Marshal(map[string]string{"token": server.secret})
	exchange := httptest.NewRecorder()
	handler.ServeHTTP(exchange, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(sessionBody)))
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange: HTTP %d %s", exchange.Code, exchange.Body.String())
	}
	cookies := exchange.Result().Cookies()
	doRequest := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CPGen-CSRF", "local-session")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	runID := domain.RunID("run_000000000000000000000000000000b1")
	configJSON := []byte(`{}`)
	configDigest := domain.SumBytes(configJSON)
	limits := domain.BudgetLimits{MaxSimilarityCostMicroUSD: 7000, MaxActiveTimeMilliseconds: 30000}
	submitted := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "HTTP review test", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}
	raw, err := json.Marshal(submitted)
	if err != nil {
		t.Fatal(err)
	}
	var canonicalValue any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&canonicalValue); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(canonicalValue)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateRun(ctx, domain.CreateRunRequest{RunID: runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw), EffectiveSeed: 7,
		RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: configDigest, WorkflowRevision: "slice1/v1", SchemaVersion: domain.RequestSchemaV1,
		WorkflowDigest: domain.SumBytes([]byte("workflow")), BudgetLimits: limits, StageSequence: []domain.StageName{"prepare", "exercise"}, CreatedAt: createdAt,
		IdempotencyKey: "create_000000000000000000000000000000b1"}); err != nil {
		t.Fatalf("CreateRun fixture: %v", err)
	}
	input := domain.SumBytes([]byte("HTTP review input"))
	evidence := domain.SumBytes([]byte("HTTP review evidence"))
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000b1")
	if _, err := store.BeginStage(ctx, domain.BeginStageCommand{RunID: runID, ExpectedRunVersion: 1, StageName: "prepare", AttemptID: attemptID, InputDigest: input, IdempotencyKey: "begin_000000000000000000000000000000b1", At: createdAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &configDigest, IdempotencyKey: "finish_000000000000000000000000000000b1", At: createdAt.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	reviewBody := fmt.Sprintf(`{"operation_key":"retry-review","expected_run_version":"%d","workflow_revision":%q,"kind":"RETRY","reviewer":"web-test","reason":"continue with approved headroom","budget_increase":{"max_similarity_cost_usd":"0.25"}}`, snapshot.Version, snapshot.WorkflowRevision)
	response := doRequest(http.MethodPost, "/api/runs/"+string(runID)+"/review", reviewBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("retry review: HTTP %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			Decision struct {
				ID             string `json:"id"`
				State          string `json:"state"`
				BudgetIncrease struct {
					MaxSimilarityCostMicroUSD string `json:"max_similarity_cost_micro_usd"`
				} `json:"budget_increase"`
			} `json:"decision"`
			RequiresExplicitResume bool `json:"requires_explicit_resume"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Decision.State != string(domain.ReviewPending) || result.Data.Decision.BudgetIncrease.MaxSimilarityCostMicroUSD != "250000" || !result.Data.RequiresExplicitResume {
		t.Fatalf("HTTP review did not persist exact approved amount: %+v", result.Data)
	}
	pending, err := store.PendingReview(ctx, runID)
	if err != nil || pending == nil || string(pending.ID) != result.Data.Decision.ID {
		t.Fatalf("pending review = %+v, %v", pending, err)
	}
	budget, err := store.BudgetSnapshot(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Limits.MaxSimilarityCostMicroUSD != 7000 {
		t.Fatalf("HTTP review changed allowance before explicit resume: %d", budget.Limits.MaxSimilarityCostMicroUSD)
	}
}
