package application_test

import (
	"context"
	"cpgen/internal/application"
	"reflect"
	"testing"
)

func TestStructuredLLMCommittedReadRequiresAttachmentAndNeverGenerates(t *testing.T) {
	for _, repair := range []bool{false, true} {
		name := "original"
		options := structuredLLMOptions{firstContent: `{"schema_version":"cpgen.idea/v1","title":"original"}`}
		if repair {
			name, options.firstContent = "repair", ""
		}
		t.Run(name, func(t *testing.T) {
			f := newStructuredLLMOptionsFixture(t, options)
			// Deliberately expose only read methods; no dispatcher or writer is
			// available to this composition, even through interface assertions.
			reader, err := application.NewCommittedLLMReader(struct {
				application.CommittedLLMReadStore
			}{f.store}, f.blobs, struct{ application.LLMReadPolicy }{f.model}, f.policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := reader.ReadCommitted(context.Background(), f.open, f.request); err == nil || f.httpCalls.Load() != 0 {
				t.Fatalf("unstarted read dispatched or succeeded: %v", err)
			}
			result, err := f.structured.Generate(context.Background(), f.open, f.request)
			if err != nil {
				t.Fatal(err)
			}
			before := f.httpCalls.Load()
			if _, _, err := reader.ReadCommitted(context.Background(), f.open, f.request); err == nil || f.httpCalls.Load() != before {
				t.Fatalf("unattached read accepted output or dispatched: %v", err)
			}
			commitStructuredSource(t, f, result)
			beforeBudget, err := f.store.BudgetSnapshot(context.Background(), f.open.RunID)
			if err != nil {
				t.Fatal(err)
			}
			id, response, err := reader.ReadCommitted(context.Background(), f.open, f.request)
			if err != nil || response == nil || string(response.Structured) != string(result.Outcome.Value.Structured) || f.httpCalls.Load() != before {
				t.Fatalf("committed response=%+v err=%v", response, err)
			}
			afterBudget, budgetErr := f.store.BudgetSnapshot(context.Background(), f.open.RunID)
			if budgetErr != nil || !reflect.DeepEqual(beforeBudget, afterBudget) {
				t.Fatalf("read changed budget: %v", budgetErr)
			}
			if (id != f.open.ID) != repair {
				t.Fatalf("original/repair provenance conflated: %s", id)
			}
		})
	}
}
