package application

import (
	"context"
	"errors"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
)

// ReconcileStage restores the exact existing Similarity operation. Its input
// still comes from verified committed Statement content, even after active
// accounting is closed. Missing operations remain absent.
func (s *SimilarityExecutor) ReconcileStage(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.config.Generation.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != "similarity" {
		return errors.New("similarity cleanup requires its current stage")
	}
	statement, input, err := s.readInput(ctx, runID)
	if err != nil {
		return err
	}
	digest, err := input.Digest()
	if err != nil {
		return err
	}
	if digest != attempt.InputDigest {
		return errors.New("similarity cleanup input differs")
	}
	request, err := s.attemptRequest(runID, *attempt, statement.Problem, input)
	if err != nil {
		return err
	}
	plan, err := s.config.Provider.PlanSearch(request)
	if err != nil {
		return err
	}
	if plan.PolicyDigest != input.ProviderPolicyDigest {
		return errors.New("similarity cleanup provider policy differs")
	}
	logical := request.LogicalIdempotencyKey
	open := domain.OpenCallRequest{ID: domain.CallRecordID(coordinatorMutationID("callrec", logical)), RunID: runID, ExpectedRunVersion: current.Version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, LogicalOperationID: logical, Kind: domain.CallSimilaritySearch, Provider: plan.Provider, RequestDigest: plan.RequestDigest, PolicyDigest: plan.PolicyDigest, RetryPolicy: s.config.RetryPolicy, IdempotencyKey: coordinatorMutationID("open", logical), At: attempt.StartedAt}
	generation := s.config.Generation.config
	if _, err := generation.Store.ReadLogicalCall(ctx, open.ID); errors.Is(err, sqlite.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	ledger, err := NewRunBoundLLMLedger(generation.Store, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	calls, err := NewReplayableSimilarityCalls(ledger, s.config.Provider, generation.Blobs, generation.Clock, s.config.CostUpperBoundMicroUSD)
	if err != nil {
		return err
	}
	_, err = calls.Reconcile(ctx, open, request)
	return err
}
