package port

import (
	"errors"
	"math"
	"testing"

	"cpgen/internal/domain"
)

func testPromptVersion() PromptVersion {
	const template = "Generate one candidate."
	return PromptVersion{
		Step:           "idea",
		Version:        "v1",
		Template:       template,
		TemplateDigest: TemplateDigestFor(template),
		OutputSchema: OutputSchemaRef{
			SchemaVersion: domain.SchemaVersion("cpgen.idea/v1"),
			Digest:        domain.SumBytes([]byte("idea-schema-v1")),
		},
		MigrationPolicy: "NONE",
	}
}

func TestPromptRegistryRejectsDuplicateAndUnknownVersion(t *testing.T) {
	definition := testPromptVersion()
	registry, err := NewPromptRegistry(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); err == nil {
		t.Fatal("duplicate prompt version was accepted")
	} else {
		var typed *PromptRegistryError
		if !errors.As(err, &typed) || typed.Code != PromptDuplicateVersion {
			t.Fatalf("duplicate error = %T %v", err, err)
		}
	}
	ref := definition.Ref()
	ref.Version = "v2"
	if _, err := registry.Lookup(ref); err == nil {
		t.Fatal("unknown prompt version was accepted")
	} else {
		var typed *PromptRegistryError
		if !errors.As(err, &typed) || typed.Code != PromptUnknownVersion {
			t.Fatalf("unknown error = %T %v", err, err)
		}
	}
	ref = definition.Ref()
	ref.TemplateDigest = domain.SumBytes([]byte("different"))
	if _, err := registry.Lookup(ref); err == nil {
		t.Fatal("stale template digest was accepted")
	}
}

func TestPromptRegistryValidatesImmutableTemplateDigestAndResolvesLegacyRef(t *testing.T) {
	definition := testPromptVersion()
	definition.TemplateDigest = domain.SumBytes([]byte("wrong"))
	if _, err := NewPromptRegistry(definition); err == nil {
		t.Fatal("definition with stale template digest was accepted")
	}
	definition = testPromptVersion()
	registry, err := NewPromptRegistry(definition)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve(PromptRef{
		Step:          definition.Step,
		Version:       definition.Version,
		Digest:        definition.TemplateDigest,
		SchemaVersion: definition.OutputSchema.SchemaVersion,
		SchemaDigest:  definition.OutputSchema.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Template != definition.Template {
		t.Fatalf("resolved template = %q", resolved.Template)
	}
	if got := registry.Versions(); len(got) != 1 || got[0] != definition.Ref() {
		t.Fatalf("versions = %#v", got)
	}
}

func TestPromptRegistryBindsLegacyPromptToGenerateRequestSchema(t *testing.T) {
	definition := testPromptVersion()
	registry, err := NewPromptRegistry(definition)
	if err != nil {
		t.Fatal(err)
	}
	request := GenerateRequest{
		Prompt:    PromptRef{Step: definition.Step, Version: definition.Version, Digest: definition.TemplateDigest},
		Schema:    definition.OutputSchema,
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  SamplingPolicy{TopP: 1},
		MaxOutput: OutputLimit{Tokens: 10, Bytes: 1024},
	}
	if _, err := registry.ResolveRequest(request); err != nil {
		t.Fatalf("legacy request did not resolve: %v", err)
	}
	request.Schema.Digest = domain.SumBytes([]byte("other-schema"))
	if _, err := registry.ResolveRequest(request); err == nil {
		t.Fatal("request with stale schema identity resolved")
	}
	request = GenerateRequest{
		Prompt:    PromptRef{Step: definition.Step, Version: definition.Version, Digest: definition.TemplateDigest, SchemaDigest: definition.OutputSchema.Digest},
		Schema:    definition.OutputSchema,
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  SamplingPolicy{TopP: 1},
		MaxOutput: OutputLimit{Tokens: 10, Bytes: 1024},
	}
	if _, err := registry.ResolveRequest(request); err == nil {
		t.Fatal("prompt schema digest without schema version was accepted")
	}
}

func TestGenerateRequestRejectsSchemaIdentityMismatchAndNonFiniteSampling(t *testing.T) {
	definition := testPromptVersion()
	base := GenerateRequest{
		Prompt:    PromptRef{Step: definition.Step, Version: definition.Version, Digest: definition.TemplateDigest, SchemaVersion: definition.OutputSchema.SchemaVersion, SchemaDigest: definition.OutputSchema.Digest},
		Schema:    definition.OutputSchema,
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  SamplingPolicy{TopP: 1},
		MaxOutput: OutputLimit{Tokens: 10, Bytes: 1024},
	}
	for name, mutate := range map[string]func(*GenerateRequest){
		"schema version":    func(value *GenerateRequest) { value.Prompt.SchemaVersion = "cpgen.idea/v2" },
		"schema digest":     func(value *GenerateRequest) { value.Prompt.SchemaDigest = domain.SumBytes([]byte("other")) },
		"nan":               func(value *GenerateRequest) { value.Sampling.Temperature = math.NaN() },
		"positive infinity": func(value *GenerateRequest) { value.Sampling.TopP = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}
