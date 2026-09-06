package fake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type PrepareStep struct{ capabilities workflow.PrepareCapabilities }

func NewPrepareStep(capabilities workflow.PrepareCapabilities) *PrepareStep {
	return &PrepareStep{capabilities: capabilities}
}
func NewPrepare(capabilities workflow.PrepareCapabilities) *PrepareStep {
	return NewPrepareStep(capabilities)
}
func (s *PrepareStep) Name() domain.StageName { return "prepare" }
func (s *PrepareStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Input) (domain.AgentResult[domain.Slice1Prepared], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Prepared](cancelEvidenceFor(ctx, err)), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Prepared]{}, err
	}
	if input.RequestDigest != view.RequestDigest() || input.ConfigDigest != view.ConfigDigest() {
		return domain.AgentResult[domain.Slice1Prepared]{}, errors.New("slice1 input is not bound to RunView")
	}
	switch input.Scenario {
	case "blocked":
		return domain.Blocked[domain.Slice1Prepared](blockedCheckpoint(view, input.RequestDigest, "prepare")), nil
	case "retry":
		return domain.Retry[domain.Slice1Prepared](retryFailure(input.RequestDigest)), nil
	case "failure":
		return domain.Failure[domain.Slice1Prepared](permanentFailure(input.RequestDigest)), nil
	case "cancel":
		return domain.Cancelled[domain.Slice1Prepared](cancelEvidence(input.RequestDigest)), nil
	}
	if strings.Contains(input.Scenario, "artifact") && s.capabilities.Artifacts != nil {
		writer, err := s.capabilities.Artifacts.Prepare(ctx, port.ArtifactDeclaration{
			MediaType: "text/plain", Role: domain.ArtifactEvidence, LogicalPath: "slice1/prepare.txt", MaxBytes: 128,
			Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "slice1.fake", InputDigest: &input.RequestDigest},
		})
		if err != nil {
			return domain.AgentResult[domain.Slice1Prepared]{}, err
		}
		if writer == nil {
			return domain.AgentResult[domain.Slice1Prepared]{}, errors.New("fake artifact sink returned nil writer")
		}
		if _, err := writer.Write([]byte("slice1 prepared\n")); err != nil {
			return domain.AgentResult[domain.Slice1Prepared]{}, err
		}
		if _, err := writer.Finalize(ctx); err != nil {
			return domain.AgentResult[domain.Slice1Prepared]{}, err
		}
	}
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.prepare/v1\x00%s\x00%s\x00%s", input.RequestDigest, input.ConfigDigest, view.WorkflowDigest())))
	return domain.Success(domain.Slice1Prepared{Digest: digest, Summary: "slice1 prepared", Scenario: input.Scenario}), nil
}

func (s *PrepareStep) Revalidate(ctx context.Context, view domain.RunView, binding domain.BlockedCheckpoint) (bool, error) {
	if err := binding.Validate(); err != nil {
		return false, err
	}
	if binding.PolicyDigest != view.ConfigDigest() {
		return false, nil
	}
	if s.capabilities.LLM == nil {
		return true, nil
	}
	outcome, err := s.capabilities.LLM.Generate(ctx, port.GenerateRequest{Prompt: port.PromptRef{Step: binding.DependencyID, Version: "v1", Digest: binding.DependencyDigest}, Schema: port.OutputSchemaRef{SchemaVersion: view.SchemaVersion(), Digest: binding.PolicyDigest}, Variables: []byte(`{"dependency_digest":"` + string(binding.DependencyDigest) + `"}`), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 1, Bytes: 1}})
	if err != nil {
		return false, err
	}
	return outcome.Failure == nil && outcome.Value != nil, nil
}

type ExerciseStep struct{ capabilities workflow.ExerciseCapabilities }

