package application_test

import (
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
)

func TestMutationPromptCannotReuseInitialDraftContract(t *testing.T) {
	initial, initialSchema, err := application.BuildLLMDraftPrompt("idea")
	if err != nil {
		t.Fatal(err)
	}
	mutation, mutationSchema, err := application.BuildIdeaMutationPrompt()
	if err != nil || mutation.Step != "idea.mutate" || mutation.InputSchemaVersion != domain.IdeaMutationDraftInputSchemaV1 ||
		mutation.TemplateDigest == initial.TemplateDigest || mutationSchema.Digest == initialSchema.Digest {
		t.Fatalf("mutation prompt does not have independent binding identity: %+v %v", mutation, err)
	}
	mapped, _, err := application.BuildLLMConfig(llmApplicationConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	mutation.InputSchemaVersion = initial.InputSchemaVersion
	if _, err := mapped.PromptRegistry.Resolve(mutation); err == nil {
		t.Fatal("initial draft input identity selected the mutation prompt")
	}
	mutation, _, _ = application.BuildIdeaMutationPrompt()
	mutation.SchemaDigest = initial.SchemaDigest
	if _, err := mapped.PromptRegistry.Resolve(mutation); err == nil {
		t.Fatal("initial binding schema substituted for mutation binding")
	}
}
