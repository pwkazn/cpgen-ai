package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestDraftRetryFeedbackHistoryIsBoundToAttemptStart(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "feedback.db"), clock.NewFake(testNow))
	create := testCreateRunRequest(testRunID, testNow, time.Minute)
	create.WorkflowRevision = workflow.ExecutedSamplesRevision
	create.WorkflowDigest = domain.SumBytes([]byte(create.WorkflowRevision))
	definition, err := workflow.DefinitionFor(create.WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	create.StageSequence = definition.Stages()
	if _, err := store.CreateRun(ctx, create); err != nil {
		t.Fatal(err)
	}

	autoAttempt := domain.AttemptID("attempt_00000000000000000000000000000051")
	stageInput := domain.SumBytes([]byte("historical judge input"))
	evidence := domain.SumBytes([]byte("automatic retry evidence"))
	policy := domain.SumBytes([]byte("frozen policy"))
	config := domain.SumBytes([]byte("frozen config"))
	autoAt := testNow.Add(4 * time.Second)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO stage_attempts(attempt_id,run_id,stage_name,ordinal,state,input_digest,started_at,finished_at)
		VALUES(?,?,?,1,'NEEDS_REVIEW',?,?,?)`, autoAttempt, testRunID, "judge", stageInput, formatTime(testNow.Add(3*time.Second)), formatTime(autoAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO content_retries(run_id,ordinal,attempt_id,source_stage,target_stage,reason,evidence_digest,policy_digest,config_digest,created_at)
		VALUES(?,1,?,'judge','statement',?,?,?,?,?)`, testRunID, autoAttempt, "solution_requires_review:sample.3.differential.WA", evidence, policy, config, formatTime(autoAt)); err != nil {
		t.Fatal(err)
	}

	reviewID := domain.ReviewDecisionID("review_00000000000000000000000000000052")
	reviewCreatedAt := testNow.Add(8 * time.Second)
	reviewAppliedAt := testNow.Add(10 * time.Second)
	edits := domain.SumBytes([]byte("reduce generated output safely"))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO review_decisions(
		review_id,run_id,kind,state,expected_run_version,run_version,workflow_revision,stage_name,stage_input_digest,evidence_digest,policy_digest,
		requested_edits_digest,waiver_scope_digest,external_condition_digest,budget_increase_json,waivable_gate,reviewer,reason,idempotency_key,command_digest,created_at,applied_at,revision_target_stage)
		VALUES(?,?,'REVISE','APPLIED',1,2,?,'judge',?,?,?,?,NULL,NULL,NULL,0,'fixture',?,'review_feedback_fixture',?,?,?,'data')`,
		reviewID, testRunID, create.WorkflowRevision, stageInput, evidence, policy, edits,
		"generated.4.run.1.OLE: retain output headroom", domain.SumBytes([]byte("review command")),
		formatTime(reviewCreatedAt), formatTime(reviewAppliedAt)); err != nil {
		t.Fatal(err)
	}

	statementAttemptAt := testNow.Add(5 * time.Second)
	statementFeedback, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "statement", statementAttemptAt)
	if err != nil || statementFeedback == nil || statementFeedback.SourceStage != "judge" || statementFeedback.TargetStage != "statement" || statementFeedback.Reason != "solution_requires_review:sample.3.differential.WA" {
		t.Fatalf("historical statement feedback = %+v, %v", statementFeedback, err)
	}
	beforeApply, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "statement", testNow.Add(9*time.Second))
	if err != nil || beforeApply == nil || *beforeApply != *statementFeedback {
		t.Fatalf("review creation incorrectly changed active feedback before Apply: %+v, %v", beforeApply, err)
	}
	afterLaterRevision, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "statement", testNow.Add(11*time.Second))
	if err != nil || afterLaterRevision != nil {
		t.Fatalf("later data revision failed to suppress stale statement feedback for a new attempt: %+v, %v", afterLaterRevision, err)
	}
	dataFeedback, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "data", reviewAppliedAt)
	if err != nil || dataFeedback == nil || dataFeedback.SourceStage != "judge" || dataFeedback.TargetStage != "data" || dataFeedback.Reason != "generated.4.run.1.OLE: retain output headroom" {
		t.Fatalf("data revision feedback at its apply time = %+v, %v", dataFeedback, err)
	}
	replayed, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "statement", statementAttemptAt)
	if err != nil || replayed == nil || *replayed != *statementFeedback {
		t.Fatalf("repeated historical lookup changed result: %+v, %v", replayed, err)
	}
	if _, err := store.ReadDraftRetryFeedbackBefore(ctx, testRunID, "statement", time.Time{}); err == nil {
		t.Fatal("zero attempt start time was accepted")
	}
}
