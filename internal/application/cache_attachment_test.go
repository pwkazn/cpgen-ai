package application_test

import (
	"context"
	"testing"

	"cpgen/internal/domain"
)

func TestCacheAttachmentRejectsSubstitutedImmutableMetadata(t *testing.T) {
	for name, change := range map[string]func(*domain.PendingCacheReuse){
		"role":         func(p *domain.PendingCacheReuse) { p.Role = domain.ArtifactOutput },
		"media type":   func(p *domain.PendingCacheReuse) { p.MediaType = "text/plain" },
		"logical path": func(p *domain.PendingCacheReuse) { p.LogicalPath = "different-output.json" },
		"producer":     func(p *domain.PendingCacheReuse) { p.Provenance.Producer = "substituted producer" },
		"input": func(p *domain.PendingCacheReuse) {
			digest := domain.SumBytes([]byte("substituted input"))
			p.Provenance.InputDigest = &digest
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{stages: []domain.StageName{"prepare", "exercise", "finish"}})
			cache := structuredCacheService(t, f)
			result, err := f.structured.Generate(ctx, f.open, f.request)
			if err != nil || result.Outcome.Value == nil {
				t.Fatalf("generate=%+v %v", result, err)
			}
			open, request := commitStructuredSource(t, f, result)
			if _, err := cache.Put(ctx, f.open, f.request); err != nil {
				t.Fatal(err)
			}
			hit, err := cache.Reuse(ctx, open, request)
			if err != nil || !hit.Hit || len(hit.Reuses) != 1 {
				t.Fatalf("cache reuse=%+v %v", hit, err)
			}
			original := hit.Reuses[0]
			changed := original
			change(&changed)
			output := domain.SumBytes(hit.Outcome.Value.Structured)
			command := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: open.AttemptID,
				AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "finish", NextInputDigest: &output,
				Occurrences:    []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &changed}},
				IdempotencyKey: coordinatorID("finish", "cache-attachment"), At: f.clock.Now(),
			}
			if _, err := f.store.FinishStage(ctx, command); err == nil {
				t.Fatal("changed cache metadata was attached")
			}
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil || current.Version != 4 || current.CurrentStage != "exercise" {
				t.Fatalf("rejected attachment changed stage: %+v %v", current, err)
			}
			command.Occurrences[0].CacheReuse = &original
			if _, err := f.store.FinishStage(ctx, command); err != nil {
				t.Fatalf("original cache attachment no longer succeeds: %v", err)
			}
			if f.httpCalls.Load() != 2 {
				t.Fatal("metadata validation dispatched provider work")
			}
		})
	}
}
