package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

func (s *GenerationExecutor) draftCall(runID domain.RunID, version int64, attempt domain.StageAttempt, variables []byte) (domain.OpenCallRequest, port.GenerateRequest, error) {
	prompt, schema, err := BuildLLMDraftPrompt(string(attempt.StageName))
	if err != nil {
		return domain.OpenCallRequest{}, port.GenerateRequest{}, err
	}
	logical := coordinatorMutationID("generation", runID, attempt.AttemptID, attempt.StageName, "draft/v1")
	request := port.GenerateRequest{Prompt: prompt, Schema: schema, Variables: append(json.RawMessage(nil), variables...), Sampling: s.config.Content.Sampling, MaxOutput: s.config.Content.MaxOutput, LogicalIdempotencyKey: logical, ProviderPolicyDigest: s.config.Content.ProviderPolicyDigest, PrivacyClassification: "private"}
	plan, err := s.config.LLM.PlanGenerate(request)
	if err != nil {
		return domain.OpenCallRequest{}, request, err
	}
	open := domain.OpenCallRequest{ID: domain.CallRecordID(coordinatorMutationID("callrec", logical)), RunID: runID, ExpectedRunVersion: version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, LogicalOperationID: logical, Kind: domain.CallLLMGenerate, Provider: plan.Provider, RequestDigest: plan.RequestDigest, PolicyDigest: request.ProviderPolicyDigest, RetryPolicy: s.config.RetryPolicy, IdempotencyKey: coordinatorMutationID("open", logical), At: attempt.StartedAt}
	return open, request, nil
}

// reconciliationAttempt deliberately does not require an active interval:
// settlement is required after cancellation or budget accounting has stopped.
// New generation remains guarded by admit and the durable dispatch ledger.
func (s *GenerationExecutor) reconciliationAttempt(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, *domain.StageAttempt, error) {
	var empty domain.RunSnapshot
	if ctx == nil {
		return empty, nil, errors.New("stage reconciliation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, nil, err
	}
	current, err := s.config.Store.GetRun(ctx, runID)
	if err != nil {
		return empty, nil, err
	}
	if current.WorkflowRevision != workflow.Slice2WorkflowRevision && current.WorkflowRevision != workflow.Slice2CheckpointWorkflowRevision && !workflow.HasSolutionStages(current.WorkflowRevision) {
		return current, nil, errors.New("cleanup workflow revision differs")
	}
	if current.WorkflowDigest != domain.SumBytes([]byte(current.WorkflowRevision)) || current.SchemaVersion != domain.RequestSchemaV1 || current.ConfigDigest != s.config.Content.ProviderPolicyDigest {
		return current, nil, errors.New("cleanup frozen configuration or schema differs")
	}
	if current.State != domain.RunRunning {
		return current, nil, nil
	}
	attempt, err := s.config.Store.CurrentStageAttempt(ctx, runID, current.CurrentStage)
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

// ReconcileStage reconstructs the exact current original request and restores
// only already-opened original/repair operations. It never checks the cache for
// a new hit, starts a provider exchange or creates a missing operation.
func (s *GenerationExecutor) ReconcileStage(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	var variables []byte
	var digest domain.Digest
	switch current.CurrentStage {
	case "idea":
		snapshot, err := s.config.Store.ReadGenerationSnapshot(ctx, runID)
		if err != nil {
			return err
		}
		input, err := domain.NewIdeaDraftInput(snapshot, s.config.Content.IdeaCount)
		if err != nil {
			return err
		}
		variables, err = input.CanonicalJSON()
		if err != nil {
			return err
		}
		digest = snapshot.SnapshotDigest
	case "statement":
		idea, err := s.Reader().ReadIdea(ctx, runID)
		if err != nil {
			return err
		}
		input, err := domain.NewStatementDraftInput(idea.StatementInput, idea.Snapshot, idea.Batch, idea.Selection)
		if err != nil {
			return err
		}
		variables, err = input.CanonicalJSON()
		if err != nil {
			return err
		}
		digest, err = idea.StatementInput.Digest()
		if err != nil {
			return err
		}
	default:
		return errors.New("generation cleanup requires Idea or Statement")
	}
	if attempt.InputDigest != digest {
		return errors.New("cleanup input differs from verified content")
	}
	return s.reconcileDraftRequest(ctx, current, *attempt, variables)
}

func (s *GenerationExecutor) reconcileDraftRequest(ctx context.Context, current domain.RunSnapshot, attempt domain.StageAttempt, variables []byte) error {
	runID := current.RunID
	open, request, err := s.draftCall(runID, current.Version, attempt, variables)
	if err != nil {
		return err
	}
	existing, err := s.config.Store.ReadLogicalCall(ctx, open.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	ledger, err := NewRunBoundLLMLedger(s.config.Store, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	calls, err := s.calls(ledger, attempt.StageName)
	if err != nil {
		return err
	}
	if existing.Kind == domain.CallCacheReuse {
		// Cache admission is atomic and has no partially dispatched state. Check
		// its exact immutable identity without performing another cache lookup.
		cache, err := NewStructuredLLMCache(calls, s.config.Store, s.config.Locks)
		if err != nil {
			return err
		}
		bound, _, key, _, err := cache.identity(open, request)
		if err != nil {
			return err
		}
		if existing.State != domain.CallRecordTerminal || existing.Failure != nil || existing.RunID != runID || existing.StageName != attempt.StageName || existing.AttemptID != attempt.AttemptID || existing.Provider != "private-llm-cache" || existing.RequestDigest != key.Digest || existing.PolicyDigest != bound.PolicyDigest || existing.LogicalOperationID != bound.LogicalOperationID {
			return errors.New("cleanup cache operation differs from its admitted identity")
		}
		return nil
	}
	_, err = calls.Reconcile(ctx, open, request)
	return err
}
