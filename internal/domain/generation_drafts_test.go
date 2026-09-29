package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestIdeaDraftDerivesIdentityAndFrozenConstraintsLocally(t *testing.T) {
	snapshot, _, _, _, _ := testContentChain(t)
	draft := validIdeaDraft()
	input, err := NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	if batch.RequestDigest != snapshot.RequestDigest || batch.CallBudget != snapshot.Request.BudgetLimits || batch.EffectiveSeed != snapshot.EffectiveSeed || batch.RequestedCount != 2 {
		t.Fatalf("lost bindings: %+v", batch)
	}
	for index, candidate := range batch.Candidates {
		if candidate.IdeaID == "" || candidate.CandidateOrdinal != index || !reflect.DeepEqual(candidate.SeedAxes, input.Candidates[index].SeedAxes) || !reflect.DeepEqual(candidate.NegativeConstraints, cleanSet(snapshot.Request.ForbiddenFeatures)) {
			t.Fatalf("candidate=%+v", candidate)
		}
	}
	replayed, err := draft.Bind(input)
	if err != nil || !reflect.DeepEqual(batch, replayed) {
		t.Fatalf("replay differs: %v", err)
	}
	draft.Candidates[0].FeasibilityReasons[0] = "changed after binding"
	input.Candidates[0].SeedAxes[0] = "changed after binding"
	if err := batch.Validate(); err != nil {
		t.Fatalf("output aliases input: %v", err)
	}
}

func TestIdeaDraftInputCarriesFreeformTagsToPromptVariables(t *testing.T) {
	snapshot, err := NewGenerationRequestSnapshotV1(GenerationRequestV1{
		SchemaVersion: RequestSchemaV1, Mode: RequestModeManual, Brief: "Localized prompt fixture",
		Tags: []string{" 图论 ", "动态规划"}, NormalizedTags: []string{"动态规划", "图论"},
		Language: "zh-CN", Difficulty: "hard", TimeLimitMilliseconds: 2000,
		MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default",
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	variables, err := input.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range [][]byte{[]byte(`"tags":[" 图论 ","动态规划"]`), []byte(`"normalized_tags":["动态规划","图论"]`)} {
		if !bytes.Contains(variables, tag) {
			t.Fatalf("prompt variables omitted submitted tag data %s: %s", tag, variables)
		}
	}
}

func TestIdeaDraftRejectsWrongCountAndUntrustedSeedAxes(t *testing.T) {
	snapshot, _, _, _, _ := testContentChain(t)
	for _, count := range []int{1, 9} {
		if _, err := NewIdeaDraftInput(snapshot, count); err == nil {
			t.Fatalf("count=%d accepted", count)
		}
	}
	input, err := NewIdeaDraftInput(snapshot, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validIdeaDraft().Bind(input); err == nil {
		t.Fatal("provider changed requested candidate count")
	}
	input, err = NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	input.Candidates[0].SeedAxes[0] = "invented"
	if _, err := validIdeaDraft().Bind(input); err == nil {
		t.Fatal("untrusted seed axes admitted")
	}
}

func TestDraftDecodersRejectForgedIdentityAndMalformedContent(t *testing.T) {
	ideaRaw, err := json.Marshal(validIdeaDraft())
	if err != nil {
		t.Fatal(err)
	}
	statementRaw, err := json.Marshal(validStatementDraft())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"idea identity":               append([]byte(`{"batch_digest":"sha256:forged",`), ideaRaw[1:]...),
		"idea duplicate":              append([]byte(`{"schema_version":"cpgen.idea-draft/v1",`), ideaRaw[1:]...),
		"idea unknown nested":         []byte(strings.Replace(string(ideaRaw), `"abstract_task":`, `"idea_id":"forged","abstract_task":`, 1)),
		"idea missing candidates":     []byte(`{"schema_version":"cpgen.idea-draft/v1"}`),
		"idea malformed status":       []byte(strings.ReplaceAll(string(ideaRaw), "FEASIBLE", "GUESS")),
		"statement identity":          append([]byte(`{"request_digest":"sha256:forged",`), statementRaw[1:]...),
		"statement resource override": append([]byte(`{"time_limit_ms":9999,`), statementRaw[1:]...),
		"statement field alias":       []byte(strings.Replace(string(statementRaw), `"title":`, `"Title":`, 1)),
		"statement empty":             []byte(`{"schema_version":"cpgen.statement-draft/v1"}`),
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if strings.HasPrefix(name, "idea") {
				var draft IdeaDraftV1
				err = json.Unmarshal(raw, &draft)
			} else {
				var draft StatementDraftV1
				err = json.Unmarshal(raw, &draft)
			}
			if err == nil {
				t.Fatal("untrusted or malformed draft admitted")
			}
		})
	}
}

func TestStatementDraftBindsCompleteChainAndPreservesSampleBytes(t *testing.T) {
	snapshot, batch, selection, input, _ := testContentChain(t)
	draftInput, err := NewStatementDraftInput(input, snapshot, batch, selection)
	if err != nil {
		t.Fatal(err)
	}
	draft := validStatementDraft()
	draft.Samples[0].Input = " 2 1\n\n"
	draft.Samples[0].Output = "1 \n"
	problem, err := draft.Bind(draftInput, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := problem.ValidateChain(snapshot, batch, selection); err != nil {
		t.Fatal(err)
	}
	if problem.Samples[0].Input != " 2 1\n\n" || problem.Samples[0].Output != "1 \n" {
		t.Fatal("sample bytes changed")
	}
	if problem.Language != snapshot.Request.Language || problem.TimeLimitMS != snapshot.Request.TimeLimitMilliseconds || problem.IntendedAlgorithm != batch.Candidates[0].IntendedAlgorithm {
		t.Fatal("trusted fields lost")
	}
	draft.Samples[0].Input = "mutated"
	draft.Input.Fields[0] = "mutated"
	draftInput.Selection.SelectedIdeaID = batch.Candidates[1].IdeaID
	if err := problem.Validate(); err != nil {
		t.Fatalf("output aliases input: %v", err)
	}
	if _, err := draft.Bind(draftInput, 1); err == nil {
		t.Fatal("substituted selection admitted")
	}
}

func validIdeaDraft() IdeaDraftV1 {
	return IdeaDraftV1{SchemaVersion: IdeaDraftSchemaV1, Candidates: []IdeaContentDraft{
		{AbstractTask: "Find paths", IntendedAlgorithm: "BFS", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE", FeasibilityReasons: []string{"bounded graph"}},
		{AbstractTask: "Find components", IntendedAlgorithm: "DFS", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE", FeasibilityReasons: []string{}},
	}}
}

func validStatementDraft() StatementDraftV1 {
	return StatementDraftV1{SchemaVersion: StatementDraftSchemaV1, Title: "Paths", Description: "Find a path in a graph.", Input: ProblemIO{Description: "An edge.", Fields: []string{"u", "v"}}, Output: ProblemIO{Description: "Distance.", Fields: []string{"distance"}}, Samples: []ProblemSample{{Input: "1 2\n", Output: "1\n"}}}
}
