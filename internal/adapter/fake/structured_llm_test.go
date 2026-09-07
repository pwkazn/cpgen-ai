package fake

import (
	"context"
	"errors"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestStructuredLLMIsDeterministicAndAppliesStrictBoundary(t *testing.T) {
	const schema = domain.SchemaVersion("cpgen.idea/v1")
	request := port.GenerateRequest{
		Prompt:    port.PromptRef{Step: "idea", Version: "v1", Digest: domain.SumBytes([]byte("prompt"))},
		Schema:    port.OutputSchemaRef{SchemaVersion: schema, Digest: domain.SumBytes([]byte("schema"))},
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  port.SamplingPolicy{TopP: 1},
		MaxOutput: port.OutputLimit{Tokens: 10, Bytes: 1024},
	}
	fixture := []byte(`{"schema_version":"cpgen.idea/v1","title":"fixture"}`)
	model := NewStructuredLLM(fixture)
	first, err := model.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value == nil || string(first.Value.Structured) != string(fixture) {
		t.Fatalf("first fixture = %#v", first.Value)
	}
	if got := model.Requests(); len(got) != 1 || got[0].Prompt.Step != "idea" {
		t.Fatalf("requests = %#v", got)
	}
	second, err := model.Generate(context.Background(), request)
	if err == nil || second.Value != nil {
		t.Fatalf("depleted fixture = %#v, err = %v", second, err)
	}

	invalid := NewStructuredLLM([]byte(`{"schema_version":"cpgen.idea/v1","title":"fixture","title":"secret"}`))
	_, err = invalid.Generate(context.Background(), request)
	var typed *port.StructuredOutputError
	if !errors.As(err, &typed) || typed.Code != port.StructuredOutputDuplicateField {
		t.Fatalf("invalid fixture error = %T %v", err, err)
	}
}
