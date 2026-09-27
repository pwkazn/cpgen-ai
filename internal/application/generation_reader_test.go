package application_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

func TestGenerationReaderRebuildsCommittedIdeaAndStatementWithoutHTTP(t *testing.T) {
	f := newGenerationReaderFixture(t)
	reader, err := application.NewGenerationReader(f.store, f.ideaCalls, f.statementCalls, f.options)
	if err != nil {
		t.Fatal(err)
	}
	for read := 0; read < 2; read++ {
		idea, err := reader.ReadIdea(context.Background(), f.runID)
		if err != nil || idea.Snapshot.SnapshotDigest != f.snapshot.SnapshotDigest || idea.Batch.BatchDigest != f.batch.BatchDigest || idea.Selection.SelectionDigest != f.selection.SelectionDigest {
			t.Fatalf("idea=%+v err=%v", idea, err)
		}
		statement, err := reader.ReadStatement(context.Background(), f.runID)
		if err != nil || statement.Problem.SpecDigest != f.problem.SpecDigest || statement.Problem.ValidateChain(f.snapshot, f.batch, f.selection) != nil {
			t.Fatalf("statement=%+v err=%v", statement, err)
		}
		if f.httpCalls.Load() != 4 {
			t.Fatalf("input recovery sent %d provider requests, expected original four", f.httpCalls.Load())
		}
	}
	for _, change := range []func(*application.GenerationReaderOptions){
		func(v *application.GenerationReaderOptions) { v.IdeaCount = 3 },
		func(v *application.GenerationReaderOptions) { v.SelectionPolicy = domain.SelectionIdeaIDPolicyV1 },
		func(v *application.GenerationReaderOptions) {
			v.ProviderPolicyDigest = domain.SumBytes([]byte("changed policy"))
		},
		func(v *application.GenerationReaderOptions) { v.MaxOutput.Tokens++ },
	} {
		options := f.options
		change(&options)
		reader, err := application.NewGenerationReader(f.store, f.ideaCalls, f.statementCalls, options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadIdea(context.Background(), f.runID); err == nil || f.httpCalls.Load() != 4 {
			t.Fatalf("changed committed-input policy accepted or dispatched: %v", err)
		}
	}
	options := f.options
	options.StatementRevision++
	revised, err := application.NewGenerationReader(f.store, f.ideaCalls, f.statementCalls, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revised.ReadStatement(context.Background(), f.runID); err == nil || f.httpCalls.Load() != 4 {
		t.Fatalf("changed statement revision accepted or dispatched: %v", err)
	}
}

func TestGenerationReaderReopensDatabaseWithoutNewProviderWork(t *testing.T) {
	f := newGenerationReaderFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	services := map[string]*durable.StructuredLLMCalls{}
	for _, stage := range []string{"idea", "statement"} {
		calls, err := durable.NewReplayableLLMCalls(store, f.model, f.blobs, f.clock, 100)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := application.BuildFormatRepairPolicy(f.cfg, stage+".draft")
		if err != nil {
			t.Fatal(err)
		}
		services[stage], err = durable.NewStructuredLLMCalls(calls, policy)
		if err != nil {
			t.Fatal(err)
		}
	}
	reader, err := application.NewGenerationReader(store, services["idea"], services["statement"], f.options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.ReadStatement(context.Background(), f.runID)
	if err != nil || result.Problem.SpecDigest != f.problem.SpecDigest || f.httpCalls.Load() != 4 {
		t.Fatalf("reopened result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
	}
}

func TestGenerationReaderRejectsMissingPrivateBytesAndSubstitutedStageProof(t *testing.T) {
	f := newGenerationReaderFixture(t)
	for _, kind := range []string{"input", "output", "source"} {
		store := substitutedGenerationReadStore{GenerationReadStore: f.store, kind: kind}
		reader, err := application.NewGenerationReader(store, f.ideaCalls, f.statementCalls, f.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadIdea(context.Background(), f.runID); err == nil || f.httpCalls.Load() != 4 {
			t.Fatalf("substituted %s proof accepted: %v", kind, err)
		}
	}
	stage, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "idea")
	if err != nil {
		t.Fatal(err)
	}
	ref := stage.Artifacts[0].Blob.Blob
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	if err := os.Remove(filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex)); err != nil {
		t.Fatal(err)
	}
	reader, err := application.NewGenerationReader(f.store, f.ideaCalls, f.statementCalls, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadStatement(context.Background(), f.runID); err == nil || f.httpCalls.Load() != 4 {
		t.Fatalf("missing response was regenerated or accepted: %v", err)
	}
}

type substitutedGenerationReadStore struct {
	application.GenerationReadStore
	kind string
}

func (s substitutedGenerationReadStore) ReadCommittedLLMStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedLLMStage, error) {
	value, err := s.GenerationReadStore.ReadCommittedLLMStage(ctx, runID, stage)
	if err != nil {
		return value, err
	}
	digest := domain.SumBytes([]byte("substituted stage proof"))
	switch s.kind {
	case "input":
		value.Attempt.InputDigest = digest
	case "output":
		value.Attempt.OutputDigest = &digest
	case "source":
		for i := range value.Artifacts {
			value.Artifacts[i].Source.OccurrenceID = "occurrence_00000000000000000000000000009999"
		}
	}
	return value, nil
}

type generationReaderFixture struct {
	coordinatorFixture
	ideaCalls, statementCalls *durable.StructuredLLMCalls
	options                   application.GenerationReaderOptions
	snapshot                  domain.GenerationRequestSnapshotV1
	batch                     domain.IdeaBatch
	selection                 domain.IdeaSelection
	problem                   domain.ProblemSpec
	httpCalls                 *atomic.Int32
	blobs                     *blob.Store
	blobRoot                  string
	cfg                       config.Config
	model                     *agent.LangChain
	ideaOpen                  domain.OpenCallRequest
	ideaRequest               port.GenerateRequest
}

func newGenerationReaderFixture(t *testing.T) generationReaderFixture {
	return newGenerationReaderFixtureWithPolicy(t, false)
}

func newGenerationReaderFixtureWithPolicy(t *testing.T, live bool) generationReaderFixture {
	t.Helper()
	cfg := llmApplicationConfig(t)
	if live {
		cfg = explicitSlice2ApplicationConfig(t)
		cfg.Workflow.IdeaCount = 2
		cfg.LLM.MaxOutputTokens = 512
		cfg.LLM.MaxResponseBytes = 16384
		cfg.LLM.BaseURL = "https://1.1.1.1/v1"
	}
	cfg.LLM.MaxFormatRepairs = 1
	t.Setenv(cfg.LLM.APIKeyEnv, "fixture-key")
	outputs := llmBuiltinOutputs(t)
	httpCalls := new(atomic.Int32)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ordinal := httpCalls.Add(1)
		content := `{"private":"invalid output"}`
		if ordinal == 2 {
			content = string(outputs["idea.draft"])
		}
		if ordinal == 4 {
			content = string(outputs["statement.draft"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "generation-reader", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	if live {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	mapped, _, err := application.BuildLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		mapped.HTTPClient = &http.Client{Transport: transport}
	} else {
		mapped.Endpoint, mapped.AllowInsecureHTTP = server.URL, true
	}
	model, err := agent.NewLangChain(mapped)
	if err != nil {
		t.Fatal(err)
	}
	limits := domain.BudgetLimits{MaxLLMCalls: 4, MaxLLMInputTokens: 500000, MaxLLMOutputTokens: 10000, MaxLLMCostMicroUSD: 1000, MaxArtifactBytes: 1000000, MaxActiveTimeMilliseconds: 60000}
	snapshot, err := domain.NewGenerationRequestSnapshotV1(domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Graphs", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}, 9007199254740993)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "generation.db")
	clock := newRecordingClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	store, err := openFreshApplicationSQLite(t, sqlite.Config{Path: path, BusyTimeout: time.Second, MaxReaders: 4}, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := filepath.Join(t.TempDir(), "private")
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	f := coordinatorFixture{store: store, path: path, clock: clock, runID: "run_00000000000000000000000000001501", attemptID: "attempt_00000000000000000000000000001501"}
	raw, err := snapshot.Request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	configJSON, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateRun(context.Background(), domain.CreateRunRequest{RunID: f.runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: snapshot.RequestDigest, EffectiveSeed: snapshot.EffectiveSeed, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: cfg.EffectiveDigest(), WorkflowRevision: workflow.LegacySimilarityRevision, SchemaVersion: domain.RequestSchemaV1, WorkflowDigest: domain.SumBytes([]byte(workflow.LegacySimilarityRevision)), BudgetLimits: limits, StageSequence: []domain.StageName{"idea", "statement", "similarity"}, CreatedAt: clock.Now(), IdempotencyKey: coordinatorID("create", "generation-reader")})
	if err != nil {
		t.Fatal(err)
	}
	options := application.GenerationReaderOptions{IdeaCount: 2, SelectionPolicy: domain.SelectionOrdinalPolicyV1, StatementRevision: 1, ProviderPolicyDigest: cfg.EffectiveDigest(), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 512, Bytes: 16384}}
	if live {
		options, _, err = application.BuildGenerationExecutionSettings(cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	ideaInput, err := domain.NewIdeaDraftInput(snapshot, options.IdeaCount)
	if err != nil {
		t.Fatal(err)
	}
	var draft domain.IdeaDraftV1
	if err := json.Unmarshal(outputs["idea.draft"], &draft); err != nil {
		t.Fatal(err)
	}
	batch, err := draft.Bind(ideaInput)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := batch.OrderedFeasibleCandidateIDs(options.SelectionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, ids[0], options.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	statementInput := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	statementDigest, err := statementInput.Digest()
	if err != nil {
		t.Fatal(err)
	}
	statementDraftInput, err := domain.NewStatementDraftInput(statementInput, snapshot, batch, selection)
	if err != nil {
		t.Fatal(err)
	}
	var statementDraft domain.StatementDraftV1
	if err := json.Unmarshal(outputs["statement.draft"], &statementDraft); err != nil {
		t.Fatal(err)
	}
	problem, err := statementDraft.Bind(statementDraftInput, options.StatementRevision)
	if err != nil {
		t.Fatal(err)
	}
	services := map[domain.StageName]*durable.StructuredLLMCalls{}
	var ideaOpen domain.OpenCallRequest
	var ideaRequest port.GenerateRequest
	for index, stage := range []domain.StageName{"idea", "statement"} {
		attemptID := f.attemptID
		inputDigest, outputDigest, nextStage, nextInput := snapshot.SnapshotDigest, batch.BatchDigest, domain.StageName("statement"), statementDigest
		variables, err := ideaInput.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			attemptID = "attempt_00000000000000000000000000001502"
			inputDigest, outputDigest, nextStage, nextInput = statementDigest, problem.SpecDigest, "similarity", domain.SumBytes([]byte("similarity input"))
			variables, err = statementDraftInput.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
		}
		version := int64(1 + index*2)
		if _, err := store.BeginStage(context.Background(), domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: version, StageName: stage, AttemptID: attemptID, InputDigest: inputDigest, IdempotencyKey: coordinatorID("begin", string(stage)), At: clock.Now()}); err != nil {
			t.Fatal(err)
		}
		ledger, err := durable.NewRunLedger(store, f.runID, stage, attemptID)
		if err != nil {
			t.Fatal(err)
		}
		calls, err := durable.NewReplayableLLMCalls(ledger, model, blobs, clock, 100)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := application.BuildFormatRepairPolicy(cfg, string(stage)+".draft")
		if err != nil {
			t.Fatal(err)
		}
		service, err := durable.NewStructuredLLMCalls(calls, policy)
		if err != nil {
			t.Fatal(err)
		}
		services[stage] = service
		prompt, schema, err := application.BuildLLMDraftPrompt(string(stage))
		if err != nil {
			t.Fatal(err)
		}
		open := f.openRequest(1501 + index)
		open.StageName, open.AttemptID, open.ExpectedRunVersion, open.RetryPolicy.MaxAttempts = stage, attemptID, version+1, 1
		request := port.GenerateRequest{Prompt: prompt, Schema: schema, Variables: variables, Sampling: options.Sampling, MaxOutput: options.MaxOutput, LogicalIdempotencyKey: open.LogicalOperationID, ProviderPolicyDigest: options.ProviderPolicyDigest, PrivacyClassification: "private"}
		plan, err := model.PlanGenerate(request)
		if err != nil {
			t.Fatal(err)
		}
		open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
		if stage == "idea" {
			ideaOpen, ideaRequest = open, request
		}
		result, err := service.Generate(context.Background(), open, request)
		if err != nil || result.Outcome.Value == nil {
			t.Fatalf("stage %s=%+v err=%v", stage, result, err)
		}
		finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: version + 1, StageName: stage, AttemptID: attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &outputDigest, NextStage: nextStage, NextInputDigest: &nextInput, IdempotencyKey: coordinatorID("finish", string(stage)), At: clock.Now()}
		for i := range result.Artifacts {
			finish.Occurrences = append(finish.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &result.Artifacts[i]})
		}
		if _, err := store.FinishStage(context.Background(), finish); err != nil {
			t.Fatal(err)
		}
	}
	return generationReaderFixture{coordinatorFixture: f, ideaCalls: services["idea"], statementCalls: services["statement"], options: options, snapshot: snapshot, batch: batch, selection: selection, problem: problem, httpCalls: httpCalls, blobs: blobs, blobRoot: root, cfg: cfg, model: model, ideaOpen: ideaOpen, ideaRequest: ideaRequest}
}

func TestGenerationReaderRestoresCurrentCacheUseAfterReviewInvalidation(t *testing.T) {
	f := newGenerationReaderFixture(t)
	ctx := context.Background()
	locks, err := runlock.NewManager(t.TempDir(), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	cache, err := durable.NewStructuredLLMCache(f.ideaCalls, f.store, locks)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cache.Put(ctx, f.ideaOpen, f.ideaRequest)
	if err != nil {
		t.Fatal(err)
	}
	input, err := f.store.ReadStageInputDigest(ctx, f.runID, "similarity")
	if err != nil {
		t.Fatal(err)
	}
	attempt := domain.AttemptID("attempt_00000000000000000000000000001503")
	if _, err := f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: 5, StageName: "similarity", AttemptID: attempt, InputDigest: input, IdempotencyKey: coordinatorID("begin", "review-similarity"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	evidence, policy := domain.SumBytes([]byte("review evidence")), f.options.ProviderPolicyDigest
	reviewRun, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 6, StageName: "similarity", AttemptID: attempt, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy, IdempotencyKey: coordinatorID("finish", "review-similarity"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	edits := domain.SumBytes([]byte("recheck the same admitted content"))
	decision, err := f.store.CreateReview(ctx, domain.CreateReviewRequest{ID: "review_00000000000000000000000000001501", RunID: f.runID, ExpectedRunVersion: reviewRun.Version, Kind: domain.ReviewRevise, WorkflowRevision: workflow.LegacySimilarityRevision, StageName: "similarity", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, RequestedEditsDigest: &edits, Reviewer: "fixture", Reason: "recheck existing input", IdempotencyKey: coordinatorID("review", "reader-invalidation"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	configJSON, err := f.cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	configDigest := f.cfg.EffectiveDigest()
	revised, err := f.store.ApplyReview(ctx, domain.ApplyReviewCommand{RunID: f.runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID, StageName: "similarity", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, NewInputDigest: &f.snapshot.SnapshotDigest, NewConfigJSON: configJSON, NewConfigDigest: &configDigest, InvalidatedStages: []domain.StageName{"idea", "statement", "similarity"}, IdempotencyKey: coordinatorID("apply", "reader-invalidation"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := application.NewGenerationReader(f.store, f.ideaCalls, f.statementCalls, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadIdea(ctx, f.runID); err == nil {
		t.Fatal("review invalidation exposed its previous successful Idea")
	}
	currentAttempt := domain.AttemptID("attempt_00000000000000000000000000001504")
	if _, err := f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: revised.Version, StageName: "idea", AttemptID: currentAttempt, InputDigest: f.snapshot.SnapshotDigest, IdempotencyKey: coordinatorID("begin", "cached-idea"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	ledger, err := durable.NewRunLedger(f.store, f.runID, "idea", currentAttempt)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := application.BuildFormatRepairPolicy(f.cfg, "idea.draft")
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewStructuredLLMCalls(calls, repair)
	if err != nil {
		t.Fatal(err)
	}
	cache, err = durable.NewStructuredLLMCache(service, f.store, locks)
	if err != nil {
		t.Fatal(err)
	}
	open := f.openRequest(1504)
	open.StageName, open.AttemptID, open.ExpectedRunVersion, open.RetryPolicy.MaxAttempts = "idea", currentAttempt, revised.Version+1, 1
	request := f.ideaRequest
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := f.model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	hit, err := cache.Reuse(ctx, open, request)
	if err != nil || !hit.Hit {
		t.Fatalf("cached Idea=%+v err=%v", hit, err)
	}
	statementInput := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: f.snapshot.SnapshotDigest, IdeaBatchDigest: f.batch.BatchDigest, IdeaSelectionDigest: f.selection.SelectionDigest, SelectedIdeaID: f.selection.SelectedIdeaID}
	nextInput, err := statementInput.Digest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: revised.Version + 1, StageName: "idea", AttemptID: currentAttempt, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &f.batch.BatchDigest, NextStage: "statement", NextInputDigest: &nextInput, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &hit.Reuses[0]}}, IdempotencyKey: coordinatorID("finish", "cached-idea"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Invalidate(ctx, key, domain.InvalidationManual); err != nil {
		t.Fatal(err)
	}
	reader, err = application.NewGenerationReader(f.store, service, f.statementCalls, f.options)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reader.ReadIdea(ctx, f.runID)
	if err != nil || restored.Batch.BatchDigest != f.batch.BatchDigest || restored.Selection.SelectionDigest != f.selection.SelectionDigest || f.httpCalls.Load() != 4 {
		t.Fatalf("current cache provenance=%+v err=%v HTTP=%d", restored, err, f.httpCalls.Load())
	}
	if _, err := reader.ReadStatement(ctx, f.runID); err == nil {
		t.Fatal("old invalidated Statement was restored before a new commit")
	}
}
