package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	docker "cpgen/internal/adapter/sandbox/docker"
	artifact "cpgen/internal/artifact"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

type CommittedSimilarityReader interface {
	ReadCommitted(context.Context, domain.RunID) (SimilarityContent, error)
}
type SolutionReader struct {
	similarity CommittedSimilarityReader
	generation *GenerationReader
	calls      durable.CommittedDraftReader
	store      SandboxEvidenceReadStore
	blobs      port.VerifiedBlobReader
	revision   string
}

// ReadInput reconstructs current committed acceptance; a caller-supplied ACCEPT is insufficient.
func (s *SolutionReader) ReadInput(ctx context.Context, runID domain.RunID) (domain.AgentResult[domain.SolutionDraftInputV1], error) {
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

// ReadDraft validates committed content provenance, not compilation or Judge proof.
func (s *SolutionReader) ReadDraft(ctx context.Context, runID domain.RunID) (domain.SolutionContent, error) {
	var empty domain.SolutionContent
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("solution draft has no accepted current source")
	}
	return s.readDraftForInput(ctx, runID, *input.Value)
}

// readDraftForInput consumes acceptance verified during the same top-level read.
func (s *SolutionReader) readDraftForInput(ctx context.Context, runID domain.RunID, input domain.SolutionDraftInputV1) (domain.SolutionContent, error) {
	var empty domain.SolutionContent
	variables, err := solutionDraftVariables(s.revision, input)
	if err != nil {
		return empty, err
	}
	digest, err := input.Digest()
	if err != nil {
		return empty, err
	}
	calls := s.calls
	raw, expected, err := s.generation.readDraft(ctx, runID, "solution", digest, variables, calls)
	if err != nil {
		return empty, err
	}
	var draft domain.SolutionDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return empty, err
	}
	if err := validateSolutionDraftForWorkflow(s.revision, draft); err != nil {
		return empty, err
	}
	content, err := draft.Bind(input)
	if err != nil {
		return empty, err
	}
	if content.ContentDigest != expected {
		return empty, errors.New("committed solution digest differs from reconstructed content")
	}
	return content, nil
}

func solutionDraftVariables(revision string, input domain.SolutionDraftInputV1) ([]byte, error) {
	if revision == workflow.ExecutedSamplesRevision {
		return input.ProgramContextJSON()
	}
	return input.CanonicalJSON()
}

func validateSolutionDraftForWorkflow(revision string, draft domain.SolutionDraftV1) error {
	if revision == workflow.ExecutedSamplesRevision {
		return draft.ValidateDistinctSources()
	}
	return draft.Validate()
}

type solutionVerificationReadStore interface {
	port.SandboxLifecycleReader
	ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
}

// ReadVerification proves the current committed report, its exact source and
// sample execution requests, Docker receipts, retained artifacts and cleanup.
// Configuration supplies the frozen toolchain/Engine policy only; this method
// never starts Docker, writes artifacts, or calls either external provider.
func (s *SolutionReader) ReadVerification(ctx context.Context, runID domain.RunID, config sandboxexec.ReadPolicy) (SolutionVerificationReport, error) {
	var empty SolutionVerificationReport
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("verification has no current accepted input")
	}
	content, err := s.readDraftForInput(ctx, runID, *input.Value)
	if err != nil {
		return empty, err
	}
	return s.readVerificationForInput(ctx, runID, config, *input.Value, content)
}

