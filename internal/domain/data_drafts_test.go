package domain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func dataDraftInputFixture(t *testing.T) DataDraftInputV1 {
	t.Helper()
	input := solutionDraftInputFixture(t)
	solution, err := validSolutionDraft().Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := NewDataDraftInput(input, solution, SumBytes([]byte("committed passing sample verification")))
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func validDataDraft() DataDraftV1 {
	return DataDraftV1{SchemaVersion: DataDraftSchemaV1, GeneratorCode: "// café\r\nint main() { return 0; }\n", ValidatorCode: "int main() { return 3; }\n", Cases: []DataCaseDraft{
		{Kind: DataCaseSmall, Purpose: "Minimal connected instance"},
		{Kind: DataCaseSmall, Purpose: "Small disconnected random instance"},
		{Kind: DataCaseBoundary, Purpose: "Empty edge set"},
		{Kind: DataCaseStress, Purpose: "Maximum supported vertex and edge counts"},
	}}
}

func TestDataDraftBindsVerifiedSolutionAndDerivesStableSeeds(t *testing.T) {
	input, draft := dataDraftInputFixture(t), validDataDraft()
	content, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	if content.GeneratorCode != draft.GeneratorCode || content.ValidatorCode != draft.ValidatorCode || content.SolutionContentDigest != input.Solution.ContentDigest || content.SolutionVerificationDigest != input.SolutionVerificationDigest || content.Plan.EffectiveSeed != input.SolutionInput.Snapshot.EffectiveSeed {
		t.Fatal("data binding changed bytes or lost solution/seed provenance")
	}
	raw, err := content.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	again, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := again.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, second) {
		t.Fatal("data plan changed across identical binding")
	}
	seeds := make(map[uint64]bool)
	for ordinal, item := range content.Plan.Cases {
		if item.Ordinal != ordinal+1 || seeds[item.Seed] {
			t.Fatal("host case ordinals/seeds are not distinct")
		}
		seeds[item.Seed] = true
	}
	input.SolutionVerificationDigest = SumBytes([]byte("different verification"))
	if content.ValidateInput(input) == nil {
		t.Fatal("data accepts a different solution verification")
	}
	draft.Cases[0].Purpose = "Changed after binding"
	if content.Validate() != nil {
		t.Fatal("bound plan aliases model case array")
	}
}

func TestDataDraftRejectsUnboundedPlansAndProviderOwnedExecution(t *testing.T) {
	raw, err := json.Marshal(validDataDraft())
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"verdict":           append([]byte(`{"passed":true,`), raw[1:]...),
		"digest":            append([]byte(`{"content_digest":"forged",`), raw[1:]...),
		"seed":              []byte(strings.Replace(string(raw), `"kind":"small"`, `"seed":123,"kind":"small"`, 1)),
		"argv":              []byte(strings.Replace(string(raw), `"kind":"small"`, `"args":["--override"],"kind":"small"`, 1)),
		"duplicate":         append([]byte(`{"schema_version":"cpgen.data-draft/v1",`), raw[1:]...),
		"missing validator": []byte(strings.Replace(string(raw), `"validator_code"`, `"Validator_code"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			var decoded DataDraftV1
			if json.Unmarshal(candidate, &decoded) == nil {
				t.Fatal("untrusted execution field accepted")
			}
		})
	}
	for _, change := range []func(*DataDraftV1){
		func(d *DataDraftV1) { d.Cases = nil },
		func(d *DataDraftV1) { d.Cases = d.Cases[:3] },
		func(d *DataDraftV1) { d.Cases = append(d.Cases, make([]DataCaseDraft, 9)...) },
		func(d *DataDraftV1) { d.Cases[0].Kind = DataCaseStress },
		func(d *DataDraftV1) { d.Cases[2].Kind = DataCaseSmall },
		func(d *DataDraftV1) { d.Cases[3].Kind = DataCaseSmall },
		func(d *DataDraftV1) { d.Cases[0].Purpose = "" },
		func(d *DataDraftV1) { d.Cases[1] = d.Cases[0] },
		func(d *DataDraftV1) { d.GeneratorCode = strings.Repeat("x", 262145) },
		func(d *DataDraftV1) { d.ValidatorCode = "x\x00y" },
	} {
		draft := validDataDraft()
		change(&draft)
		if draft.Validate() == nil {
			t.Fatal("invalid data proposal accepted")
		}
	}
}

func TestTestPlanRejectsSeedAndOrdinalSubstitution(t *testing.T) {
	content, err := validDataDraft().Bind(dataDraftInputFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*TestPlanV1){
		func(p *TestPlanV1) { p.EffectiveSeed++ },
		func(p *TestPlanV1) { p.Cases[0].Seed++ },
		func(p *TestPlanV1) { p.Cases[0].Ordinal = 2 },
		func(p *TestPlanV1) { p.Cases[0], p.Cases[1] = p.Cases[1], p.Cases[0] },
	} {
		plan := content.Plan
		plan.Cases = append([]DataCase(nil), plan.Cases...)
		change(&plan)
		if plan.Validate() == nil {
			t.Fatal("substituted seed/ordering accepted")
		}
	}
	content.GeneratorCode += "// changed"
	if content.Validate() == nil {
		t.Fatal("modified generator kept original content identity")
	}
}
