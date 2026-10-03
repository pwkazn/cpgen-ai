package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestReadStageAttemptRetainsHistoricalMetadataAndScope(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "attempts.db"), clock.NewFake(testNow))
	created := mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, time.Minute))
	input := domain.SumBytes([]byte("historical input"))
	output := domain.SumBytes([]byte("historical output"))
	finished := testNow.Add(time.Second)
	cause := domain.CauseUserCancel
	binding := &domain.BlockedCheckpoint{
		RunID: testRunID, StageName: "prepare", StageInputDigest: input,
		DependencyID: "provider", DependencyDigest: input, PolicyDigest: input, ErrorDigest: output,
		CreatedAt: finished, RetryAfter: finished.Add(time.Minute),
	}
	attempts := []domain.StageAttempt{
		{State: domain.StageAttemptSucceeded, OutputDigest: &output, FinishedAt: &finished},
		{State: domain.StageAttemptBlocked, BlockedBinding: binding, FinishedAt: &finished},
		{State: domain.StageAttemptCancelled, Cause: &cause, FinishedAt: &finished},
		{State: domain.StageAttemptRunning},
	}
	for i := range attempts {
		attempt := &attempts[i]
		attempt.AttemptID = domain.AttemptID(fmt.Sprintf("attempt_%032x", i+1))
		attempt.RunID, attempt.StageName, attempt.Ordinal = testRunID, "prepare", i+1
		attempt.InputDigest, attempt.StartedAt = input, testNow
		if err := attempt.Validate(); err != nil {
			t.Fatal(err)
		}
		var blockedJSON, finishedAt any
		if attempt.BlockedBinding != nil {
			raw, err := json.Marshal(attempt.BlockedBinding)
			if err != nil {
				t.Fatal(err)
			}
			blockedJSON = raw
		}
		if attempt.FinishedAt != nil {
			finishedAt = formatTime(*attempt.FinishedAt)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO stage_attempts
			(attempt_id,run_id,stage_name,ordinal,state,input_digest,output_digest,cause,blocked_binding_json,started_at,finished_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, attempt.AttemptID, attempt.RunID, attempt.StageName, attempt.Ordinal, attempt.State,
			attempt.InputDigest, attempt.OutputDigest, attempt.Cause, blockedJSON, formatTime(attempt.StartedAt), finishedAt); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := store.CurrentStageAttempt(ctx, testRunID, "prepare")
	if err != nil || !reflect.DeepEqual(latest, attempts[len(attempts)-1]) {
		t.Fatalf("latest attempt = %+v, %v", latest, err)
	}
	for _, want := range attempts {
		got, err := store.ReadStageAttempt(ctx, want.RunID, want.StageName, want.AttemptID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("historical attempt %s = %+v, want %+v; %v", want.AttemptID, got, want, err)
		}
	}
	for _, scope := range []struct {
		runID   domain.RunID
		stage   domain.StageName
		attempt domain.AttemptID
	}{
		{"run_00000000000000000000000000000002", "prepare", attempts[0].AttemptID},
		{testRunID, "exercise", attempts[0].AttemptID},
		{testRunID, "prepare", "attempt_00000000000000000000000000000099"},
	} {
		if _, err := store.ReadStageAttempt(ctx, scope.runID, scope.stage, scope.attempt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong scope %+v returned %v, want ErrNotFound", scope, err)
		}
	}
	for _, scope := range []struct {
		runID   domain.RunID
		stage   domain.StageName
		attempt domain.AttemptID
	}{
		{"invalid", "prepare", attempts[0].AttemptID},
		{testRunID, "", attempts[0].AttemptID},
		{testRunID, "prepare", "invalid"},
	} {
		if _, err := store.ReadStageAttempt(ctx, scope.runID, scope.stage, scope.attempt); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid scope %+v was not rejected before querying: %v", scope, err)
		}
	}
	after, err := store.GetRun(ctx, testRunID)
	if err != nil || !reflect.DeepEqual(after, created) {
		t.Fatalf("reading historical attempts changed run: %+v, %v", after, err)
	}
}
