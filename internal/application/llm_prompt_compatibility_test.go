package application_test

import (
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

func TestFrozenDraftAndRepairPromptIdentities(t *testing.T) {
	// Historical hashes come from the pre-executed-samples source, including
	// its exact schema definitions. V3 hashes freeze already-persisted runs.
	// Neither set is computed from the registry being tested.
	checks := []struct {
		stage, oldPrompt, currentPrompt, oldSchema, currentSchema, oldRepair, currentRepair string
	}{
		{"idea",
			"d9bf5b2536abdf5372cb8f7e826592e04716af9000a27ef4ccd5b02b808064cd", "d9bf5b2536abdf5372cb8f7e826592e04716af9000a27ef4ccd5b02b808064cd",
			"29b91439dffa8e3a9d2943df5ada0b5d778e51dea9a35e9d80e27b04dc121035", "29b91439dffa8e3a9d2943df5ada0b5d778e51dea9a35e9d80e27b04dc121035",
			"fad4d2e9f90f0a226789bb5f687023adbd36096fcda44e40773ace64762e7f0e", "fad4d2e9f90f0a226789bb5f687023adbd36096fcda44e40773ace64762e7f0e"},
		{"statement",
			"6af84deab4ea89f48d7c268bae84a3fa6929d1c1f0bba4f568f45fad659b85c6", "db41a402cf9565b43bd358e7e7759a795e78614d7124d8ce6a4fcdf52c1d7416",
			"20aaa77b8da70956ed47137c853235abfec15cd497ee296af273ed7ce84ce70a", "20aaa77b8da70956ed47137c853235abfec15cd497ee296af273ed7ce84ce70a",
			"630bca1cfa824d78b89d66651c670021c2d4f120fada176529a6337063e57bce", "f95a07555d8c8ba11bbddab89d585bdbd53937522b628d29fb668d4d75f3e911"},
		{"solution",
			"9cfe865befce99012b59949f1fed11c072b47e922f9d2150de43930682fbd2c3", "be9975eaac508416f90a4345643b7c4a0cb8fb15626bb20fe5ed9aea19d25576",
			"22e588f141ce5ab04ec1caab2e2f4868eefb9450f31fec54289e02f8b64d6b23", "95837c67e381d3610597c3b59947fc0142f4cfe3a1a50a2f104304e97feb836b",
			"b15f60edcec6bb00bca8f02ca373dff92a16f01b414ea8a9d81e2d5f28c84821", "655fe31f6709c4cecd93106c4291e902c745b58fb0b91eb525e4951233b640b7"},
		{"data",
			"2a40a633e364c5aeb4365becd3c6d264f0db88e36589fcf3444ba0ea3a54e695", "e8be5799625ff021e94dae053a0160b0c5ab2104fe48b94eb3785955200a10ee",
			"c405b804205ebd9d4a84808f049bae88dab8c86cce3f583774971e8117ef6014", "c405b804205ebd9d4a84808f049bae88dab8c86cce3f583774971e8117ef6014",
			"49878ab50c983ef51f7d38c67db00bd8ff009f9ab6295d484910da5e68efc409", "596a53688cc943eeac8f009b4c02b04b1c2ec75038b3ed65d6cc707063be1592"},
	}
	for _, revision := range []string{workflow.GenerationRevision, workflow.RetryingGenerationRevision, workflow.ExecutedSamplesRevision} {
		for _, tc := range checks {
			t.Run(revision+"/"+tc.stage, func(t *testing.T) {
				version, promptHash, schemaHash, repairHash := "v1", tc.oldPrompt, tc.oldSchema, tc.oldRepair
				inputSchema := "cpgen." + tc.stage + "-draft-input/v1"
				if revision == workflow.ExecutedSamplesRevision {
					version, promptHash, schemaHash, repairHash = "v2", tc.currentPrompt, tc.currentSchema, tc.currentRepair
					if tc.stage == "solution" || tc.stage == "data" {
						inputSchema = "cpgen.program-context/v1"
					}
				}
				wantSchema := port.OutputSchemaRef{SchemaVersion: domain.SchemaVersion("cpgen." + tc.stage + "-draft/v1"), Digest: domain.Digest("sha256:" + schemaHash)}
				wantPrompt := port.PromptRef{Step: tc.stage + ".draft", Version: version, TemplateDigest: domain.Digest("sha256:" + promptHash), InputSchemaVersion: domain.SchemaVersion(inputSchema), SchemaVersion: wantSchema.SchemaVersion, SchemaDigest: wantSchema.Digest, MigrationPolicy: "NONE"}
				prompt, schema, err := application.BuildLLMDraftPromptForWorkflow(tc.stage, revision)
				if err != nil || prompt != wantPrompt || schema != wantSchema {
					t.Fatalf("frozen draft identity drifted: prompt=%+v schema=%+v err=%v", prompt, schema, err)
				}
				cfg := llmApplicationConfig(t)
				cfg.Workflow = &config.WorkflowConfig{Revision: revision}
				cfg.LLM.MaxFormatRepairs = 1
				repair, err := application.BuildFormatRepairPolicy(cfg, tc.stage+".draft")
				wantRepair := wantPrompt
				wantRepair.Step += ".format-repair"
				wantRepair.TemplateDigest = domain.Digest("sha256:" + repairHash)
				wantRepair.InputSchemaVersion = "cpgen.format-repair-input/v1"
				if err != nil || repair.MaxRepairs != 1 || repair.Prompt != wantRepair {
					t.Fatalf("frozen repair identity drifted: repair=%+v err=%v", repair, err)
				}
				mapped, _, err := application.BuildLLMConfig(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := mapped.SchemaRegistry.ValidateBinding(wantSchema); err != nil {
					t.Fatalf("frozen schema is unavailable: %v", err)
				}
				for _, ref := range []port.PromptRef{wantPrompt, wantRepair} {
					if _, err := mapped.PromptRegistry.Resolve(ref); err != nil {
						t.Fatalf("frozen prompt is unavailable: %v", err)
					}
				}
			})
		}
	}
}
