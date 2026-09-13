package application_test

import (
	"context"
	"strings"
	"testing"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/workflow"
)

func TestMVPCompositionRejectsForeignResourcesBeforeExecution(t *testing.T) {
	f := similarityExecutorFixtureFromGeneration(t, newGenerationExecutorFixtureWithWorkflow(t, 4, false, workflow.GenerationRevision, 3))
	f.config.WorkflowRevision = workflow.GenerationRevision
	evidence, err := application.NewSimilarityExecutor(f.config)
	if err != nil {
		t.Fatal(err)
	}
	_, frozen, err := f.store.RunViewDocuments(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	g := f.executorConfig
	c := application.GenerationRunConfig{Store: g.Store, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks, Generation: f.executor, Similarity: evidence, EffectiveConfigJSON: frozen}
	if _, err := application.NewGenerationRunService(c); err == nil || !strings.Contains(err.Error(), "durable Docker configuration") {
		t.Fatalf("valid resources did not reach sandbox requirement: %v", err)
	}
	wrongStore := c
	wrongStore.Store = &struct {
		application.GenerationExecutionStore
	}{g.Store}
	if _, err := application.NewGenerationRunService(wrongStore); err == nil || !strings.Contains(err.Error(), "execution resources") {
		t.Fatalf("foreign resource accepted: %v", err)
	}
	wrongPolicy := c
	wrongPolicy.EffectiveConfigJSON = []byte(`{"changed":true}`)
	if _, err := application.NewGenerationRunService(wrongPolicy); err == nil || !strings.Contains(err.Error(), "frozen configuration") {
		t.Fatalf("foreign policy accepted: %v", err)
	}
	stage := application.SimilarityStageConfig{Statement: f.executor.Reader(), Admission: f.executor.StageAdmission, Store: f.store, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks, Provider: f.config.Provider, Policy: f.config.Policy, Limit: f.config.Limit, RetryPolicy: f.config.RetryPolicy, CostUpperBoundMicroUSD: f.config.CostUpperBoundMicroUSD, WorkflowRevision: workflow.GenerationRevision}
	foreignStore := stage
	foreignStore.Store = &struct {
		application.SimilarityStageStore
	}{f.store}
	if _, err := application.NewSimilarityExecutorWithConfig(foreignStore); err == nil || !strings.Contains(err.Error(), "share execution storage") {
		t.Fatalf("similarity accepted foreign evidence storage: %v", err)
	}
	foreignBlobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage.Blobs = foreignBlobs
	foreignSimilarity, err := application.NewSimilarityExecutorWithConfig(stage)
	if err != nil {
		t.Fatal(err)
	}
	wrongStage := c
	wrongStage.Similarity = foreignSimilarity
	if _, err := application.NewGenerationRunService(wrongStage); err == nil || !strings.Contains(err.Error(), "execution resources") {
		t.Fatalf("MVP accepted a foreign stage blob store: %v", err)
	}
	if f.httpCalls.Load() != 0 || f.sends.Load() != 0 {
		t.Fatal("construction dispatched provider work")
	}
}
