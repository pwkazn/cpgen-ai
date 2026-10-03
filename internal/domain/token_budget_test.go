package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestTokenBudgetPreservesLegacyCanonicalBytes(t *testing.T) {
	// These bytes participate in immutable request, stage and artifact digests.
	const legacy = `{"max_llm_calls":12,"max_similarity_calls":2,"max_llm_input_tokens":2000000,"max_llm_output_tokens":100000,"max_llm_cost_micro_usd":1200000,"max_similarity_cost_micro_usd":200000,"max_sandbox_creates":256,"max_artifact_bytes":268435456,"max_package_bytes":67108864,"max_mutations_per_stage":0,"max_active_time_milliseconds":900000}`
	var limits domain.BudgetLimits
	if err := json.Unmarshal([]byte(legacy), &limits); err != nil {
		t.Fatal(err)
	}
	if limits.UsesTokenBudget() {
		t.Fatal("legacy limits acquired a shared allowance")
	}
	raw, err := json.Marshal(limits)
	if err != nil || string(raw) != legacy {
		t.Fatalf("legacy budget bytes changed: %s (%v)", raw, err)
	}
}

func TestExplicitZeroTokenBudgetSurvivesSnapshotRoundTrip(t *testing.T) {
	limits, err := domain.NewTokenBudgetLimits(0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	var restored domain.BudgetLimits
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.UsesTokenBudget() || restored.MaxLLMTokens != 0 || restored != limits {
		t.Fatalf("zero allowance lost its policy during persistence: %+v", restored)
	}
	if _, err := domain.NewTokenBudgetLimits(-1); err == nil {
		t.Fatal("negative token allowance accepted")
	}
}

func TestIndependentTokenBudgetPreservesZeroAndAsymmetricCaps(t *testing.T) {
	for _, caps := range [][2]int64{{0, 0}, {0, 50000}, {150000, 0}, {150000, 50000}} {
		limits, err := domain.NewSplitTokenBudgetLimits(caps[0], caps[1])
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(limits)
		if err != nil {
			t.Fatal(err)
		}
		var restored domain.BudgetLimits
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if restored != limits || !restored.SplitTokenBudget || restored.UsesTokenBudget() || restored.MaxLLMInputTokens != caps[0] || restored.MaxLLMOutputTokens != caps[1] {
			t.Fatalf("independent allowances lost their identity: %+v", restored)
		}
		restored.TokenBudget = true
		if err := restored.Validate(); err == nil {
			t.Fatal("a shared allowance was accepted on an independent-token request")
		}
	}
	for _, caps := range [][2]int64{{-1, 1}, {1, -1}} {
		if _, err := domain.NewSplitTokenBudgetLimits(caps[0], caps[1]); err == nil {
			t.Fatal("negative independent allowance accepted")
		}
	}
}

func TestTotalTokensCannotReplacePhysicalUsageEvidence(t *testing.T) {
	reservation := domain.BudgetReservation{
		ID: "res_00000000000000000000000000000001", RunID: "run_00000000000000000000000000000001",
		StageName: "prepare", AttemptID: "attempt_00000000000000000000000000000001",
		CallRecordID: "callrec_00000000000000000000000000000001", AttemptCallID: "call_00000000000000000000000000000001",
		Dimension: domain.BudgetLLMInputTokens, Subkey: "request", UpperBound: 10,
		State: domain.ReservationReserved, CreatedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
	}
	if err := reservation.ValidateFor(domain.PhysicalLLMRequest); err != nil {
		t.Fatal(err)
	}
	reservation.Dimension = domain.BudgetLLMTokens
	if err := reservation.ValidateFor(domain.PhysicalLLMRequest); err == nil {
		t.Fatal("computed token total accepted as physical usage evidence")
	}
}
