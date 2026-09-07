package port

import (
	"errors"
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
