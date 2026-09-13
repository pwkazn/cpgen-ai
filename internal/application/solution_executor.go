package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

// SolutionExecutor reuses the generation ledger and verified committed
// Similarity chain. Draft collection alone is not compilation or Judge proof.
type SolutionExecutor struct {
	publisher StagePublisher
	blobs     port.VerifiedBlobReader
	reader    *SolutionReader
	drafts    *DraftExecution
}

// SolutionSandboxFactory is invoked only after the current verification
// attempt and committed acceptance/draft have been checked. It receives the
// application-owned scope used for both Docker calls and source publications.
type SolutionSandboxFactory func(context.Context, port.SandboxAuthorizationIdentity) (SolutionSandbox, toolchain.Lock, error)

// VerifyDraft completes an evidence-producing stage. A report with Passed=false
// still needs a successful atomic stage commit before the next gate can select
// human review. Infrastructure errors do not produce a passing content report.
func (s *SolutionExecutor) VerifyDraft(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (SolutionVerificationResult, error) {
	var empty SolutionVerificationResult
	if factory == nil {
		return empty, errors.New("solution verification requires a sandbox factory")
	}
	input, err := s.reader.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("solution verification requires committed acceptance")
	}
	content, err := s.reader.ReadDraft(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	attempt, err := s.drafts.admit(ctx, view, "solution_verify", content.ContentDigest)
	if err != nil {
		return empty, err
	}
	identity := solutionVerificationIdentity(attempt, view.Version())
	sandbox, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	publisher, err := s.publisher(attempt, view.Version())
	if err != nil {
		return empty, err
	}
	verifier, err := NewSolutionVerifier(SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: s.blobs, Lock: lock})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, *input.Value, content)
}

func solutionVerificationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID,
		SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", "solution-verification", attempt.RunID, attempt.AttemptID, attempt.InputDigest)),
		LogicalOperationID: "solution-verification", Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *SolutionExecutor) CollectDraft(ctx context.Context, view domain.RunView, input domain.SolutionDraftInputV1) (GenerationStageResult[domain.SolutionContent], error) {
	var result GenerationStageResult[domain.SolutionContent]
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.drafts.admit(ctx, view, "solution", digest)
	if err != nil {
		return result, err
	}
	expected, err := s.reader.ReadInput(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if expected.Review != nil {
		result.Outcome = domain.Review[domain.SolutionContent](*expected.Review)
		return result, nil
	}
	expectedDigest, err := expected.Value.Digest()
	if err != nil || expectedDigest != digest {
		return result, errors.New("solution input differs from current committed acceptance")
	}
	variables, err := input.CanonicalJSON()
	if err != nil {
		return result, err
	}
	generated, err := s.drafts.generate(ctx, view, attempt, variables)
	result = generationResult[domain.SolutionContent](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.SolutionContent](view, attempt, s.drafts.config.Content.ProviderPolicyDigest, generated.outcome, s.drafts.config.Clock.Now())
		return result, nil
	}
	var draft domain.SolutionDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	content, err := draft.Bind(input)
	if err != nil {
		result.Outcome = generationContentReview[domain.SolutionContent](s.drafts.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "solution_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(content)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

// ReadDraft reconstructs a committed content proposal. It must not be used as
// proof that its source compiled, matched samples, or passed later Judge gates.

func (s *SolutionExecutor) Reader() *SolutionReader { return s.reader }

func (s *SolutionExecutor) ReconcileDraft(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.drafts.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != "solution" {
		return errors.New("Solution cleanup requires its draft stage")
	}
	input, err := s.reader.ReadInput(ctx, runID)
	if err != nil {
		return err
	}
	if input.Value == nil {
		return errors.New("Solution cleanup lost current acceptance")
	}
	digest, err := input.Value.Digest()
	if err != nil {
		return err
	}
	if digest != attempt.InputDigest {
		return errors.New("Solution cleanup input differs")
	}
	variables, err := input.Value.CanonicalJSON()
	if err != nil {
		return err
	}
	return s.drafts.reconcileDraftRequest(ctx, current, *attempt, variables)
}
