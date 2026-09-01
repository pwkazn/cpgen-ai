package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cpgen/internal/domain"
)

// TestWorkflowEnumsRejectUnknownJSON catches opening a persisted lifecycle enum to
// arbitrary strings, which would let incompatible database state enter the workflow.
func TestWorkflowEnumsRejectUnknownJSON(t *testing.T) {
	t.Parallel()
	for _, target := range []any{
		new(domain.RunState), new(domain.StageState), new(domain.StageAttemptState),
		new(domain.ReviewDecisionKind), new(domain.ReviewDecisionState),
	} {
		if err := json.Unmarshal([]byte(`"UNKNOWN"`), target); err == nil {
			t.Fatalf("%T accepted an unknown lifecycle value", target)
		}
	}
}

// TestWorkflowEnumsAcceptDocumentedValues catches an enum implementation that
// accidentally rejects a valid persisted lifecycle value.
func TestWorkflowEnumsAcceptDocumentedValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		json string
		dst  any
	}{
		{"run", `"READY"`, new(domain.RunState)},
		{"stage", `"SUCCEEDED"`, new(domain.StageState)},
		{"attempt", `"INTERRUPTED"`, new(domain.StageAttemptState)},
		{"review kind", `"WAIVE"`, new(domain.ReviewDecisionKind)},
		{"review state", `"STALE"`, new(domain.ReviewDecisionState)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.json), tc.dst); err != nil {
				t.Fatalf("decode %s: %v", tc.name, err)
			}
		})
	}
}

// TestWorkflowStageBoundaryRejectsReady catches treating READY as a normal stage
// boundary before the package-verification slice can enforce its atomic binding.
func TestWorkflowStageBoundaryRejectsReady(t *testing.T) {
	t.Parallel()
	if err := domain.ValidateStageBoundary(domain.RunReady); !errors.Is(err, domain.ErrStageBoundary) {
		t.Fatalf("ValidateStageBoundary(READY) = %v, want ErrStageBoundary", err)
	}
	if err := domain.ValidateStageBoundary(domain.RunRunning); err != nil {
		t.Fatalf("ValidateStageBoundary(RUNNING): %v", err)
	}
}

// TestLifecycleValuesValidatePersistentInvariants catches accepting malformed
// projections that would make versioned restart and audit state ambiguous.
func TestLifecycleValuesValidatePersistentInvariants(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("input"))
	runID := domain.RunID("run_0123456789abcdef0123456789abcdef")
	request := domain.CreateRunRequest{
		RunID:                         runID,
		SubmittedRequestJSON:          []byte(`{"schema":"cpgen.request/v1"}`),
		SubmittedRequestDigest:        domain.SumBytes([]byte(`{"schema":"cpgen.request/v1"}`)),
		EffectiveSeed:                 1,
		RedactedEffectiveConfigJSON:   []byte(`{"schema":"cpgen.config/v1"}`),
		RedactedEffectiveConfigDigest: domain.SumBytes([]byte(`{"schema":"cpgen.config/v1"}`)),
		WorkflowDigest:                digest,
		BudgetLimits:                  domain.BudgetLimits{MaxLLMCalls: 1, MaxActiveTimeMilliseconds: 1},
		StageSequence:                 []domain.StageName{"idea"},
		CreatedAt:                     now,
		IdempotencyKey:                "create_0123456789abcdef0123456789abcdef",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid create request rejected: %v", err)
	}

	for _, tc := range []struct {
		name  string
		value any
	}{
		{"zero run version", domain.RunSnapshot{RunID: runID, State: domain.RunCreated, Version: 0, CreatedAt: now, UpdatedAt: now}},
		{"zero stage ordinal", domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 0, State: domain.StagePending, Version: 1, InputDigest: digest, CreatedAt: now, UpdatedAt: now}},
		{"zero attempt ordinal", domain.StageAttempt{AttemptID: domain.AttemptID("attempt_0123456789abcdef0123456789abcdef"), RunID: runID, StageName: "idea", Ordinal: 0, State: domain.StageAttemptRunning, StartedAt: now}},
		{"non-UTC checkpoint", domain.BlockedCheckpoint{RunID: runID, StageName: "idea", StageInputDigest: digest, DependencyID: "provider", DependencyDigest: digest, PolicyDigest: digest, ErrorDigest: digest, RetryAfter: now.In(time.FixedZone("offset", 3600)), CreatedAt: now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			switch value := tc.value.(type) {
			case domain.RunSnapshot:
				err = value.Validate()
			case domain.StageSnapshot:
				err = value.Validate()
			case domain.StageAttempt:
				err = value.Validate()
			case domain.BlockedCheckpoint:
				err = value.Validate()
			}
			if err == nil {
				t.Fatal("malformed persistent value was accepted")
			}
		})
	}
}