// readVerificationForInput reuses the current acceptance and draft already
// verified in this read; the report and all retained execution proof are reread.
func (s *SolutionReader) readVerificationForInput(ctx context.Context, runID domain.RunID, config sandboxexec.ReadPolicy, input domain.SolutionDraftInputV1, content domain.SolutionContent) (SolutionVerificationReport, error) {
	var empty SolutionVerificationReport
	store := s.store
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "solution_verify")
	if err != nil {
		return empty, err
	}
	attempt := stage.Attempt
	if attempt.Validate() != nil || attempt.RunID != runID || attempt.StageName != "solution_verify" || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != content.ContentDigest || attempt.OutputDigest == nil {
		return empty, errors.New("solution verification stage differs from the current draft")
	}
	config.Identity = solutionVerificationIdentity(attempt, 1)
	lockDigest, err := config.Lock.Digest()
	if err != nil {
		return empty, err
	}
	items := make(map[domain.SafeRelPath]port.CommittedPrivateStageArtifact, len(stage.Artifacts))
	used := make(map[domain.SafeRelPath]bool, len(stage.Artifacts))
	for _, item := range stage.Artifacts {
		if _, duplicate := items[item.Blob.LogicalPath]; duplicate {
			return empty, errors.New("verification repeats an artifact path")
		}
		items[item.Blob.LogicalPath] = item
	}
	blobs := s.blobs
	read := func(path domain.SafeRelPath, expected domain.BlobRef, role domain.ArtifactRole, limit int64) ([]byte, error) {
		item, exists := items[path]
		if !exists || item.Blob.Blob != expected || item.Blob.Role != role {
			return nil, fmt.Errorf("verification artifact is absent or differs: %s", path)
		}
		used[path] = true
		return artifact.ReadVerified(ctx, blobs, expected, limit)
	}
	item, found := items["solution/verification.json"]
	wantSchema := domain.SchemaVersion(solutionVerificationSchema)
	if s.revision == workflow.ExecutedSamplesRevision {
		wantSchema = domain.SchemaVersion(executedSolutionVerificationSchema)
	}
	if !found || item.Blob.Blob.Digest != *attempt.OutputDigest || item.Blob.MediaType != "application/vnd.cpgen.solution-verification+json" || item.Blob.Provenance.SchemaVersion != wantSchema || item.Blob.Provenance.Producer != "solution-verifier" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != content.ContentDigest {
		return empty, errors.New("verification output lacks its committed report binding")
	}
	raw, err := read(item.Blob.LogicalPath, item.Blob.Blob, domain.ArtifactOutput, 1<<20)
	if err != nil {
		return empty, err
	}
	var report SolutionVerificationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return empty, err
	}
	if report.SchemaVersion != string(wantSchema) {
		return empty, errors.New("verification report schema differs from the frozen workflow")
	}
	canonical, err := json.Marshal(report)
	if err != nil || !bytes.Equal(raw, canonical) {
		return empty, errors.New("verification report is not canonical")
	}
	if err := report.ValidateFor(input, content); err != nil {
		return empty, err
	}
	if report.ToolchainLockDigest != lockDigest || report.PolicyDigest != solutionVerificationPolicyDigest(lockDigest, report.SchemaVersion) {
		return empty, errors.New("verification policy differs from the frozen toolchain")
	}
	verifyPending := func(p *domain.PendingArtifact) error {
		if p == nil {
			return nil
		}
		item, exists := items[p.LogicalPath]
		if !exists || item.Blob.MediaType != p.MediaType || !reflect.DeepEqual(item.Blob.Provenance, p.Provenance) {
			return errors.New("verification process artifact metadata differs")
		}
		call, err := s.store.ReadLogicalCall(ctx, item.CurrentCallRecordID)
		if err != nil {
			return err
		}
		if call.ResultAttemptCallID == nil || *call.ResultAttemptCallID != p.CallID {
			return errors.New("verification process artifact changed its local producer")
		}
		_, err = read(p.LogicalPath, p.Blob, p.Role, 8<<20)
		return err
	}
	verifyResult := func(kind domain.CallKind, request any, result any, build func(docker.PlanIdentity) (port.ContainerPlan, error)) error {
		identity, planIdentity, err := sandboxexec.ReadOperationIdentity(config, kind, request)
		if err != nil {
			return err
		}
		plan, err := build(planIdentity)
		if err != nil {
			return err
		}
		path := domain.SafeRelPath("sandbox/" + string(identity.SandboxExecutionID) + "/result.json")
		item, exists := items[path]
		if !exists || item.Blob.MediaType != "application/vnd.cpgen.sandbox-result+json" || item.Blob.Provenance.SchemaVersion != "cpgen.sandbox-result/v1" || item.Blob.Provenance.Producer != "docker" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != identity.ScopeDigest {
			return errors.New("verification lacks the exact Docker request receipt")
		}
		encoded, err := read(path, item.Blob.Blob, domain.ArtifactEvidence, 1<<20)
		if err != nil {
			return err
		}
		var receipt sandboxexec.ResultReceipt[json.RawMessage]
		if err := json.Unmarshal(encoded, &receipt); err != nil {
			return err
		}
		expected, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if receipt.Schema != "cpgen.sandbox-result/v1" || receipt.Scope != identity.ScopeDigest || receipt.Plan != plan.PlanDigest || !bytes.Equal(receipt.Result, expected) {
			return errors.New("verification result differs from its Docker receipt")
		}
		return sandboxexec.VerifyCleaned(ctx, store, identity, plan)
	}
	language, filename, _, compiler, err := solutionCompiler(content.Language, config.Lock)
	if err != nil {
		return empty, err
	}
	programs := make(map[port.ProgramRole]domain.BlobRef)
	for index, compiled := range report.Compiles {
		name := "reference"
		if index == 1 {
			name = "brute"
		}
		if _, err := read(domain.SafeRelPath("solution/"+name+"/"+filename), compiled.Source, domain.ArtifactSource, 262144); err != nil {
			return empty, err
		}
		bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: domain.SafeRelPath(filename), Files: []port.SourceFile{{Path: domain.SafeRelPath(filename), Blob: compiled.Source}}}
		bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
		if err != nil {
			return empty, err
		}
		if bundle.Digest != compiled.SourceBundleDigest {
			return empty, errors.New("verification source bundle differs")
		}
		request := port.CompileRequest{Language: language, Role: compiled.Role, SourceBundle: bundle, Toolchain: compiler.ID, Limits: solutionCompileLimits(), ExpectedOutput: compiler.OutputPath}
		if err := verifyResult(domain.CallSandboxCompile, request, compiled.Result, func(i docker.PlanIdentity) (port.ContainerPlan, error) {
			return docker.BuildCompilePlan(request, config.Lock, i)
		}); err != nil {
			return empty, err
		}
		for _, p := range []*domain.PendingArtifact{compiled.Result.Program, compiled.Result.Stdout, compiled.Result.Stderr, compiled.Result.Execution} {
			if err := verifyPending(p); err != nil {
				return empty, err
			}
		}
		if compiled.Result.Program != nil {
			programs[compiled.Role] = compiled.Result.Program.Blob
		}
	}
	for _, sample := range report.Samples {
		if _, err := read(domain.SafeRelPath(fmt.Sprintf("solution/samples/%03d.in", sample.Sample)), sample.Input, domain.ArtifactInput, 1<<20); err != nil {
			return empty, err
		}
		if report.SchemaVersion == solutionVerificationSchema {
			if sample.Expected == nil {
				return empty, errors.New("legacy verification sample lacks its expected output")
			}
			if _, err := read(domain.SafeRelPath(fmt.Sprintf("solution/samples/%03d.out", sample.Sample)), *sample.Expected, domain.ArtifactOutput, 1<<20); err != nil {
				return empty, err
			}
		}
		request := port.RunRequest{Role: sample.Role, Program: programs[sample.Role], Stdin: &sample.Input, Limits: port.RunLimits{Time: time.Duration(input.Problem.TimeLimitMS) * time.Millisecond, MemoryBytes: input.Problem.MemoryLimitMB << 20, PIDs: 64, StdoutBytes: 1 << 20, StderrBytes: 1 << 20}}
		if err := verifyResult(domain.CallSandboxRun, request, sample.Result, func(i docker.PlanIdentity) (port.ContainerPlan, error) {
			return docker.BuildRunPlan(request, config.Lock, i)
		}); err != nil {
			return empty, err
		}
		for _, p := range []*domain.PendingArtifact{sample.Result.Stdout, sample.Result.Stderr, sample.Result.Execution} {
			if err := verifyPending(p); err != nil {
				return empty, err
			}
		}
		if sample.ActualTokenDigest != "" {
			actual, err := artifact.ReadVerified(ctx, blobs, sample.Result.Stdout.Blob, 1<<20)
			if err != nil {
				return empty, err
			}
			if solutionTokenDigest(actual) != sample.ActualTokenDigest {
				return empty, errors.New("verification token comparison differs from actual stdout")
			}
		}
	}
	if len(used) != len(items) {
		return empty, errors.New("verification stage contains unrelated artifacts")
	}
	return report, nil
}
