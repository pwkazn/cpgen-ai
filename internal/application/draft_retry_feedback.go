package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

const draftRetryFeedbackField = "retry_feedback"

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
