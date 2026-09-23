package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

type GenerationReadStore interface {
	port.GenerationStore
	ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
}

// These are frozen stage policy values, shared with the composing executor.
// Changing them cannot reinterpret already committed model output.
type GenerationReaderOptions struct {
	WorkflowRevision     string
	DataPromptVersion    string
	IdeaCount            int
	SelectionPolicy      string
	StatementRevision    int64
	ProviderPolicyDigest domain.Digest
	Sampling             port.SamplingPolicy
	MaxOutput            port.OutputLimit
}

type GenerationReader struct {
	store           GenerationReadStore
	idea, statement durable.CommittedDraftReader
	options         GenerationReaderOptions
}

type GenerationIdeaContent struct {
	Snapshot       domain.GenerationRequestSnapshotV1
	Batch          domain.IdeaBatch
	Selection      domain.IdeaSelection
	StatementInput domain.StatementInput
}

// Candidate collection can retain a full rejected batch before a subsequent
// selection/mutation boundary. It grants no permission to generate Statement.
type GenerationIdeaCandidates struct {
	Snapshot domain.GenerationRequestSnapshotV1
	Batch    domain.IdeaBatch
}

type GenerationStatementContent struct {
	Idea    GenerationIdeaContent
	Problem domain.ProblemSpec
}

func NewGenerationReader(store GenerationReadStore, idea, statement durable.CommittedDraftReader, options GenerationReaderOptions) (*GenerationReader, error) {
	if store == nil || idea == nil || statement == nil {
		return nil, errors.New("generation reader requires typed storage and both structured response readers")
	}
	if options.IdeaCount < 2 || options.IdeaCount > 8 || options.StatementRevision <= 0 || (options.SelectionPolicy != domain.SelectionOrdinalPolicyV1 && options.SelectionPolicy != domain.SelectionIdeaIDPolicyV1) || options.MaxOutput.Bytes > 64<<20 {
		return nil, errors.New("generation reader has an unsupported content policy")
	}
	if _, err := draftPromptVersion("data.draft", options.WorkflowRevision, options.DataPromptVersion); err != nil {
		return nil, err
	}
	for _, err := range []error{options.ProviderPolicyDigest.Validate(), options.Sampling.Validate(), options.MaxOutput.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	return &GenerationReader{store: store, idea: idea, statement: statement, options: options}, nil
}

// ReadIdeaCandidates verifies only the committed collection boundary. The
// current successful attempt and private producer provenance remain mandatory,
// including for a batch whose recorded candidates are all rejected.
func (r *GenerationReader) ReadIdeaCandidates(ctx context.Context, runID domain.RunID) (GenerationIdeaCandidates, error) {
	var result GenerationIdeaCandidates
	if ctx == nil {
		return result, errors.New("generation input read requires a context")
	}
	snapshot, err := r.store.ReadGenerationSnapshot(ctx, runID)
	if err != nil {
		return result, err
	}
	input, err := domain.NewIdeaDraftInput(snapshot, r.options.IdeaCount)
	if err != nil {
		return result, err
	}
	variables, err := input.CanonicalJSON()
	if err != nil {
		return result, err
	}
	raw, expected, err := r.readDraft(ctx, runID, "idea", snapshot.SnapshotDigest, variables, r.idea)
	if err != nil {
		return result, err
	}
	var draft domain.IdeaDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return result, err
	}
	batch, err := draft.Bind(input)
	if err != nil {
		return result, err
	}
	if batch.BatchDigest != expected {
		return result, errors.New("committed Idea digest differs from reconstructed domain output")
	}
	return GenerationIdeaCandidates{snapshot, batch}, nil
}

// ReadIdea additionally proves selection and the committed Statement input.
// A retained rejected batch cannot become downstream generation input.
func (r *GenerationReader) ReadIdea(ctx context.Context, runID domain.RunID) (GenerationIdeaContent, error) {
	var result GenerationIdeaContent
	collected, err := r.ReadIdeaCandidates(ctx, runID)
	if err != nil {
		return result, err
	}
	snapshot, batch := collected.Snapshot, collected.Batch
	ids, err := batch.OrderedFeasibleCandidateIDs(r.options.SelectionPolicy)
	if err != nil {
		return result, err
	}
	if len(ids) == 0 {
		return result, errors.New("committed Idea output has no feasible candidate for downstream work")
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, ids[0], r.options.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		return result, err
	}
	statement := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	if err := statement.ValidateChain(snapshot, batch, selection); err != nil {
		return result, err
	}
	// Idea's stage commit also bound the downstream input. Check this before
	// any new Statement attempt can replace a pending stage's input digest.
	expectedInput, err := r.store.ReadStageInputDigest(ctx, runID, "statement")
	if err != nil {
		return result, err
	}
	actualInput, err := statement.Digest()
	if err != nil {
		return result, err
	}
	if actualInput != expectedInput {
		return result, errors.New("reconstructed selection differs from the committed Statement input")
	}
	return GenerationIdeaContent{snapshot, batch, selection, statement}, nil
}

func (r *GenerationReader) ReadStatement(ctx context.Context, runID domain.RunID) (GenerationStatementContent, error) {
	var result GenerationStatementContent
	idea, err := r.ReadIdea(ctx, runID)
	if err != nil {
		return result, err
	}
	input, err := domain.NewStatementDraftInput(idea.StatementInput, idea.Snapshot, idea.Batch, idea.Selection)
	if err != nil {
		return result, err
	}
	variables, err := input.CanonicalJSON()
	if err != nil {
		return result, err
	}
	inputDigest, err := idea.StatementInput.Digest()
	if err != nil {
		return result, err
	}
	raw, expected, err := r.readDraft(ctx, runID, "statement", inputDigest, variables, r.statement)
	if err != nil {
		return result, err
	}
	var draft domain.StatementDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return result, err
	}
	problem, err := draft.Bind(input, r.options.StatementRevision)
	if err != nil {
		return result, err
	}
	if problem.SpecDigest != expected {
		return result, errors.New("committed Statement digest differs from reconstructed domain output")
	}
	return GenerationStatementContent{idea, problem}, nil
}

