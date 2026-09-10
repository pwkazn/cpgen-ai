package domain

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"
)

func mutationContentFixture(t *testing.T) (GenerationRequestSnapshotV1, IdeaBatch, IdeaMutationParameters) {
	t.Helper()
	request := testGenerationRequest()
	seed := int64(9007199254740993)
	request.Seed = &seed
	request.BudgetLimits.MaxMutationsPerStage = 8
	snapshot, err := NewGenerationRequestSnapshotV1(request, seed)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	source, err := validIdeaDraft().Bind(initial)
	if err != nil {
		t.Fatal(err)
	}
	parameters := IdeaMutationParameters{RunID: "run_0123456789abcdef0123456789abcdef", WorkflowRevision: "mutation-contract-fixture/v1",
		ConfigDigest: SumBytes([]byte("frozen-config")), TriggerKind: IdeaMutationSimilarity,
		TriggerEvidenceDigest: SumBytes([]byte("verified-rejection-decision")), ParentIdeaID: source.Candidates[0].IdeaID, MutationOrdinal: 1,
	}
	return snapshot, source, parameters
}

// Pure contract tests use an explicitly synthetic grant. Application acceptance
// separately binds the same core to an actual SQLite claim and checks its hash.
func mutationIntentFixture(t *testing.T, core IdeaMutationCore) IdeaMutationIntent {
	t.Helper()
	grant := MutationGrant{ClaimID: "mutation_claim_fixture", RunID: core.RunID, StageName: "idea", ScopeDigest: core.StageScopeDigest,
		SourceBatchDigest: core.SourceBatchDigest, Ordinal: core.MutationOrdinal, LimitSnapshot: core.LimitSnapshot,
		Kind: MutationContent, IntentDigest: core.CoreDigest,
	}
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	grant.GrantDigest = SumBytes(raw)
	intent, err := NewIdeaMutationIntent(core, grant)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func TestIdeaMutationCoreBreaksClaimHashCycleAndRetainsExactIdentity(t *testing.T) {
	snapshot, source, parameters := mutationContentFixture(t)
	core, err := NewIdeaMutationCore(snapshot, source, parameters)
	if err != nil {
		t.Fatal(err)
	}
	command, err := core.ClaimRequest(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	if err != nil || command.IntentDigest != core.CoreDigest || command.Ordinal != 1 || command.LimitSnapshot != 8 || command.Kind != MutationContent {
		t.Fatalf("command=%+v %v", command, err)
	}
	intent := mutationIntentFixture(t, core)
	if intent.IntentDigest == core.CoreDigest || intent.Grant.IntentDigest != core.CoreDigest {
		t.Fatal("grant and authorized intent have circular identity")
	}
	input, err := NewIdeaMutationDraftInput(snapshot, source, intent)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := validIdeaDraft().BindMutation(input)
	if err != nil {
		t.Fatal(err)
	}
	if batch.BatchOrdinal != 1 || batch.EffectiveSeed != 9007199254740993 || batch.CallBudget != snapshot.Request.BudgetLimits {
		t.Fatalf("mutation lost frozen fields: %+v", batch)
	}
	for ordinal, candidate := range batch.Candidates {
		if candidate.ParentIdeaID != parameters.ParentIdeaID || candidate.MutationOrdinal != ordinal+1 || candidate.MutationReason != "similarity_rejected" || candidate.IdeaID == source.Candidates[ordinal].IdeaID || !reflect.DeepEqual(candidate.SeedAxes, input.Candidates[ordinal].SeedAxes) {
			t.Fatalf("lineage=%+v", candidate)
		}
	}
	raw, err := input.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var reopened IdeaMutationDraftInputV1
	if err := json.Unmarshal(raw, &reopened); err != nil {
		t.Fatal(err)
	}
	replayed, err := validIdeaDraft().BindMutation(reopened)
	if err != nil || !reflect.DeepEqual(batch, replayed) {
		t.Fatalf("restart changed deterministic batch: %v", err)
	}
	selection, err := NewIdeaSelection(snapshot.RequestDigest, batch, batch.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"new_batch"}, []Digest{intent.IntentDigest})
	if err != nil || selection.IdeaBatchDigest != batch.BatchDigest {
		t.Fatalf("new batch selection=%+v %v", selection, err)
	}
	if _, err := NewIdeaSelection(snapshot.RequestDigest, batch, parameters.ParentIdeaID, SelectionOrdinalPolicyV1, []string{"old_parent"}, nil); err == nil {
		t.Fatal("old selection chose a new batch")
	}
	// Construction and output must own their slices, including the explicit seed.
	*snapshot.Request.Seed = 4
	source.Candidates[0].AbstractTask = "changed source"
	if err := input.Validate(); err != nil {
		t.Fatalf("mutation input aliases caller data: %v", err)
	}
	input.Candidates[0].SeedAxes[0] = "changed axes"
	if err := batch.Validate(); err != nil {
		t.Fatalf("mutation output aliases input data: %v", err)
	}
}

func TestIdeaMutationChildRangesRemainUniqueAndRejectOverflow(t *testing.T) {
	snapshot, source, parameters := mutationContentFixture(t)
	seen := map[int]bool{}
	for claim := int64(1); claim <= 8; claim++ {
		parameters.MutationOrdinal = claim
		core, err := NewIdeaMutationCore(snapshot, source, parameters)
		if err != nil {
			t.Fatal(err)
		}
		for ordinal := range core.RequestedCount {
			child, err := core.ChildMutationOrdinal(ordinal)
			if err != nil || seen[child] || child != int((claim-1)*8)+ordinal+1 {
				t.Fatalf("overlapping child=%d %v", child, err)
			}
			seen[child] = true
		}
		if _, err := core.ChildMutationOrdinal(2); err == nil {
			t.Fatal("out-of-batch child admitted")
		}
	}
	core, err := NewIdeaMutationCore(snapshot, source, parameters)
	if err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []func(*IdeaMutationCore){
		func(c *IdeaMutationCore) { c.MutationOrdinal = 0 },
		func(c *IdeaMutationCore) { c.MutationOrdinal = 9 },
		func(c *IdeaMutationCore) {
			c.MutationOrdinal = int64(math.MaxInt)/8 + 1
			c.LimitSnapshot = math.MaxInt64
		},
		func(c *IdeaMutationCore) { c.SourceBatchOrdinal = math.MaxInt },
	} {
		bad := core
		corrupt(&bad)
		bad.CoreDigest = ""
		bad.CoreDigest = contentSum(bad)
		if bad.Validate() == nil {
			t.Fatalf("invalid ordinal accepted: %+v", bad)
		}
	}
}

func TestIdeaMutationRejectsCrossedSourceTriggerAndGrant(t *testing.T) {
	snapshot, source, parameters := mutationContentFixture(t)
	core, err := NewIdeaMutationCore(snapshot, source, parameters)
	if err != nil {
		t.Fatal(err)
	}
	intent := mutationIntentFixture(t, core)
	for name, corrupt := range map[string]func(*MutationGrant){
		"run":     func(g *MutationGrant) { g.RunID = "run_0123456789abcdef0123456789abcdea" },
		"kind":    func(g *MutationGrant) { g.Kind = MutationMetadata },
		"core":    func(g *MutationGrant) { g.IntentDigest = SumBytes([]byte("other core")) },
		"source":  func(g *MutationGrant) { g.SourceBatchDigest = SumBytes([]byte("other batch")) },
		"scope":   func(g *MutationGrant) { g.ScopeDigest = SumBytes([]byte("other scope")) },
		"ordinal": func(g *MutationGrant) { g.Ordinal = 2 },
		"limit":   func(g *MutationGrant) { g.LimitSnapshot = 9 },
	} {
		grant := intent.Grant
		corrupt(&grant)
		grant.GrantDigest = ""
		raw, err := json.Marshal(grant)
		if err != nil {
			t.Fatal(err)
		}
		grant.GrantDigest = SumBytes(raw)
		if _, err := NewIdeaMutationIntent(core, grant); err == nil {
			t.Fatalf("crossed %s grant admitted", name)
		}
	}
	wrong := source
	wrong.RequestDigest = SumBytes([]byte("different source request"))
	if _, err := NewIdeaMutationDraftInput(snapshot, wrong, intent); err == nil {
		t.Fatal("crossed source batch admitted")
	}
	parameters.ParentIdeaID = "idea:" + string(SumBytes([]byte("foreign parent")))
	if _, err := NewIdeaMutationCore(snapshot, source, parameters); err == nil {
		t.Fatal("foreign parent admitted")
	}
	parameters.ParentIdeaID = ""
	parameters.TriggerKind = IdeaMutationNoFeasible
	if _, err := NewIdeaMutationCore(snapshot, source, parameters); err == nil {
		t.Fatal("no-feasible trigger accepted a feasible source")
	}
	parameters.TriggerKind = "UNKNOWN"
	if _, err := NewIdeaMutationCore(snapshot, source, parameters); err == nil {
		t.Fatal("unknown trigger admitted")
	}
}

func TestIdeaMutationNoFeasibleEvidenceBindsWholeRecordedBatch(t *testing.T) {
	snapshot, _, parameters := mutationContentFixture(t)
	initial, err := NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	draft := validIdeaDraft()
	for index := range draft.Candidates {
		draft.Candidates[index].FeasibilityStatus = "REJECTED"
		draft.Candidates[index].FeasibilityReasons = []string{"recorded_constraint_failure"}
	}
	source, err := draft.Bind(initial)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := NoFeasibleIdeaEvidence(source)
	if err != nil {
		t.Fatal(err)
	}
	parameters.ParentIdeaID, parameters.TriggerKind, parameters.TriggerEvidenceDigest = "", IdeaMutationNoFeasible, evidence
	core, err := NewIdeaMutationCore(snapshot, source, parameters)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewIdeaMutationDraftInput(snapshot, source, mutationIntentFixture(t, core))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := validIdeaDraft().BindMutation(input)
	if err != nil || len(batch.FeasibleCandidateIDs()) != 2 {
		t.Fatalf("regenerated batch=%+v %v", batch, err)
	}
	for _, candidate := range batch.Candidates {
		if candidate.ParentIdeaID != "" || candidate.MutationOrdinal == 0 {
			t.Fatal("parentless regeneration lost lineage")
		}
	}
	parameters.TriggerEvidenceDigest = SumBytes([]byte("unrelated report"))
	if _, err := NewIdeaMutationCore(snapshot, source, parameters); err == nil {
		t.Fatal("unrelated no-feasible evidence accepted")
	}
}

func TestIdeaMutationDraftRejectsForgedLineageAndStrictEnvelope(t *testing.T) {
	snapshot, source, parameters := mutationContentFixture(t)
	core, err := NewIdeaMutationCore(snapshot, source, parameters)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewIdeaMutationDraftInput(snapshot, source, mutationIntentFixture(t, core))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := input.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"initial schema": bytes.Replace(raw, []byte(IdeaMutationDraftInputSchemaV1), []byte(IdeaDraftInputSchemaV1), 1),
		"duplicate":      bytes.Replace(raw, []byte(`"batch_ordinal":1`), []byte(`"batch_ordinal":2,"batch_ordinal":1`), 1),
		"alias":          bytes.Replace(raw, []byte(`"intent":`), []byte(`"INTENT":`), 1),
		"unknown":        bytes.Replace(raw, []byte(`"intent":`), []byte(`"authorization_override":true,"intent":`), 1),
		"lineage":        bytes.Replace(raw, []byte(`"mutation_ordinal":1`), []byte(`"mutation_ordinal":2`), 1),
	} {
		var decoded IdeaMutationDraftInputV1
		if err := json.Unmarshal(data, &decoded); err == nil {
			t.Fatalf("%s mutation envelope admitted", name)
		}
	}
	input.Candidates[0].MutationOrdinal = input.Candidates[1].MutationOrdinal
	if _, err := validIdeaDraft().BindMutation(input); err == nil {
		t.Fatal("provider-controlled lineage admitted")
	}
}
