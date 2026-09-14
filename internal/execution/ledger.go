package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
)

type Store interface {
	ArtifactCallLedger
	ReadCommittedLLMArtifact(context.Context, domain.RunID, domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error)
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
	OpenReplayableCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error)
	ReadOpenCall(context.Context, domain.CallRecordID) (domain.OpenCallRequest, error)
	FinishReplayableCall(context.Context, domain.FinishCallRequest) (domain.CallTrace, error)
	ReadFinishCall(context.Context, domain.CallRecordID) (domain.FinishCallRequest, error)
	ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
}

// RunLedger bridges long-lived stage calls to optimistic run versions.
// The foreground owner must hold the run lock. Only ledger transitions retry;
// no provider operation runs inside this bridge or any database transaction.
// ResumeDispatch remains an inherited read-only receipt operation; reading an
// old cache source cannot grant fresh provider-send authority.
type RunLedger struct {
	Store
	runID   domain.RunID
	stage   domain.StageName
	attempt domain.AttemptID
}

func NewRunLedger(store Store, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) (*RunLedger, error) {
	if store == nil || runID.Validate() != nil || stage.Validate() != nil || attempt.Validate() != nil {
		return nil, errors.New("run-bound LLM ledger requires a store and exact stage attempt")
	}
	return &RunLedger{Store: store, runID: runID, stage: stage, attempt: attempt}, nil
}

func (l *RunLedger) checkScope(runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) error {
	if runID != l.runID || stage != l.stage || attempt != l.attempt {
		return errors.New("LLM ledger operation is outside the bound stage attempt")
	}
	return nil
}

func (l *RunLedger) currentVersion(ctx context.Context) (int64, error) {
	run, err := l.GetRun(ctx, l.runID)
	if err != nil {
		return 0, err
	}
	if run.State != domain.RunRunning || run.CurrentStage != l.stage {
		return 0, errors.New("LLM ledger requires its current RUNNING stage")
	}
	attempt, err := l.CurrentStageAttempt(ctx, l.runID, l.stage)
	if err != nil {
		return 0, err
	}
	if attempt.AttemptID != l.attempt || attempt.State != domain.StageAttemptRunning {
		return 0, errors.New("LLM ledger attempt is no longer current")
	}
	return run.Version, nil
}

func withRunCallVersion[T any](ctx context.Context, ledger *RunLedger, transition func(int64) (T, error)) (T, error) {
	var empty T
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		version, err := ledger.currentVersion(ctx)
		if err != nil {
			return empty, err
		}
		value, err := transition(version)
		if !errors.Is(err, sqlite.ErrVersionConflict) {
			return value, err
		}
	}
	return empty, sqlite.ErrVersionConflict
}

func (l *RunLedger) OpenCall(ctx context.Context, request domain.OpenCallRequest) (domain.CallRecord, error) {
	if err := request.Validate(); err != nil {
		return domain.CallRecord{}, err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return domain.CallRecord{}, err
	}
	original, err := l.ReadOpenCall(ctx, request.ID)
	if err == nil {
		candidate := request
		candidate.ExpectedRunVersion = original.ExpectedRunVersion
		left, leftErr := json.Marshal(candidate)
		right, rightErr := json.Marshal(original)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return domain.CallRecord{}, errors.New("replayed LLM logical open changed immutable command fields")
		}
		return withRunCallVersion(ctx, l, func(int64) (domain.CallRecord, error) { return l.Store.OpenReplayableCall(ctx, original) })
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return domain.CallRecord{}, err
	}
	// A legacy call can acquire original metadata only when the caller still
	// possesses its exact original command. Never guess its prior version.
	if _, err := l.ReadLogicalCall(ctx, request.ID); err == nil {
		return withRunCallVersion(ctx, l, func(int64) (domain.CallRecord, error) { return l.Store.OpenReplayableCall(ctx, request) })
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return domain.CallRecord{}, err
	}
	return withRunCallVersion(ctx, l, func(version int64) (domain.CallRecord, error) {
		request.ExpectedRunVersion = version
		return l.Store.OpenReplayableCall(ctx, request)
	})
}

func (l *RunLedger) PrepareCalls(ctx context.Context, request domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	if err := request.Validate(); err != nil {
		return domain.PreparedCalls{}, err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return domain.PreparedCalls{}, err
	}
	return withRunCallVersion(ctx, l, func(version int64) (domain.PreparedCalls, error) {
		request.ExpectedRunVersion = version
		return l.Store.PrepareCalls(ctx, request)
	})
}

func (l *RunLedger) BeginDispatch(ctx context.Context, request domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	if err := request.Validate(); err != nil {
		return domain.DispatchGrant{}, err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return domain.DispatchGrant{}, err
	}
	return withRunCallVersion(ctx, l, func(version int64) (domain.DispatchGrant, error) {
		request.ExpectedRunVersion = version
		return l.Store.BeginDispatch(ctx, request)
	})
}

func (l *RunLedger) MarkSent(ctx context.Context, grant domain.DispatchGrant, at time.Time) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	if err := l.checkScope(grant.RunID, grant.StageName, grant.AttemptID); err != nil {
		return err
	}
	_, err := withRunCallVersion(ctx, l, func(version int64) (struct{}, error) {
		grant.ExpectedRunVersion = version
		return struct{}{}, l.Store.MarkSent(ctx, grant, at)
	})
	return err
}

func (l *RunLedger) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return err
	}
	_, err := withRunCallVersion(ctx, l, func(version int64) (struct{}, error) {
		request.ExpectedRunVersion = version
		return struct{}{}, l.Store.CompletePhysical(ctx, request)
	})
	return err
}

func (l *RunLedger) FinishCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	if err := request.Validate(); err != nil {
		return domain.CallTrace{}, err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return domain.CallTrace{}, err
	}
	original, err := l.ReadFinishCall(ctx, request.CallRecordID)
	if err == nil {
		candidate := request
		candidate.ExpectedRunVersion = original.ExpectedRunVersion
		left, leftErr := json.Marshal(candidate)
		right, rightErr := json.Marshal(original)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return domain.CallTrace{}, errors.New("replayed LLM logical finish changed immutable command fields")
		}
		return withRunCallVersion(ctx, l, func(int64) (domain.CallTrace, error) { return l.Store.FinishReplayableCall(ctx, original) })
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return domain.CallTrace{}, err
	}
	call, err := l.ReadLogicalCall(ctx, request.CallRecordID)
	if err != nil {
		return domain.CallTrace{}, err
	}
	if call.State == domain.CallRecordTerminal {
		return withRunCallVersion(ctx, l, func(int64) (domain.CallTrace, error) { return l.Store.FinishReplayableCall(ctx, request) })
	}
	return withRunCallVersion(ctx, l, func(version int64) (domain.CallTrace, error) {
		request.ExpectedRunVersion = version
		return l.Store.FinishReplayableCall(ctx, request)
	})
}

func (l *RunLedger) ReleaseUnwrittenArtifactReservations(ctx context.Context, request domain.OpenCallRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := l.checkScope(request.RunID, request.StageName, request.AttemptID); err != nil {
		return err
	}
	_, err := withRunCallVersion(ctx, l, func(version int64) (struct{}, error) {
		request.ExpectedRunVersion = version
		return struct{}{}, l.Store.ReleaseUnwrittenArtifactReservations(ctx, request)
	})
	return err
}