func NewExerciseStep(capabilities workflow.ExerciseCapabilities) *ExerciseStep {
	return &ExerciseStep{capabilities: capabilities}
}
func NewExercise(capabilities workflow.ExerciseCapabilities) *ExerciseStep {
	return NewExerciseStep(capabilities)
}
func (s *ExerciseStep) Name() domain.StageName { return "exercise" }
func (s *ExerciseStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Prepared) (domain.AgentResult[domain.Slice1Evidence], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Evidence](cancelEvidenceFor(ctx, err)), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Evidence]{}, err
	}
	if strings.Contains(input.Scenario, "sandbox") && s.capabilities.Sandbox != nil {
		source := domain.BlobRef{Digest: domain.SumBytes([]byte("slice1.source")), Size: int64(len("slice1.source"))}
		manifest := port.SourceBundleManifest{SchemaVersion: view.SchemaVersion(), Files: []port.SourceFile{{Path: "main.cpp", Blob: source}}, EntryPoint: "main.cpp"}
		var err error
		manifest.Digest, err = port.ComputeSourceBundleDigest(manifest)
		if err != nil {
			return domain.AgentResult[domain.Slice1Evidence]{}, err
		}
		compiled, err := s.capabilities.Sandbox.Compile(ctx, port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleSolution, SourceBundle: manifest, Toolchain: "slice1.fake", Limits: port.CompileLimits{Time: time.Second, MemoryBytes: 1 << 20, PIDs: 16, OutputBytes: 1024}, ExpectedOutput: "main"})
		if err != nil {
			return domain.AgentResult[domain.Slice1Evidence]{}, err
		}
		if compiled.Failure != nil {
			return domain.Retry[domain.Slice1Evidence](retryFailure(input.Digest)), nil
		}
		if s.capabilities.Blobs != nil {
			reader, err := s.capabilities.Blobs.OpenVerified(ctx, source)
			if err != nil {
				return domain.AgentResult[domain.Slice1Evidence]{}, err
			}
			if reader != nil {
				if err := reader.Close(); err != nil {
					return domain.AgentResult[domain.Slice1Evidence]{}, err
				}
			}
		}
		runResult, err := s.capabilities.Sandbox.Run(ctx, port.RunRequest{Role: port.RoleSolution, Program: source, Limits: port.RunLimits{Time: time.Second, MemoryBytes: 1 << 20, PIDs: 16, StdoutBytes: 1024, StderrBytes: 1024}})
		if err != nil {
			return domain.AgentResult[domain.Slice1Evidence]{}, err
		}
		if runResult.Failure != nil {
			return domain.Retry[domain.Slice1Evidence](retryFailure(input.Digest)), nil
		}
	}
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.exercise/v1\x00%s\x00%s", input.Digest, view.ConfigDigest())))
	return domain.Success(domain.Slice1Evidence{Digest: digest, PreparedDigest: input.Digest, Scenario: input.Scenario}), nil
}

type CheckpointStep struct {
	capabilities workflow.CheckpointCapabilities
}

