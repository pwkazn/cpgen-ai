package application_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
)

func TestSimilarityReaderRequiresCommittedExactEvidence(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	result := f.search(t, f.service)
	reader, err := application.NewSimilarityReader(similarityReadFacade{store: f.store}, f.blobs, noSimilarityExchange{PhysicalProvider: f.provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reader.Read(ctx, f.runID, "prepare", f.request); err == nil {
		t.Fatal("uncommitted private bytes became a stage output")
	}
	if _, err := f.store.FinishStage(ctx, f.finish(result)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadCommittedLLMStage(ctx, f.runID, "prepare"); err == nil {
		t.Fatal("LLM reader admitted a similarity receipt")
	}
	before, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		evidence, err := reader.Read(ctx, f.runID, "prepare", f.request)
		if err != nil || !reflect.DeepEqual(evidence, *result.Outcome.Value) || f.httpCalls.Load() != 1 {
			t.Fatalf("committed evidence differs err=%v HTTP=%d", err, f.httpCalls.Load())
		}
	}
	after, err := f.store.GetRun(ctx, f.runID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read changed the run projection")
	}
	changed := f.request
	changed.LogicalIdempotencyKey += "/different"
	if _, err := reader.Read(ctx, f.runID, "prepare", changed); err == nil {
		t.Fatal("different request restored committed output")
	}
	policyReader, err := application.NewSimilarityReader(similarityReadFacade{store: f.store}, f.blobs, changedSimilarityPolicy{PhysicalProvider: f.provider})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policyReader.Read(ctx, f.runID, "prepare", f.request); err == nil {
		t.Fatal("different provider policy restored committed output")
	}
	hex := strings.TrimPrefix(string(result.Artifact.Blob.Digest), "sha256:")
	if err := os.Remove(filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(ctx, f.runID, "prepare", f.request); err == nil || f.httpCalls.Load() != 1 {
		t.Fatal("missing committed bytes were silently regenerated")
	}
}

func TestSimilarityReaderRejectsSubstitutedMetadata(t *testing.T) {
	for _, mode := range []string{"input", "output", "attempt", "occurrence", "provenance", "duplicate", "provider", "publication"} {
		t.Run(mode, func(t *testing.T) {
			f := newSimilarityReplayFixture(t)
			result := f.search(t, f.service)
			if _, err := f.store.FinishStage(context.Background(), f.finish(result)); err != nil {
				t.Fatal(err)
			}
			reader, err := application.NewSimilarityReader(similarityReadFacade{store: f.store, mode: mode}, f.blobs, noSimilarityExchange{PhysicalProvider: f.provider})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Read(context.Background(), f.runID, "prepare", f.request); err == nil || f.httpCalls.Load() != 1 {
				t.Fatalf("substituted %s proof was accepted", mode)
			}
		})
	}
}

func TestSimilarityReaderRejectsHistoryAfterReviewInvalidation(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	result := f.search(t, f.service)
	ctx := context.Background()
	if _, err := f.store.FinishStage(ctx, f.finish(result)); err != nil {
		t.Fatal(err)
	}
	attempt := domain.AttemptID("attempt_00000000000000000000000000001802")
	input := result.Outcome.Value.EvidenceDigest
	if _, err := f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: 3, StageName: "exercise", AttemptID: attempt, InputDigest: input, IdempotencyKey: coordinatorID("begin", "similarity-review"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	evidence, policy := domain.SumBytes([]byte("review-evidence")), domain.SumBytes([]byte("review-policy"))
	paused, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: attempt, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy, IdempotencyKey: coordinatorID("finish", "similarity-review"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	edits := domain.SumBytes([]byte("request another similarity pass"))
	decision, err := f.store.CreateReview(ctx, domain.CreateReviewRequest{ID: "review_00000000000000000000000000001801", RunID: f.runID, ExpectedRunVersion: paused.Version, Kind: domain.ReviewRevise, WorkflowRevision: "slice1/v1", StageName: "exercise", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, RequestedEditsDigest: &edits, Reviewer: "fixture", Reason: "re-evaluate input", IdempotencyKey: coordinatorID("review", "similarity-reader"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	newInput, err := f.request.Digest()
	if err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"revision":2,"schema":"cpgen.config/v1"}`)
	configDigest := domain.SumBytes(config)
	if _, err := f.store.ApplyReview(ctx, domain.ApplyReviewCommand{RunID: f.runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID, StageName: "exercise", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, NewInputDigest: &newInput, NewConfigJSON: config, NewConfigDigest: &configDigest, InvalidatedStages: []domain.StageName{"prepare", "exercise"}, IdempotencyKey: coordinatorID("apply", "similarity-reader"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	reader, err := application.NewSimilarityReader(similarityReadFacade{store: f.store}, f.blobs, noSimilarityExchange{PhysicalProvider: f.provider})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(ctx, f.runID, "prepare", f.request); err == nil || f.httpCalls.Load() != 1 {
		t.Fatal("historical success escaped review invalidation")
	}
}

// This facade has no mutating methods; neither committed reading nor its
// provider interface can recover a writer or send an exchange in these tests.
type similarityReadFacade struct {
	store application.SimilarityReadStore
	mode  string
}

func (s similarityReadFacade) ReadCommittedSimilarityStage(ctx context.Context, run domain.RunID, stage domain.StageName) (port.CommittedPrivateStage, error) {
	result, err := s.store.ReadCommittedSimilarityStage(ctx, run, stage)
	if err != nil {
		return result, err
	}
	bad := domain.SumBytes([]byte("substituted"))
	switch s.mode {
	case "input":
		result.Attempt.InputDigest = bad
	case "output":
		result.Attempt.OutputDigest = &bad
	case "attempt":
		result.Attempt.AttemptID = "attempt_00000000000000000000000000001809"
	case "occurrence":
		result.Artifacts[0].Source.OccurrenceID = "occ_00000000000000000000000000001809"
	case "provenance":
		result.Artifacts[0].Blob.Provenance.InputDigest = &bad
	case "duplicate":
		result.Artifacts = append(result.Artifacts, result.Artifacts[0])
	}
	return result, nil
}

func (s similarityReadFacade) LoadCall(ctx context.Context, id domain.CallRecordID) (domain.PreparedCalls, error) {
	result, err := s.store.LoadCall(ctx, id)
	if s.mode == "provider" && result.Call.Provider != "private-blob" {
		result.Call.RequestDigest = domain.SumBytes([]byte("substituted provider"))
	}
	if s.mode == "publication" && result.Call.Provider == "private-blob" {
		for i := range result.PhysicalCalls {
			result.PhysicalCalls[i].ResponseDigest = nil
		}
	}
	return result, err
}

type noSimilarityExchange struct{ similarity.PhysicalProvider }

func (noSimilarityExchange) SearchPhysical(context.Context, similarity.Request, domain.AttemptCallID) (similarity.PhysicalSearchResult, error) {
	panic("committed reader sent HTTP")
}

type changedSimilarityPolicy struct{ similarity.PhysicalProvider }

func (p changedSimilarityPolicy) PlanSearch(request similarity.Request) (similarity.PhysicalSearchPlan, error) {
	plan, err := p.PhysicalProvider.PlanSearch(request)
	plan.PolicyDigest = domain.SumBytes([]byte("changed physical policy"))
	return plan, err
}
