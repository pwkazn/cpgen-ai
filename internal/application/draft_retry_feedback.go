package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

const draftRetryFeedbackField = "retry_feedback"

// Unversioned runs predate both the legacy and feedback protocols. For an
// already opened call, only its complete bound identity may select between
// those two compiled contracts. Errors during reconstruction never cause a
// fallback. Runs with an explicit version have exactly one contract.
var errDraftProtocolIdentityMismatch = errors.New("no compiled draft protocol matches the persisted call identity")

func resolveDraftRetryVariables(ctx context.Context, options GenerationReaderOptions, reader port.DraftRetryFeedbackReader, attempt domain.StageAttempt, variables []byte, persisted bool, matches func([]byte) (bool, error)) ([]byte, error) {
	if options.DraftRetryFeedbackVersion != "" && options.DraftRetryFeedbackVersion != "v1" {
		return nil, errors.New("unsupported frozen draft retry feedback version")
	}
	if options.DraftRetryFeedbackVersion == "v1" {
		return addDraftRetryFeedback(ctx, options.WorkflowRevision, reader, attempt.RunID, attempt.StageName, attempt.Ordinal, attempt.StartedAt, variables)
	}
	if !persisted || options.WorkflowRevision != workflow.ExecutedSamplesRevision || attempt.Ordinal <= 1 {
		return append([]byte(nil), variables...), nil
	}
	feedback, err := addDraftRetryFeedback(ctx, options.WorkflowRevision, reader, attempt.RunID, attempt.StageName, attempt.Ordinal, attempt.StartedAt, variables)
	if err != nil {
		return nil, err
	}
	variants := [][]byte{variables}
	if !bytes.Equal(variables, feedback) {
		variants = append(variants, feedback)
	}
	var selected []byte
	for _, variant := range variants {
		ok, err := matches(variant)
		if err != nil {
			return nil, err
		}
		if ok {
			if selected != nil {
				return nil, errors.New("persisted draft call matches multiple compiled protocols")
			}
			selected = append([]byte(nil), variant...)
		}
	}
	if selected == nil {
		return nil, errDraftProtocolIdentityMismatch
	}
	return selected, nil
}

func addDraftRetryFeedback(ctx context.Context, revision string, reader port.DraftRetryFeedbackReader, runID domain.RunID, stage domain.StageName, ordinal int, startedAt time.Time, variables []byte) ([]byte, error) {
	if revision != workflow.ExecutedSamplesRevision || ordinal <= 1 {
		return append([]byte(nil), variables...), nil
	}
	if reader == nil {
		return nil, errors.New("draft regeneration requires its durable feedback reader")
	}
	if startedAt.IsZero() {
		return nil, errors.New("draft regeneration requires its persisted attempt start time")
	}
	feedback, err := reader.ReadDraftRetryFeedbackBefore(ctx, runID, stage, startedAt)
	if err != nil {
		return nil, err
	}
	if feedback == nil {
		return append([]byte(nil), variables...), nil
	}
	if err := feedback.Validate(); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(variables, &object); err != nil || object == nil {
		return nil, errors.New("draft retry variables are not a JSON object")
	}
	if _, exists := object[draftRetryFeedbackField]; exists {
		return nil, errors.New("draft retry variables already contain feedback")
	}
	encoded, err := json.Marshal(feedback)
	if err != nil {
		return nil, err
	}
	object[draftRetryFeedbackField] = encoded
	return json.Marshal(object)
}

func buildLLMDraftPromptForVariables(stage, revision, dataVersion string, variables []byte) (port.PromptRef, port.OutputSchemaRef, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(variables, &object); err != nil {
		return port.PromptRef{}, port.OutputSchemaRef{}, errors.New("draft variables are not valid JSON")
	}
	if _, found := object[draftRetryFeedbackField]; !found {
		return buildLLMDraftPrompt(stage, revision, dataVersion)
	}
	if revision != workflow.ExecutedSamplesRevision {
		return port.PromptRef{}, port.OutputSchemaRef{}, errors.New("retry feedback requires the executed-samples prompt contract")
	}
	dataRetryVersion := "v4"
	if dataVersion == "v5" {
		dataRetryVersion = "v6"
	}
	version := map[string]string{"idea": "v3", "statement": "v3", "solution": "v3", "data": dataRetryVersion}[stage]
	if version == "" {
		return port.PromptRef{}, port.OutputSchemaRef{}, errors.New("retry feedback is unsupported for this draft stage")
	}
	return buildCompiledDraftPromptVersion(stage+".draft", version)
}