// TestLifecycleUsesOnlyClosedExecutionCauses catches reintroducing obsolete
// ownership modes as interruption causes.
func TestLifecycleUsesOnlyClosedExecutionCauses(t *testing.T) {
	t.Parallel()
	for _, cause := range []domain.ExecutionCause{
		domain.CauseUserCancel, domain.CauseRevisionInvalidated,
		domain.CauseStepDeadline, domain.CauseRunBudgetDeadline,
	} {
		if err := (domain.ExecutionInterrupted{Cause: cause}).Validate(); err != nil {
			t.Fatalf("documented cause %q rejected: %v", cause, err)
		}
	}
	for _, cause := range []domain.ExecutionCause{"quiesce", "lease_lost"} {
		if err := (domain.ExecutionInterrupted{Cause: cause}).Validate(); err == nil {
			t.Fatalf("obsolete cause %q accepted", cause)
		}
	}
}

// TestLifecycleStateFieldMatrices catches accepting stage and attempt projections
// whose fields contradict their durable lifecycle state.
func TestLifecycleStateFieldMatrices(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("state matrix"))
	runID := domain.RunID("run_0123456789abcdef0123456789abcdef")
	attemptID := domain.AttemptID("attempt_0123456789abcdef0123456789abcdef")
	for _, tc := range []struct {
		name  string
		valid bool
		stage domain.StageSnapshot
	}{
		{"pending", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StagePending, Version: 1, InputDigest: digest, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"running", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageRunning, Version: 1, InputDigest: digest, AttemptCount: 1, CurrentAttemptID: &attemptID, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"succeeded", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageSucceeded, Version: 1, InputDigest: digest, OutputDigest: &digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"blocked", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageBlocked, Version: 1, InputDigest: digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"needs review", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageNeedsReview, Version: 1, InputDigest: digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"failed", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageFailed, Version: 1, InputDigest: digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"cancelled before first attempt", true, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageCancelled, Version: 1, InputDigest: digest, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"pending rejects current attempt", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StagePending, Version: 1, InputDigest: digest, CurrentAttemptID: &attemptID, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"running rejects output", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageRunning, Version: 1, InputDigest: digest, OutputDigest: &digest, AttemptCount: 1, CurrentAttemptID: &attemptID, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"succeeded rejects current attempt", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageSucceeded, Version: 1, InputDigest: digest, OutputDigest: &digest, AttemptCount: 1, CurrentAttemptID: &attemptID, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"blocked rejects output", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageBlocked, Version: 1, InputDigest: digest, OutputDigest: &digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"needs review rejects output", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageNeedsReview, Version: 1, InputDigest: digest, OutputDigest: &digest, AttemptCount: 1, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"failed rejects current attempt", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageFailed, Version: 1, InputDigest: digest, AttemptCount: 1, CurrentAttemptID: &attemptID, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
		{"cancelled rejects output", false, domain.StageSnapshot{RunID: runID, Name: "idea", Ordinal: 1, State: domain.StageCancelled, Version: 1, InputDigest: digest, OutputDigest: &digest, LogicalIdempotencyKey: "stage_0123456789abcdef0123456789abcdef", CreatedAt: now, UpdatedAt: now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.stage.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid=%v", err, tc.valid)
			}
		})
	}
	cause := domain.CauseUserCancel
	for _, tc := range []struct {
		name  string
		valid bool
		value domain.StageAttempt
	}{
		{"running", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptRunning, InputDigest: digest, StartedAt: now}},
		{"succeeded", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptSucceeded, InputDigest: digest, OutputDigest: &digest, StartedAt: now, FinishedAt: &now}},
		{"blocked", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptBlocked, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"needs review", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptNeedsReview, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"failed", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptFailed, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"cancelled", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptCancelled, InputDigest: digest, Cause: &cause, StartedAt: now, FinishedAt: &now}},
		{"interrupted", true, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptInterrupted, InputDigest: digest, Cause: &cause, StartedAt: now, FinishedAt: &now}},
		{"running rejects finish time", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptRunning, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"succeeded requires output", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptSucceeded, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"blocked rejects output", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptBlocked, InputDigest: digest, OutputDigest: &digest, StartedAt: now, FinishedAt: &now}},
		{"needs review rejects cause", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptNeedsReview, InputDigest: digest, Cause: &cause, StartedAt: now, FinishedAt: &now}},
		{"failed rejects cause", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptFailed, InputDigest: digest, Cause: &cause, StartedAt: now, FinishedAt: &now}},
		{"cancelled requires cause", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptCancelled, InputDigest: digest, StartedAt: now, FinishedAt: &now}},
		{"interrupted rejects output", false, domain.StageAttempt{AttemptID: attemptID, RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptInterrupted, InputDigest: digest, Cause: &cause, OutputDigest: &digest, StartedAt: now, FinishedAt: &now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.value.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid=%v", err, tc.valid)
			}
		})
	}
}

