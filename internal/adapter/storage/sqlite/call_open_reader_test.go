package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestReplayableOpenCallRetainsExactOriginalCommand(t *testing.T) {
	f := newMeteringFixture(t, "d4", domain.BudgetLimits{})
	request := openCallRequest(f, 1, domain.CallLLMGenerate)
	first, err := f.store.OpenReplayableCall(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.ReadOpenCall(context.Background(), first.ID)
	if err != nil || !reflect.DeepEqual(request, stored) {
		t.Fatalf("request=%+v error=%v", stored, err)
	}
	if replayed, err := f.store.OpenReplayableCall(context.Background(), stored); err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("call=%+v error=%v", replayed, err)
	}
	changed := request
	changed.ExpectedRunVersion++
	if _, err := f.store.OpenReplayableCall(context.Background(), changed); !errors.Is(err, ErrConsistency) {
		t.Fatalf("changed original open accepted: %v", err)
	}
	for _, query := range []string{`UPDATE call_open_requests SET request_json = request_json WHERE call_record_id = ?`, `DELETE FROM call_open_requests WHERE call_record_id = ?`} {
		if _, err := f.store.db.Exec(query, request.ID); err == nil {
			t.Fatal("original open receipt was mutable")
		}
	}
}

func TestReplayableOpenCallReceiptAndLogicalRecordCommitAtomically(t *testing.T) {
	f := newMeteringFixture(t, "d5", domain.BudgetLimits{})
	request := openCallRequest(f, 1, domain.CallLLMGenerate)
	if _, err := f.store.db.Exec(`CREATE TRIGGER fail_open_receipt BEFORE INSERT ON call_open_requests BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenReplayableCall(context.Background(), request); err == nil {
		t.Fatal("receipt failure was ignored")
	}
	var count int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM call_records WHERE call_record_id = ?`, request.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("logical open survived receipt transaction rollback")
	}
	if _, err := f.store.db.Exec(`DROP TRIGGER fail_open_receipt`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenReplayableCall(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestReplayableOpenCanAdoptOnlyExactHistoricalCommand(t *testing.T) {
	f := newMeteringFixture(t, "d6", domain.BudgetLimits{})
	request := openCallRequest(f, 1, domain.CallLLMGenerate)
	if _, err := f.store.OpenCall(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadOpenCall(context.Background(), request.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy call has unexpected receipt: %v", err)
	}
	changed := request
	changed.ExpectedRunVersion++
	if _, err := f.store.OpenReplayableCall(context.Background(), changed); !errors.Is(err, ErrConsistency) {
		t.Fatalf("guessed historical command accepted: %v", err)
	}
	if _, err := f.store.OpenReplayableCall(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if stored, err := f.store.ReadOpenCall(context.Background(), request.ID); err != nil || !reflect.DeepEqual(stored, request) {
		t.Fatalf("stored=%+v error=%v", stored, err)
	}
}

func TestReplayableFinishRetainsExactCommandWithAtomicRollback(t *testing.T) {
	f := newMeteringFixture(t, "d7", domain.BudgetLimits{})
	open := openCallRequest(f, 1, domain.CallLLMGenerate)
	if _, err := f.store.OpenReplayableCall(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	finish := domain.FinishCallRequest{RunID: open.RunID, ExpectedRunVersion: open.ExpectedRunVersion, StageName: open.StageName, AttemptID: open.AttemptID, CallRecordID: open.ID, DispatchKind: domain.DispatchNone, Failure: &domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked}, IdempotencyKey: "finish_00000000000000000000000000000001", At: open.At.Add(time.Millisecond)}
	if _, err := f.store.db.Exec(`CREATE TRIGGER fail_finish_receipt BEFORE INSERT ON call_finish_requests BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishReplayableCall(context.Background(), finish); err == nil {
		t.Fatal("finish receipt failure was ignored")
	}
	call, err := f.store.ReadLogicalCall(context.Background(), open.ID)
	if err != nil || call.State != domain.CallRecordOpen {
		t.Fatalf("terminal state survived receipt rollback: %+v %v", call, err)
	}
	if _, err := f.store.db.Exec(`DROP TRIGGER fail_finish_receipt`); err != nil {
		t.Fatal(err)
	}
	first, err := f.store.FinishReplayableCall(context.Background(), finish)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.ReadFinishCall(context.Background(), open.ID)
	if err != nil || !reflect.DeepEqual(stored, finish) {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if replay, err := f.store.FinishReplayableCall(context.Background(), stored); err != nil || !first.Equal(replay) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	finish.ExpectedRunVersion++
	if _, err := f.store.FinishReplayableCall(context.Background(), finish); !errors.Is(err, ErrConsistency) {
		t.Fatalf("changed completion version accepted by exact store: %v", err)
	}
	for _, query := range []string{`UPDATE call_finish_requests SET request_json=request_json WHERE call_record_id=?`, `DELETE FROM call_finish_requests WHERE call_record_id=?`} {
		if _, err := f.store.db.Exec(query, open.ID); err == nil {
			t.Fatal("original completion command was mutable")
		}
	}
	if _, err := f.store.db.Exec(`DROP TRIGGER call_finish_requests_immutable_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE call_finish_requests SET request_json=CAST(json_set(CAST(request_json AS TEXT),'$.expected_run_version',99) AS BLOB) WHERE call_record_id=?`, open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadFinishCall(context.Background(), open.ID); !errors.Is(err, ErrConsistency) {
		t.Fatalf("corrupt original finish command accepted: %v", err)
	}
}
