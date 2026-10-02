package application

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type fixedDraftFeedbackReader struct {
	feedback *domain.DraftRetryFeedback
	stage    domain.StageName
}

func (r fixedDraftFeedbackReader) ReadDraftRetryFeedbackBefore(_ context.Context, _ domain.RunID, stage domain.StageName, _ time.Time) (*domain.DraftRetryFeedback, error) {
	if stage != r.stage || r.feedback == nil {
		return nil, nil
	}
	copy := *r.feedback
	return &copy, nil
}

type draftFeedbackEvent struct {
	at       time.Time
	feedback domain.DraftRetryFeedback
}

type timelineDraftFeedbackReader []draftFeedbackEvent

func (r timelineDraftFeedbackReader) ReadDraftRetryFeedbackBefore(_ context.Context, _ domain.RunID, stage domain.StageName, asOf time.Time) (*domain.DraftRetryFeedback, error) {
	var latest *draftFeedbackEvent
	for index := range r {
		event := &r[index]
		if event.at.After(asOf) || latest != nil && !event.at.After(latest.at) {
			continue
		}
		latest = event
	}
	if latest == nil || latest.feedback.TargetStage != stage {
		return nil, nil
	}
	copy := latest.feedback
	return &copy, nil
}

func TestAddDraftRetryFeedbackPreservesSourceInputAndUsesVersionedPrompts(t *testing.T) {
	variables := []byte(`{"request":{"brief":"frozen"},"source_input_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	original := append([]byte(nil), variables...)
	feedback := &domain.DraftRetryFeedback{SourceStage: "judge", TargetStage: "statement", Reason: "data_requires_review:sample.1.INVALID"}
	reader := fixedDraftFeedbackReader{feedback: feedback, stage: "statement"}
	runID := domain.RunID("run_00000000000000000000000000000001")

	attemptAt := time.Date(2026, 9, 30, 1, 17, 51, 436000000, time.UTC)
	firstAttempt, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, nil, runID, "statement", 1, attemptAt, variables)
	if err != nil || !reflect.DeepEqual(firstAttempt, variables) {
		t.Fatalf("first attempt variables = %s, %v; want unchanged", firstAttempt, err)
	}
	if !reflect.DeepEqual(variables, original) {
		t.Fatal("feedback enrichment mutated its source input")
	}

	enriched, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, reader, runID, "statement", 2, attemptAt, variables)
	if err != nil {
		t.Fatalf("enrich retry variables: %v", err)
	}
	if !reflect.DeepEqual(variables, original) {
		t.Fatal("feedback enrichment changed the committed producer input")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(enriched, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["request"], json.RawMessage(`{"brief":"frozen"}`)) {
		t.Fatalf("immutable request changed: %s", got["request"])
	}
	var gotFeedback domain.DraftRetryFeedback
	if err := json.Unmarshal(got[draftRetryFeedbackField], &gotFeedback); err != nil {
		t.Fatal(err)
	}
	if gotFeedback != *feedback {
		t.Fatalf("retry feedback = %+v, want %+v", gotFeedback, *feedback)
	}

	for stage, wantVersion := range map[string]string{"idea": "v3", "statement": "v3", "solution": "v3", "data": "v4"} {
		prompt, schema, err := buildLLMDraftPromptForVariables(stage, workflow.ExecutedSamplesRevision, "v3", enriched)
		if err != nil {
			t.Fatalf("resolve %s retry prompt: %v", stage, err)
		}
		if prompt.Version != wantVersion {
			t.Errorf("%s retry prompt version = %s, want %s", stage, prompt.Version, wantVersion)
		}
		registry, _, err := builtinLLMRegistries()
		if err != nil {
			t.Fatal(err)
		}
		definition, err := registry.Resolve(port.PromptRef{Step: prompt.Step, Version: prompt.Version, TemplateDigest: prompt.TemplateDigest, InputSchemaVersion: prompt.InputSchemaVersion, SchemaVersion: prompt.SchemaVersion, SchemaDigest: prompt.SchemaDigest, MigrationPolicy: prompt.MigrationPolicy})
		if err != nil || definition.OutputSchema != schema {
			t.Fatalf("resolve registered %s retry prompt: %v", stage, err)
		}
		if !containsAll(definition.Template, "retry_feedback", "untrusted diagnostic data", "Never weaken a validator") {
			t.Errorf("%s retry prompt lacks diagnostic safety instructions", stage)
		}
	}
}

func TestDataOLEFeedbackUsesDataRepairAndKeepsOutputHeadroom(t *testing.T) {
	variables := []byte(`{"solution_input":{"problem":{"title":"Tree"}},"source_input_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	feedback := &domain.DraftRetryFeedback{SourceStage: "judge", TargetStage: "data", Reason: "data_requires_review:generated.4.run.1.OLE"}
	reader := fixedDraftFeedbackReader{feedback: feedback, stage: "data"}
	runID := domain.RunID("run_00000000000000000000000000000002")
	attemptAt := time.Date(2026, 9, 30, 1, 19, 0, 0, time.UTC)

	for _, tc := range []struct{ dataVersion, wantPrompt string }{{"v3", "v4"}, {"v5", "v6"}} {
		enriched, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, reader, runID, "data", 4, attemptAt, variables)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(enriched, &object); err != nil {
			t.Fatal(err)
		}
		var got domain.DraftRetryFeedback
		if err := json.Unmarshal(object[draftRetryFeedbackField], &got); err != nil || got != *feedback {
			t.Fatalf("data retry feedback = %+v, %v", got, err)
		}
		prompt, _, err := buildLLMDraftPromptForVariables("data", workflow.ExecutedSamplesRevision, tc.dataVersion, enriched)
		if err != nil || prompt.Version != tc.wantPrompt {
			t.Fatalf("data retry prompt = %+v, %v; want %s", prompt, err, tc.wantPrompt)
		}
		registry, _, err := builtinLLMRegistries()
		if err != nil {
			t.Fatal(err)
		}
		definition, err := registry.Resolve(prompt)
		if err != nil || !containsAll(definition.Template, "retry_feedback", "Never weaken a validator") {
			t.Fatalf("Data retry prompt does not retain OLE feedback: %v", err)
		}
		if tc.dataVersion == "v5" && !containsAll(definition.Template, "943718 bytes", "Never rely on truncation") {
			t.Fatal("V6 dropped V5's output headroom rule")
		}
	}
}

func TestDraftRetryFeedbackReconstructsHistoricalStatementAfterLaterDataRevision(t *testing.T) {
	base := time.Date(2026, 9, 30, 1, 17, 51, 0, time.UTC)
	events := timelineDraftFeedbackReader{
		{at: base, feedback: domain.DraftRetryFeedback{SourceStage: "solution_decision", TargetStage: "statement", Reason: "automatic solution retry: sample mismatch"}},
		{at: base.Add(time.Second), feedback: domain.DraftRetryFeedback{SourceStage: "judge", TargetStage: "statement", Reason: "manual statement revision: correct sample format"}},
		{at: base.Add(30 * time.Minute), feedback: domain.DraftRetryFeedback{SourceStage: "judge", TargetStage: "data", Reason: "generated.4.run.1.OLE: keep output below 943718 bytes"}},
	}
	statementAttemptAt := base.Add(2 * time.Second)
	variables := []byte(`{"problem":{"statement":"frozen"},"source_input_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	runID := domain.RunID("run_00000000000000000000000000000003")

	statementVariables, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, events, runID, "statement", 3, statementAttemptAt, variables)
	if err != nil {
		t.Fatal(err)
	}
	var statementObject map[string]json.RawMessage
	if err := json.Unmarshal(statementVariables, &statementObject); err != nil {
		t.Fatal(err)
	}
	var statementFeedback domain.DraftRetryFeedback
	if err := json.Unmarshal(statementObject[draftRetryFeedbackField], &statementFeedback); err != nil {
		t.Fatal(err)
	}
	if statementFeedback.Reason != "manual statement revision: correct sample format" {
		t.Fatalf("historical statement feedback = %+v; want the feedback effective at attempt start", statementFeedback)
	}
	statementPrompt, _, err := buildLLMDraftPromptForVariables("statement", workflow.ExecutedSamplesRevision, "v5", statementVariables)
	if err != nil || statementPrompt.Version != "v3" {
		t.Fatalf("historical statement prompt = %+v, %v; want v3", statementPrompt, err)
	}

	// A later read of the historical statement must still select the same
	// feedback, even though the run now has a newer revision for Data.
	replayed, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, events, runID, "statement", 3, statementAttemptAt, variables)
	if err != nil || string(replayed) != string(statementVariables) {
		t.Fatalf("historical statement replay differs: %s, %v", replayed, err)
	}

	dataAttemptAt := base.Add(time.Hour)
	dataVariables, err := addDraftRetryFeedback(context.Background(), workflow.ExecutedSamplesRevision, events, runID, "data", 4, dataAttemptAt, variables)
	if err != nil {
		t.Fatal(err)
	}
	dataPrompt, _, err := buildLLMDraftPromptForVariables("data", workflow.ExecutedSamplesRevision, "v5", dataVariables)
	if err != nil || dataPrompt.Version != "v6" {
		t.Fatalf("current data prompt = %+v, %v; want v6", dataPrompt, err)
	}
	registry, _, err := builtinLLMRegistries()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := registry.Resolve(dataPrompt)
	if err != nil || !containsAll(definition.Template, "943718 bytes", "Never rely on truncation", "retry_feedback") {
		t.Fatalf("latest OLE prompt lost its output cap or feedback: %v", err)
	}
	if string(statementObject["problem"]) != `{"statement":"frozen"}` {
		t.Fatalf("historical feedback changed the frozen source input: %s", statementObject["problem"])
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