// TestLifecycleRequiresDurableDigests catches accepting an otherwise-valid
// projection that is detached from immutable request/config/workflow or input bytes.
func TestLifecycleRequiresDurableDigests(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("durable binding"))
	runID := domain.RunID("run_0123456789abcdef0123456789abcdef")
	for _, tc := range []struct {
		name  string
		value domain.RunSnapshot
	}{
		{"request digest", domain.RunSnapshot{RunID: runID, State: domain.RunCreated, Version: 1, RequestDigest: "", ConfigDigest: digest, WorkflowDigest: digest, CreatedAt: now, UpdatedAt: now}},
		{"config digest", domain.RunSnapshot{RunID: runID, State: domain.RunCreated, Version: 1, RequestDigest: digest, ConfigDigest: "", WorkflowDigest: digest, CreatedAt: now, UpdatedAt: now}},
		{"workflow digest", domain.RunSnapshot{RunID: runID, State: domain.RunCreated, Version: 1, RequestDigest: digest, ConfigDigest: digest, WorkflowDigest: "", CreatedAt: now, UpdatedAt: now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.value.Validate(); err == nil {
				t.Fatal("zero durable digest was accepted")
			}
		})
	}
	attempt := domain.StageAttempt{AttemptID: domain.AttemptID("attempt_0123456789abcdef0123456789abcdef"), RunID: runID, StageName: "idea", Ordinal: 1, State: domain.StageAttemptRunning, StartedAt: now}
	if err := attempt.Validate(); err == nil {
		t.Fatal("zero attempt input digest was accepted")
	}
}

// TestTransitionMatricesAreClosed catches adding an undocumented lifecycle edge
// or removing one of the fixed local resume/review/cancel edges.
func TestTransitionMatricesAreClosed(t *testing.T) {
	t.Parallel()
	runStates := []domain.RunState{
		domain.RunCreated, domain.RunRunning, domain.RunBlocked, domain.RunNeedsReview,
		domain.RunReady, domain.RunFailed, domain.RunCancelled,
	}
	legalRuns := map[[2]domain.RunState]bool{
		{domain.RunCreated, domain.RunRunning}:       true,
		{domain.RunCreated, domain.RunCancelled}:     true,
		{domain.RunRunning, domain.RunRunning}:       true,
		{domain.RunRunning, domain.RunBlocked}:       true,
		{domain.RunRunning, domain.RunNeedsReview}:   true,
		{domain.RunRunning, domain.RunFailed}:        true,
		{domain.RunRunning, domain.RunCancelled}:     true,
		{domain.RunBlocked, domain.RunRunning}:       true,
		{domain.RunBlocked, domain.RunCancelled}:     true,
		{domain.RunNeedsReview, domain.RunCreated}:   true,
		{domain.RunNeedsReview, domain.RunFailed}:    true,
		{domain.RunNeedsReview, domain.RunCancelled}: true,
	}
	for _, from := range runStates {
		for _, to := range runStates {
			err := domain.ValidateRunTransition(from, to)
			if legalRuns[[2]domain.RunState{from, to}] != (err == nil) {
				t.Fatalf("run transition %s -> %s = %v, legal=%v", from, to, err, legalRuns[[2]domain.RunState{from, to}])
			}
			if to == domain.RunReady && !errors.Is(err, domain.ErrStageBoundary) {
				t.Fatalf("run transition %s -> READY = %v, want ErrStageBoundary", from, err)
			}
		}
	}

	stageStates := []domain.StageState{
		domain.StagePending, domain.StageRunning, domain.StageSucceeded, domain.StageBlocked,
		domain.StageNeedsReview, domain.StageFailed, domain.StageCancelled,
	}
	legalStages := map[[2]domain.StageState]bool{
		{domain.StagePending, domain.StageRunning}:       true,
		{domain.StagePending, domain.StageCancelled}:     true,
		{domain.StageRunning, domain.StageSucceeded}:     true,
		{domain.StageRunning, domain.StageBlocked}:       true,
		{domain.StageRunning, domain.StageNeedsReview}:   true,
		{domain.StageRunning, domain.StageFailed}:        true,
		{domain.StageRunning, domain.StageCancelled}:     true,
		{domain.StageBlocked, domain.StageRunning}:       true,
		{domain.StageBlocked, domain.StageCancelled}:     true,
		{domain.StageNeedsReview, domain.StagePending}:   true,
		{domain.StageNeedsReview, domain.StageFailed}:    true,
		{domain.StageNeedsReview, domain.StageCancelled}: true,
	}
	for _, from := range stageStates {
		for _, to := range stageStates {
			err := domain.ValidateStageTransition(from, to)
			if legalStages[[2]domain.StageState{from, to}] != (err == nil) {
				t.Fatalf("stage transition %s -> %s = %v, legal=%v", from, to, err, legalStages[[2]domain.StageState{from, to}])
			}
		}
	}
}

