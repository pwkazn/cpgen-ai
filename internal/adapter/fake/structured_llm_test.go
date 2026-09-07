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
	strictFixtureValidator := func(raw []byte, schema port.OutputSchemaRef, maxBytes int64) error {
		var value struct {
			SchemaVersion string `json:"schema_version"`
			Title         string `json:"title" required:"true"`
		}
		return port.DecodeStructuredOutput(raw, schema.SchemaVersion, maxBytes, &value)
	}
	model := NewStructuredLLMWithValidator(strictFixtureValidator, fixture)
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
	request.Variables[2] = 'X'
	stored := model.Requests()
	if string(stored[0].Variables) != `{"brief":"fixture"}` {
		t.Fatalf("stored variables aliased caller memory: %s", stored[0].Variables)
	}
	stored[0].Variables[2] = 'Y'
	if string(model.Requests()[0].Variables) != `{"brief":"fixture"}` {
		t.Fatal("Requests returned an aliased variables buffer")
	}
	unbound := NewStructuredLLM(fixture)
	_, err = unbound.Generate(context.Background(), request)
	var unboundErr *port.StructuredOutputError
	if !errors.As(err, &unboundErr) || unboundErr.Code != port.StructuredOutputSchemaUnbound {
		t.Fatalf("unbound default validator error = %T %v", err, err)
	}
	second, err := model.Generate(context.Background(), request)
	if err == nil || second.Value != nil {
		t.Fatalf("depleted fixture = %#v, err = %v", second, err)
	}

	invalid := NewStructuredLLMWithValidator(strictFixtureValidator, []byte(`{"schema_version":"cpgen.idea/v1","title":"fixture","title":"secret"}`))
	_, err = invalid.Generate(context.Background(), request)
	var typed *port.StructuredOutputError
	if !errors.As(err, &typed) || typed.Code != port.StructuredOutputDuplicateField {
		t.Fatalf("invalid fixture error = %T %v", err, err)
	}
	validatorModel := NewStructuredLLMWithValidator(func(raw []byte, schema port.OutputSchemaRef, maxBytes int64) error {
		var value struct {
			SchemaVersion string `json:"schema_version"`
			Title         string `json:"title" required:"true"`
		}
		return port.DecodeStructuredOutput(raw, schema.SchemaVersion, maxBytes, &value)
	}, []byte(`{"schema_version":"cpgen.idea/v1","title":"ok","secret":"not allowed"}`))
	_, err = validatorModel.Generate(context.Background(), request)
	if !errors.As(err, &typed) || typed.Code != port.StructuredOutputUnknownField {
		t.Fatalf("typed validator error = %T %v", err, err)
	}
}

func TestStructuredLLMValidatorCannotOptOutOfGenericBoundary(t *testing.T) {
	request := port.GenerateRequest{
		Prompt:    port.PromptRef{Step: "idea", Version: "v1", Digest: domain.SumBytes([]byte("prompt"))},
		Schema:    port.OutputSchemaRef{SchemaVersion: "cpgen.idea/v1", Digest: domain.SumBytes([]byte("schema"))},
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  port.SamplingPolicy{TopP: 1},
		MaxOutput: port.OutputLimit{Tokens: 10, Bytes: 1024},
	}
	model := NewStructuredLLMWithValidator(func([]byte, port.OutputSchemaRef, int64) error {
		return nil
	}, []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","title":"secret"}`))
	_, err := model.Generate(context.Background(), request)
	var typed *port.StructuredOutputError
	if !errors.As(err, &typed) || typed.Code != port.StructuredOutputDuplicateField {
		t.Fatalf("callback bypassed generic boundary: %T %v", err, err)
	}
}