func (r *GenerationReader) readDraft(ctx context.Context, runID domain.RunID, stage domain.StageName, inputDigest domain.Digest, variables []byte, service durable.CommittedDraftReader) ([]byte, domain.Digest, error) {
	committed, err := r.store.ReadCommittedLLMStage(ctx, runID, stage)
	if err != nil {
		return nil, "", err
	}
	attempt := committed.Attempt
	if attempt.Validate() != nil || committed.StageVersion <= 0 || attempt.RunID != runID || attempt.StageName != stage || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != inputDigest || attempt.OutputDigest == nil || len(committed.Artifacts) == 0 || len(committed.Artifacts) > 2 {
		return nil, "", errors.New("committed draft stage differs from the expected typed input")
	}
	var selected port.CommittedLLMStageArtifact
	var source domain.CallRecord
	seen := map[domain.CallRecordID]bool{}
	for _, artifact := range committed.Artifacts {
		if seen[artifact.ProviderCallRecordID] {
			return nil, "", errors.New("committed draft repeats provider provenance")
		}
		seen[artifact.ProviderCallRecordID] = true
		call, err := r.store.ReadLogicalCall(ctx, artifact.ProviderCallRecordID)
		if err != nil {
			return nil, "", err
		}
		if call.RunID != runID || call.StageName != stage || call.Kind != domain.CallLLMGenerate || call.State != domain.CallRecordTerminal {
			return nil, "", errors.New("committed draft provider scope differs")
		}
		if call.Failure == nil {
			if source.ID != "" {
				return nil, "", errors.New("committed draft has multiple successful provider outputs")
			}
			selected, source = artifact, call
		}
	}
	if source.ID == "" {
		return nil, "", errors.New("committed draft lacks a successful provider response")
	}
	candidates, err := r.store.ReadAttemptLLMCalls(ctx, runID, source.StageName, source.AttemptID)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) > 64 {
		return nil, "", errors.New("committed draft call history exceeds bound")
	}
	prompt, schema, err := buildLLMDraftPrompt(string(stage), r.options.WorkflowRevision, r.options.DataPromptVersion)
	if err != nil {
		return nil, "", err
	}
	for _, candidate := range candidates {
		if candidate.RunID != runID || candidate.StageName != stage || candidate.AttemptID != source.AttemptID || candidate.Kind != domain.CallLLMGenerate || candidate.State != domain.CallRecordTerminal {
			continue
		}
		request := port.GenerateRequest{Prompt: prompt, Schema: schema, Variables: append(json.RawMessage(nil), variables...), Sampling: r.options.Sampling, MaxOutput: r.options.MaxOutput, LogicalIdempotencyKey: candidate.LogicalOperationID, ProviderPolicyDigest: r.options.ProviderPolicyDigest, PrivacyClassification: "private"}
		plan, err := service.PlanGenerate(request)
		if err != nil {
			return nil, "", err
		}
		open := domain.OpenCallRequest{ID: candidate.ID, RunID: candidate.RunID, ExpectedRunVersion: 1, StageName: candidate.StageName, AttemptID: candidate.AttemptID, LogicalOperationID: candidate.LogicalOperationID, Kind: candidate.Kind, Provider: plan.Provider, RequestDigest: plan.RequestDigest, PolicyDigest: request.ProviderPolicyDigest, RetryPolicy: candidate.RetryPolicy, IdempotencyKey: candidate.IdempotencyKey, At: candidate.OpenedAt}
		bound, _, err := service.Bind(open, request)
		if err != nil {
			return nil, "", err
		}
		if bound.Provider != candidate.Provider || bound.RequestDigest != candidate.RequestDigest || bound.PolicyDigest != candidate.PolicyDigest {
			continue
		}
		id, response, err := service.ReadCommitted(ctx, open, request)
		if err != nil {
			return nil, "", err
		}
		if id != source.ID {
			continue
		}
		for providerID := range seen {
			if providerID != candidate.ID && providerID != id {
				return nil, "", errors.New("committed stage contains unrelated provider output")
			}
		}
		actualSource, item, err := service.ReadCommittedLLMArtifact(ctx, runID, response.RawBlob.WriterTokenID)
		if err != nil {
			return nil, "", err
		}
		if actualSource != selected.Source || item.SourceOccurrenceID != selected.Blob.SourceOccurrenceID || item.Blob != selected.Blob.Blob || item.MediaType != selected.Blob.MediaType || item.Role != selected.Blob.Role || item.LogicalPath != selected.Blob.LogicalPath || item.Provenance.SchemaVersion != selected.Blob.Provenance.SchemaVersion || item.Provenance.Producer != selected.Blob.Provenance.Producer || !durable.DigestsMatch(item.Provenance.InputDigest, selected.Blob.Provenance.InputDigest) {
			return nil, "", errors.New("committed draft response differs from current stage provenance")
		}
		return append([]byte(nil), response.Structured...), *attempt.OutputDigest, nil
	}
	return nil, "", errors.New("no original committed call matches the frozen draft input and provider policy")
}