// TestRuntimeCommandsRejectMissingStableBindings catches a mutation command
// that can bypass expected-version CAS, stable idempotency, digest binding, or
// canonical UTC event time.
func TestRuntimeCommandsRejectMissingStableBindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	runID := domain.RunID("run_00000000000000000000000000000001")
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000001")
	digest := domain.SumBytes([]byte("binding"))
	valid := domain.BeginStageCommand{
		RunID: runID, ExpectedRunVersion: 1, StageName: "prepare", AttemptID: attemptID,
		InputDigest: digest, IdempotencyKey: "begin_00000000000000000000000000000001", At: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid BeginStageCommand: %v", err)
	}
	for _, mutate := range []func(*domain.BeginStageCommand){
		func(value *domain.BeginStageCommand) { value.ExpectedRunVersion = 0 },
		func(value *domain.BeginStageCommand) { value.InputDigest = "" },
		func(value *domain.BeginStageCommand) { value.IdempotencyKey = "" },
		func(value *domain.BeginStageCommand) { value.At = now.In(time.FixedZone("offset", 3600)) },
	} {
		invalid := valid
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid BeginStageCommand accepted: %+v", invalid)
		}
	}
}

// TestReviewKindPayloadValidation catches content-free REVISE/RETRY/WAIVE
// decisions and a REJECT smuggling revision-only payloads.
func TestReviewKindPayloadValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("binding"))
	base := domain.CreateReviewRequest{
		ID:    "review_00000000000000000000000000000001",
		RunID: "run_00000000000000000000000000000001", ExpectedRunVersion: 3,
		WorkflowRevision: "slice1/v1", StageName: "prepare",
		StageInputDigest: digest, EvidenceDigest: digest, PolicyDigest: digest,
		Reviewer: "reviewer", Reason: "reason",
		IdempotencyKey: "reviewcreate_00000000000000000000000000000001", At: now,
	}
	for _, kind := range []domain.ReviewDecisionKind{domain.ReviewRevise, domain.ReviewRetry, domain.ReviewWaive} {
		value := base
		value.Kind = kind
		if err := value.Validate(); err == nil {
			t.Fatalf("content-free %s review accepted", kind)
		}
	}
	value := base
	value.Kind = domain.ReviewReject
	if err := value.Validate(); err != nil {
		t.Fatalf("plain REJECT rejected: %v", err)
	}
}

// TestActiveTimeCommandRequiresRecoveryBound catches crash accounting that can
// charge unbounded offline time or a non-recovery heartbeat carrying a second
// hidden interval.
func TestActiveTimeCommandRequiresRecoveryBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	base := domain.ActiveTimeCommand{
		RunID: "run_00000000000000000000000000000001", ExpectedRunVersion: 3,
		Action: domain.ActiveTimeRecover, HeartbeatInterval: time.Second,
		IdempotencyKey: "active_00000000000000000000000000000001", At: now,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid recovery command: %v", err)
	}
	invalid := base
	invalid.HeartbeatInterval = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("recovery without heartbeat interval accepted")
	}
	invalid = base
	invalid.Action = domain.ActiveTimeHeartbeat
	if err := invalid.Validate(); err == nil {
		t.Fatal("ordinary heartbeat with recovery interval accepted")
	}
}

// TestApplyReviewRequiresCurrentStageInvalidation catches a REVISE decision
// that would leave the current NEEDS_REVIEW stage untouched and make resume
// impossible despite recording new input and configuration digests.
func TestApplyReviewRequiresCurrentStageInvalidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("review binding"))
	newInput := domain.SumBytes([]byte("revised input"))
	newConfig := domain.SumBytes([]byte("revised config"))
	command := domain.ApplyReviewCommand{
		RunID: "run_00000000000000000000000000000001", ExpectedRunVersion: 3,
		ReviewDecisionID: "review_00000000000000000000000000000001",
		StageName:        "prepare", StageInputDigest: digest, EvidenceDigest: digest, PolicyDigest: digest,
		NewInputDigest: &newInput, NewConfigDigest: &newConfig,
		InvalidatedStages: []domain.StageName{"exercise"},
		IdempotencyKey:    "reviewapply_00000000000000000000000000000001", At: now,
	}
	if err := command.Validate(); err == nil {
		t.Fatal("REVISE application without its current stage invalidated was accepted")
	}
	command.InvalidatedStages = append(command.InvalidatedStages, command.StageName)
	if err := command.Validate(); err != nil {
		t.Fatalf("REVISE application with current stage invalidated: %v", err)
	}
}
