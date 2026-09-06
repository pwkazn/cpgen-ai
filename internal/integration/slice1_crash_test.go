package integration_test

import (
	"context"
	"fmt"
	"testing"

	"cpgen/internal/domain"
)

// These names are deliberately kept in one table. Each name is a durable
// boundary in the Slice 1 design; the helper is killed immediately after the
// corresponding committed preparation. The physical/artifact names also
// document the rule that a Fake-only bootstrap cannot perform an irreversible
// external operation, while their real ledgers are exercised by adapter tests.
var slice1CrashBoundaries = []string{
	"run_create", "stage_begin", "budget_reserve", "dispatching", "sent",
	"physical_completion", "blob_sealed", "blob_published", "blob_ready",
	"occurrence_commit", "sandbox_resource", "stage_finish",
}

func TestSlice1CrashDurableBoundariesConvergeAfterForceKill(t *testing.T) {
	for index, boundary := range slice1CrashBoundaries {
		t.Run(boundary, func(t *testing.T) {
			env := newIntegrationEnvironment(t, "review")
			runID := domain.RunID(fmt.Sprintf("run_%032x", index+101))
			helper := startIntegrationHelper(t, env, "crash-"+boundary, runID)
			helper.kill(t)

			app := openIntegrationApp(t, env)
			if boundary == "blob_sealed" || boundary == "blob_published" || boundary == "blob_ready" {
				if err := recoverArtifactAfterCrash(context.Background(), app, env.cfg, runID, boundary); err != nil {
					t.Fatalf("recover artifact after %s: %v", boundary, err)
				}
			}
			if boundary == "sandbox_resource" {
				if err := recoverSandboxAfterCrash(context.Background(), app, runID); err != nil {
					t.Fatalf("recover sandbox after %s: %v", boundary, err)
				}
			}
			// Resume through the real application service. It must repair an
			// interrupted attempt, not continue a stale occurrence or mint a
			// second physical identity.
			snapshot, err := app.Runs.Resume(context.Background(), runID)
			if err != nil {
				t.Fatalf("resume after %s: %v", boundary, err)
			}
			if snapshot.State != domain.RunNeedsReview {
				t.Fatalf("resume after %s = %+v, want NEEDS_REVIEW", boundary, snapshot)
			}
			if isArtifactCrashBoundary(boundary) {
				assertArtifactRecovered(t, app, env.cfg, runID)
			}
			if boundary == "sandbox_resource" {
				assertSandboxRecovered(t, app, runID)
			}
			events, err := app.Runtime.Events(context.Background(), runID, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertContiguousEvents(t, events)
			keys := make(map[string]struct{}, len(events))
			for _, event := range events {
				if _, exists := keys[event.IdempotencyKey]; exists {
					t.Fatalf("duplicate event idempotency key %q after %s", event.IdempotencyKey, boundary)
				}
				keys[event.IdempotencyKey] = struct{}{}
			}
			// A second resume is the replay probe. It must not append events or
			// change the terminal review projection.
			before := len(events)
			replayed, err := app.Runs.Resume(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if replayed.Version != snapshot.Version || replayed.State != snapshot.State {
				t.Fatalf("resume replay after %s = %+v, first = %+v", boundary, replayed, snapshot)
			}
			after, err := app.Runtime.Events(context.Background(), runID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != before {
				t.Fatalf("resume replay after %s appended events: before=%d after=%d", boundary, before, len(after))
			}
		})
	}
}

func assertContiguousEvents(t *testing.T, events []domain.RunEvent) {
	t.Helper()
	for index, event := range events {
		if event.Version != int64(index+1) {
			t.Fatalf("event %d has version %d", index, event.Version)
		}
		if err := event.Validate(); err != nil {
			t.Fatalf("event %d invalid: %v", index, err)
		}
	}
}