func NewCheckpointStep(capabilities workflow.CheckpointCapabilities) *CheckpointStep {
	return &CheckpointStep{capabilities: capabilities}
}
func NewCheckpoint(capabilities workflow.CheckpointCapabilities) *CheckpointStep {
	return NewCheckpointStep(capabilities)
}
func (s *CheckpointStep) Name() domain.StageName { return "checkpoint" }
func (s *CheckpointStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Evidence) (domain.AgentResult[domain.Slice1Checkpoint], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Checkpoint](cancelEvidenceFor(ctx, err)), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Checkpoint]{}, err
	}
	if strings.Contains(input.Scenario, "cache") && s.capabilities.Cache != nil {
		lookup := domain.CacheLookup{RunID: view.RunID(), Key: domain.CacheKey{Digest: domain.SumBytes([]byte("slice1.cache")), Kind: "slice1"}, SchemaVersion: view.SchemaVersion(), PolicyDigest: view.ConfigDigest(), InputDigest: input.Digest, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		candidate, hit, err := s.capabilities.Cache.Lookup(ctx, lookup)
		if err != nil {
			return domain.AgentResult[domain.Slice1Checkpoint]{}, err
		}
		if hit {
			if err := candidate.ValidateFor(lookup); err != nil {
				return domain.AgentResult[domain.Slice1Checkpoint]{}, err
			}
		}
	}
	if strings.Contains(input.Scenario, "mutation") && s.capabilities.Mutations != nil {
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		claim, err := s.capabilities.Mutations.ClaimMutation(ctx, domain.MutationClaimRequest{RunID: view.RunID(), StageName: "checkpoint", ScopeDigest: view.WorkflowDigest(), SourceBatchDigest: input.Digest, Ordinal: 1, Limit: 1, LimitSnapshot: 1, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("slice1 mutation")), At: at})
		if err != nil {
			return domain.AgentResult[domain.Slice1Checkpoint]{}, err
		}
		if claim.ClaimID != "" && claim.GrantDigest != "" {
			if err := s.capabilities.Mutations.RecordMutation(ctx, domain.MutationRecordRequest{Grant: claim, RecordID: "slice1-mutation", Operations: []domain.MutationOperation{{CallRecordID: "callrec_00000000000000000000000000000000", AttemptID: view.AttemptID()}}, Reservations: []domain.MutationReservation{{ReservationID: "reservation_00000000000000000000000000000000", CallRecordID: "callrec_00000000000000000000000000000000", AttemptCallID: "call_00000000000000000000000000000000"}}, OutputOccurrenceID: "occurrence_00000000000000000000000000000000", At: at}); err != nil {
				return domain.AgentResult[domain.Slice1Checkpoint]{}, err
			}
		}
	}
	switch input.Scenario {
	case "blocked":
		return domain.Blocked[domain.Slice1Checkpoint](blockedCheckpoint(view, input.PreparedDigest, "checkpoint")), nil
	case "retry":
		return domain.Retry[domain.Slice1Checkpoint](retryFailure(input.PreparedDigest)), nil
	case "failure":
		return domain.Failure[domain.Slice1Checkpoint](permanentFailure(input.PreparedDigest)), nil
	case "cancel":
		return domain.Cancelled[domain.Slice1Checkpoint](cancelEvidence(input.PreparedDigest)), nil
	default:
		checkpoint := domain.Slice1Checkpoint{EvidenceDigest: input.Digest, Reason: "slice1_fixture_complete"}
		checkpoint.Digest = domain.SumBytes([]byte(fmt.Sprintf("slice1.checkpoint/v1\x00%s\x00%s", input.Digest, view.WorkflowDigest())))
		return domain.Review[domain.Slice1Checkpoint](domain.ReviewRequest{EvidenceDigest: input.Digest, PolicyDigest: view.ConfigDigest(), Reason: "slice1_fixture_complete"}), nil
	}
}

func (s *CheckpointStep) Revalidate(ctx context.Context, view domain.RunView, binding domain.BlockedCheckpoint) (bool, error) {
	if err := binding.Validate(); err != nil {
		return false, err
	}
	if binding.PolicyDigest != view.ConfigDigest() {
		return false, nil
	}
	if s.capabilities.Similarity == nil {
		return true, nil
	}
	outcome, err := s.capabilities.Similarity.Search(ctx, port.SimilaritySearchRequest{Query: binding.DependencyID, QueryDigest: binding.DependencyDigest, Limit: 1})
	if err != nil {
		return false, err
	}
	return outcome.Failure == nil && outcome.Value != nil, nil
}

func blockedCheckpoint(view domain.RunView, input domain.Digest, stage domain.StageName) domain.BlockedCheckpoint {
	dependency := domain.SumBytes([]byte("slice1.dependency/v1\x00" + string(input)))
	policy := view.ConfigDigest()
	return domain.BlockedCheckpoint{RunID: view.RunID(), StageName: stage, StageInputDigest: input, DependencyID: "slice1-provider", DependencyDigest: dependency, PolicyDigest: policy, ErrorDigest: domain.SumBytes([]byte("slice1 blocked")), RetryAfter: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func retryFailure(input domain.Digest) domain.RetryableFailure {
	return domain.RetryableFailure{Code: domain.FailureUnavailable, Evidence: domain.SumBytes([]byte("slice1 retry/v1\x00" + string(input)))}
}
func permanentFailure(input domain.Digest) domain.PermanentFailure {
	return domain.PermanentFailure{Code: domain.FailurePolicyRejected, Evidence: domain.SumBytes([]byte("slice1 failure/v1\x00" + string(input)))}
}
func cancelEvidence(input domain.Digest) domain.CancellationEvidence {
	return domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte("slice1 cancel/v1\x00" + string(input)))}
}

func cancelEvidenceFor(ctx context.Context, fallback error) domain.CancellationEvidence {
	cause := domain.CauseUserCancel
	if interrupted, ok := context.Cause(ctx).(domain.ExecutionInterrupted); ok && interrupted.Cause.Valid() {
		cause = interrupted.Cause
	}
	return domain.CancellationEvidence{Cause: cause, Evidence: domain.SumBytes([]byte("slice1 cancel/v1\x00" + fallback.Error()))}
}
