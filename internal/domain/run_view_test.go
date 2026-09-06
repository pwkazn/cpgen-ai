package domain_test

import (
	"testing"

	"cpgen/internal/domain"
)

func TestRunViewCopiesMutableInputsAndAccessors(t *testing.T) {
	digest := domain.SumBytes([]byte("request"))
	request := []byte(`{"brief":"before"}`)
	config := []byte(`{"mode":"offline"}`)
	remaining := map[domain.BudgetDimension]int64{domain.BudgetLLMCalls: 2}
	view, err := domain.NewRunView(domain.RunViewData{
		RunID: domain.RunID("run_0123456789abcdef0123456789abcdef"), WorkflowRevision: "slice1.v1",
		SchemaVersion: domain.SchemaVersion("cpgen.request/v1"), RequestDigest: digest, ConfigDigest: digest,
		WorkflowDigest: digest, State: domain.RunRunning, CurrentStage: "prepare", Version: 3,
		RequestJSON: request, ConfigJSON: config, Budget: domain.BudgetSnapshot{Remaining: remaining},
	})
	if err != nil {
		t.Fatal(err)
	}
	request[0], config[0] = 'X', 'X'
	remaining[domain.BudgetLLMCalls] = 0
	if got := view.RequestJSON(); string(got) != `{"brief":"before"}` {
		t.Fatalf("request alias leaked: %q", got)
	}
	if got := view.ConfigJSON(); string(got) != `{"mode":"offline"}` {
		t.Fatalf("config alias leaked: %q", got)
	}
	copyRemaining := view.Budget().Remaining
	copyRemaining[domain.BudgetLLMCalls] = 0
	if view.Budget().Remaining[domain.BudgetLLMCalls] != 2 {
		t.Fatal("budget map accessor leaked an alias")
	}
}
