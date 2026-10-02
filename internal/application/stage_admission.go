package application

import (
	"context"
	"errors"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

// StageAdmission validates the active run, frozen policy, budget and current attempt.
type StageAdmissionStore interface {
	BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	ReadGenerationSnapshot(context.Context, domain.RunID) (domain.GenerationRequestSnapshotV1, error)
	CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
}

type StageAdmission struct {
	store  StageAdmissionStore
	policy domain.Digest
}

func (s *StageAdmission) admit(ctx context.Context, view domain.RunView, stage domain.StageName, input domain.Digest) (domain.StageAttempt, error) {
	var empty domain.StageAttempt
	if ctx == nil {
		return empty, errors.New("generation execution requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	compatible := workflow.SupportsGeneration(view.WorkflowRevision())
	if !compatible || view.SchemaVersion() != domain.RequestSchemaV1 || view.State() != domain.RunRunning || view.CurrentStage() != stage || view.AttemptID().Validate() != nil {
		return empty, errors.New("generation requires the exact current supported stage view")
	}
	current, err := s.store.GetRun(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if current.State != domain.RunRunning || current.CurrentStage != stage || current.WorkflowRevision != view.WorkflowRevision() || current.SchemaVersion != view.SchemaVersion() || current.RequestDigest != view.RequestDigest() || current.ConfigDigest != view.ConfigDigest() || current.WorkflowDigest != view.WorkflowDigest() || current.WorkflowDigest != domain.SumBytes([]byte(view.WorkflowRevision())) || current.Version < view.Version() || current.ActiveStartedAt == nil || current.ConfigDigest != s.policy {
		return empty, errors.New("generation view or frozen provider configuration differs from the active run")
	}
	snapshot, err := s.store.ReadGenerationSnapshot(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	budget, err := s.store.BudgetSnapshot(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if budget.Limits != view.Budget().Limits || !budgetLimitsAtLeast(budget.Limits, snapshot.Request.BudgetLimits) {
		return empty, errors.New("generation view budget differs from the persisted approved budget")
	}
	attempt, err := s.store.CurrentStageAttempt(ctx, view.RunID(), stage)
	if err != nil {
		return empty, err
	}
	if attempt.AttemptID != view.AttemptID() || attempt.State != domain.StageAttemptRunning || attempt.InputDigest != input {
		return empty, errors.New("generation input or attempt differs from the current stage")
	}
	return attempt, nil
}

// Reviews may increase individual run limits, while the originally submitted
// request remains immutable. The runtime snapshot is authoritative for the
// currently approved limits; they may only stay equal to or exceed the request.
func budgetLimitsAtLeast(effective, submitted domain.BudgetLimits) bool {
	return effective.MaxLLMCalls >= submitted.MaxLLMCalls &&
		effective.MaxSimilarityCalls >= submitted.MaxSimilarityCalls &&
		effective.MaxLLMInputTokens >= submitted.MaxLLMInputTokens &&
		effective.MaxLLMOutputTokens >= submitted.MaxLLMOutputTokens &&
		effective.MaxLLMCostMicroUSD >= submitted.MaxLLMCostMicroUSD &&
		effective.MaxSimilarityCostMicroUSD >= submitted.MaxSimilarityCostMicroUSD &&
		effective.MaxSandboxCreates >= submitted.MaxSandboxCreates &&
		effective.MaxArtifactBytes >= submitted.MaxArtifactBytes &&
		effective.MaxPackageBytes >= submitted.MaxPackageBytes &&
		effective.MaxMutationsPerStage >= submitted.MaxMutationsPerStage &&
		effective.MaxActiveTimeMilliseconds >= submitted.MaxActiveTimeMilliseconds
}

func (s *StageAdmission) reconciliationAttempt(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, *domain.StageAttempt, error) {
	var empty domain.RunSnapshot
	if ctx == nil {
		return empty, nil, errors.New("stage reconciliation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, nil, err
	}
	current, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return empty, nil, err
	}
	if !workflow.SupportsGeneration(current.WorkflowRevision) {
		return current, nil, errors.New("cleanup workflow revision differs")
	}
	if current.WorkflowDigest != domain.SumBytes([]byte(current.WorkflowRevision)) || current.SchemaVersion != domain.RequestSchemaV1 || current.ConfigDigest != s.policy {
		return current, nil, errors.New("cleanup frozen configuration or schema differs")
	}
	if current.State != domain.RunRunning {
		return current, nil, nil
	}
	attempt, err := s.store.CurrentStageAttempt(ctx, runID, current.CurrentStage)
	if errors.Is(err, sqlite.ErrNotFound) {
		return current, nil, nil
	}
	if err != nil {
		return current, nil, err
	}
	if attempt.State != domain.StageAttemptRunning {
		return current, nil, nil
	}
	if attempt.RunID != runID || attempt.StageName != current.CurrentStage {
		return current, nil, errors.New("cleanup attempt scope differs")
	}
	return current, &attempt, nil
}
