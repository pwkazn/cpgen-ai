package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

// SolutionExecutor reuses the generation ledger and verified committed
// Similarity chain. Draft collection alone is not compilation or Judge proof.
type SolutionExecutor struct {
	similarity *SimilarityExecutor
	generation *GenerationExecutor
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
	input, err := s.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("solution verification requires committed acceptance")
	}
	content, err := s.ReadDraft(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	attempt, err := s.generation.admit(ctx, view, "solution_verify", content.ContentDigest)
	if err != nil {
		return empty, err
	}
	identity := solutionVerificationIdentity(attempt, view.Version())
	sandbox, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	config := s.generation.config
	publisher, err := NewSandboxArtifactSink(config.Store, config.Blobs, config.Clock, identity)
	if err != nil {
		return empty, err
	}
	verifier, err := NewSolutionVerifier(SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: config.Blobs, Lock: lock})
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

func NewSolutionExecutor(evidence *SimilarityExecutor) (*SolutionExecutor, error) {
	if evidence == nil || !workflow.HasSolutionStages(evidence.config.WorkflowRevision) {
		return nil, errors.New("solution executor requires the forward solution workflow")
	}
	generation := evidence.config.Generation
	if _, err := generation.calls(generation.config.Store, "solution"); err != nil {
		return nil, err
	}
	return &SolutionExecutor{evidence, generation}, nil
}

// ReadInput performs the simple business split using current committed
// evidence. It never reads or consumes mutation quota, retries Similarity or
// starts a model call. A structural caller-supplied ACCEPT is not sufficient.
func (s *SolutionExecutor) ReadInput(ctx context.Context, runID domain.RunID) (domain.AgentResult[domain.SolutionDraftInputV1], error) {
	var empty domain.AgentResult[domain.SolutionDraftInputV1]
	content, err := s.similarity.ReadCommitted(ctx, runID)
	if err != nil {
		return empty, err
	}
	if content.Decision.Kind != similarity.DecisionAccept {
		result := domain.Review[domain.SolutionDraftInputV1](domain.ReviewRequest{EvidenceDigest: content.Evidence.EvidenceDigest, PolicyDigest: content.Input.ExecutionPolicyDigest, Reason: "similarity_requires_review:" + string(content.Decision.Kind)})
		return result, result.Validate()
	}
	inputDigest, err := content.Input.Digest()
	if err != nil {
		return empty, err
	}
	decision, err := canonicalJSON(content.Decision)
	if err != nil {
		return empty, err
	}
	input, err := domain.NewSolutionDraftInput(content.Statement.Idea.Snapshot, content.Statement.Problem, inputDigest, content.Evidence.EvidenceDigest, domain.SumBytes(decision))
	if err != nil {
		return empty, err
	}
	return domain.Success(input), nil
}

func (s *SolutionExecutor) CollectDraft(ctx context.Context, view domain.RunView, input domain.SolutionDraftInputV1) (GenerationStageResult[domain.SolutionContent], error) {
	var result GenerationStageResult[domain.SolutionContent]
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.generation.admit(ctx, view, "solution", digest)
	if err != nil {
		return result, err
	}
	expected, err := s.ReadInput(ctx, view.RunID())
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
	generated, err := s.generation.generate(ctx, view, attempt, variables)
	result = generationResult[domain.SolutionContent](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.SolutionContent](view, attempt, s.generation.config.Content.ProviderPolicyDigest, generated.outcome, s.generation.config.Clock.Now())
		return result, nil
	}
	var draft domain.SolutionDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	content, err := draft.Bind(input)
	if err != nil {
		result.Outcome = generationContentReview[domain.SolutionContent](s.generation.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "solution_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(content)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

// ReadDraft reconstructs a committed content proposal. It must not be used as
// proof that its source compiled, matched samples, or passed later Judge gates.
func (s *SolutionExecutor) ReadDraft(ctx context.Context, runID domain.RunID) (domain.SolutionContent, error) {
	var empty domain.SolutionContent
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("solution draft has no accepted current source")
	}
	variables, err := input.Value.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	digest, err := input.Value.Digest()
	if err != nil {
		return empty, err
	}
	calls, err := s.generation.calls(s.generation.config.Store, "solution")
	if err != nil {
		return empty, err
	}
	raw, expected, err := s.generation.reader.readDraft(ctx, runID, "solution", digest, variables, calls)
	if err != nil {
		return empty, err
	}
	var draft domain.SolutionDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return empty, err
	}
	content, err := draft.Bind(*input.Value)
	if err != nil {
		return empty, err
	}
	if content.ContentDigest != expected {
		return empty, errors.New("committed solution digest differs from reconstructed content")
	}
	return content, nil
}
