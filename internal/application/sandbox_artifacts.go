package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// SandboxArtifactSink owns one operation's local publications. Its caller
// holds the run and artifact-maintenance locks. Every write has its own byte
// reservation; container identities never replace artifact writer identities.
type SandboxArtifactSink struct {
	ledger   *RunBoundLLMLedger
	blobs    *blob.Store
	clock    clock.Clock
	identity port.SandboxAuthorizationIdentity
}

func NewSandboxArtifactSink(store RunLLMStore, blobs *blob.Store, source clock.Clock, identity port.SandboxAuthorizationIdentity) (*SandboxArtifactSink, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if blobs == nil || source == nil {
		return nil, errors.New("sandbox artifact storage and clock are required")
	}
	ledger, err := NewRunBoundLLMLedger(store, identity.RunID, identity.StageName, identity.AttemptID)
	if err != nil {
		return nil, err
	}
	return &SandboxArtifactSink{ledger: ledger, blobs: blobs, clock: source, identity: identity}, nil
}

func (s *SandboxArtifactSink) binding(decl port.ArtifactDeclaration) (domain.OpenCallRequest, domain.ArtifactDeclarationRecord, error) {
	if err := decl.Validate(); err != nil {
		return domain.OpenCallRequest{}, domain.ArtifactDeclarationRecord{}, err
	}
	i := s.identity
	identity := i
	identity.ExpectedRunVersion = 0
	raw, err := json.Marshal(struct {
		Schema      string
		Identity    port.SandboxAuthorizationIdentity
		Declaration port.ArtifactDeclaration
	}{"cpgen.sandbox-artifact/v1", identity, decl})
	if err != nil {
		return domain.OpenCallRequest{}, domain.ArtifactDeclarationRecord{}, err
	}
	digest := domain.SumBytes(raw)
	id := domain.CallRecordID(coordinatorMutationID("callrec", "sandbox-artifact", i.RunID, i.AttemptID, i.LogicalOperationID, decl.LogicalPath))
	physical := domain.AttemptCallID(coordinatorMutationID("call", id))
	reservation := domain.ReservationID(coordinatorMutationID("res", physical))
	open := domain.OpenCallRequest{ID: id, RunID: i.RunID, ExpectedRunVersion: i.ExpectedRunVersion, StageName: i.StageName, AttemptID: i.AttemptID, LogicalOperationID: "sandbox-artifact:" + string(id), Kind: i.Kind, Provider: "blob", RequestDigest: digest, PolicyDigest: i.ScopeDigest, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: i.ScopeDigest}, IdempotencyKey: coordinatorMutationID("open", id), At: s.clock.Now().UTC()}
	declaration := domain.ArtifactDeclarationRecord{ID: domain.ArtifactDeclarationID(coordinatorMutationID("decl", id)), RunID: i.RunID, StageName: i.StageName, AttemptID: i.AttemptID, CallRecordID: id, AttemptCallID: physical, ReservationID: reservation, ReservationSubkey: "artifact", MediaType: decl.MediaType, Role: decl.Role, LogicalPath: decl.LogicalPath, MaxBytes: decl.MaxBytes, Provenance: decl.Provenance, CreatedAt: open.At}
	return open, declaration, nil
}

func (s *SandboxArtifactSink) load(ctx context.Context, decl port.ArtifactDeclaration, create bool) (domain.PreparedCalls, domain.ArtifactDeclarationRecord, bool, error) {
	open, d, err := s.binding(decl)
	if err != nil {
		return domain.PreparedCalls{}, d, false, err
	}
	record, err := s.ledger.ReadLogicalCall(ctx, open.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		if !create {
			return domain.PreparedCalls{}, d, false, nil
		}
		record, err = s.ledger.OpenCall(ctx, open)
	} else if err == nil {
		if record.RunID != open.RunID || record.StageName != open.StageName || record.AttemptID != open.AttemptID || record.Kind != open.Kind || record.Provider != open.Provider || record.RequestDigest != open.RequestDigest || record.PolicyDigest != open.PolicyDigest || record.LogicalOperationID != open.LogicalOperationID || !reflect.DeepEqual(record.RetryPolicy, open.RetryPolicy) {
			return domain.PreparedCalls{}, d, false, errors.New("sandbox artifact publication binding differs")
		}
	}
	if err != nil {
		return domain.PreparedCalls{}, d, false, err
	}
	d.CreatedAt = record.OpenedAt
	var prepared domain.PreparedCalls
	if record.State == domain.CallRecordOpen && create {
		prepared, err = s.ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: open.RunID, ExpectedRunVersion: open.ExpectedRunVersion, StageName: open.StageName, AttemptID: open.AttemptID, CallRecordID: open.ID, PlanDigest: open.RequestDigest, Calls: []domain.PhysicalCallPlan{{ID: d.AttemptCallID, Ordinal: 1, RetryGroup: "artifact", RetryOrdinal: 1, Kind: domain.PhysicalLocalArtifactWrite, Provider: "blob", RequestDigest: open.RequestDigest, IdempotencyKey: coordinatorMutationID("physical", d.AttemptCallID), Reservations: []domain.ReservationPlan{{ID: d.ReservationID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: d.ReservationSubkey, UpperBound: d.MaxBytes}}}}, IdempotencyKey: coordinatorMutationID("prepare", open.ID), At: s.clock.Now().UTC()})
	} else if record.State == domain.CallRecordOpen {
		return domain.PreparedCalls{}, d, false, nil
	} else {
		prepared, err = s.ledger.LoadCall(ctx, record.ID)
	}
	if err != nil {
		return prepared, d, false, err
	}
	if prepared.Failure != nil {
		return prepared, d, false, fmt.Errorf("sandbox artifact publication refused: %s/%s", prepared.Failure.Code, prepared.Failure.Class)
	}
	if create {
		if err := s.ledger.CreateArtifactDeclaration(ctx, d); err != nil {
			return prepared, d, false, err
		}
	}
	return prepared, d, true, nil
}

func (s *SandboxArtifactSink) Prepare(ctx context.Context, decl port.ArtifactDeclaration) (port.ArtifactWriter, error) {
	return s.prepare(ctx, decl, false)
}

func (s *SandboxArtifactSink) prepare(ctx context.Context, decl port.ArtifactDeclaration, restart bool) (port.ArtifactWriter, error) {
	p, d, _, err := s.load(ctx, decl, true)
	if err != nil {
		return nil, err
	}
	if len(p.PhysicalCalls) != 1 || p.Call.State != domain.CallRecordPrepared {
		return nil, errors.New("sandbox artifact write is not fresh; read or recover its existing receipt")
	}
	resumed := p.PhysicalCalls[0].State == domain.PhysicalDispatching || p.PhysicalCalls[0].State == domain.PhysicalSent
	if p.PhysicalCalls[0].State != domain.PhysicalPrepared && !(restart && resumed) {
		return nil, errors.New("sandbox artifact boundary cannot be rewritten")
	}
	session, err := NewPreparedArtifactSession(s.ledger, s.blobs, p)
	if err != nil {
		return nil, err
	}
	var writer port.ArtifactWriter
	if restart {
		writer, err = session.RestartUnsealed(ctx, d.ID)
	} else {
		writer, err = session.Prepare(ctx, d.ID)
	}
	if err != nil {
		return nil, err
	}
	var grant domain.DispatchGrant
	if resumed {
		grant, err = s.ledger.ResumeDispatch(ctx, s.identity.ExpectedRunVersion, d.AttemptCallID)
	} else {
		grant, err = s.ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: d.RunID, ExpectedRunVersion: s.identity.ExpectedRunVersion, StageName: d.StageName, AttemptID: d.AttemptID, CallRecordID: d.CallRecordID, AttemptCallID: d.AttemptCallID, IdempotencyKey: coordinatorMutationID("begin", d.AttemptCallID), At: s.clock.Now().UTC()})
	}
	if err != nil {
		return nil, errors.Join(err, writer.Abort(context.WithoutCancel(ctx)))
	}
	return &sandboxArtifactWriter{ArtifactWriter: writer, sink: s, declaration: d, grant: grant}, nil
}

func (*SandboxArtifactSink) PinExisting(context.Context, domain.BlobRef, port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	return domain.PendingArtifact{}, errors.New("sandbox artifact reuse requires its exact publication receipt")
}

func (s *SandboxArtifactSink) Publish(ctx context.Context, decl port.ArtifactDeclaration, data []byte) (domain.PendingArtifact, error) {
	if int64(len(data)) > decl.MaxBytes {
		return domain.PendingArtifact{}, errors.New("sandbox artifact payload exceeds declaration")
	}
	if pending, found, err := s.Read(ctx, decl); err != nil {
		return domain.PendingArtifact{}, err
	} else if found {
		if pending.Blob.Size != int64(len(data)) || pending.Blob.Digest != domain.SumBytes(data) {
			return domain.PendingArtifact{}, errors.New("sandbox artifact replay bytes differ")
		}
		return pending, nil
	}
	writer, err := s.prepare(ctx, decl, true)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if _, err := writer.Write(data); err != nil {
		return domain.PendingArtifact{}, errors.Join(err, writer.Abort(context.WithoutCancel(ctx)))
	}
	return writer.Finalize(ctx)
}

func (s *SandboxArtifactSink) Read(ctx context.Context, decl port.ArtifactDeclaration) (domain.PendingArtifact, bool, error) {
	p, d, found, err := s.load(ctx, decl, false)
	if err != nil || !found {
		return domain.PendingArtifact{}, false, err
	}
	stored, token, err := s.ledger.ReadArtifactWriter(ctx, d.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return domain.PendingArtifact{}, false, nil
	}
	if err != nil {
		return domain.PendingArtifact{}, false, err
	}
	// Declaration creation time is assigned by the artifact ledger, after
	// logical-call preparation; it is not the call's opening timestamp.
	d.CreatedAt = stored.CreatedAt
	if !reflect.DeepEqual(stored, d) {
		return domain.PendingArtifact{}, false, errors.New("sandbox artifact declaration differs from receipt")
	}
	if token.State == domain.ArtifactWriterSealed {
		session, err := NewPreparedArtifactSession(s.ledger, s.blobs, p)
		if err != nil {
			return domain.PendingArtifact{}, false, err
		}
		writer, err := session.Prepare(ctx, d.ID)
		if err != nil {
			return domain.PendingArtifact{}, false, err
		}
		if _, err := writer.Finalize(ctx); err != nil {
			return domain.PendingArtifact{}, false, err
		}
	} else if token.State != domain.ArtifactWriterFinalized {
		return domain.PendingArtifact{}, false, nil
	}
	pending, err := s.ledger.ReadPendingArtifact(ctx, d.ID)
	if err != nil {
		return domain.PendingArtifact{}, false, err
	}
	reader, err := s.blobs.OpenVerified(ctx, pending.Blob)
	if err != nil {
		return domain.PendingArtifact{}, false, err
	}
	_, readErr := io.Copy(io.Discard, reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return domain.PendingArtifact{}, false, err
	}
	if err := s.complete(ctx, d, pending); err != nil {
		return domain.PendingArtifact{}, false, err
	}
	return pending, true, nil
}

// ReadDeclared recovers a publication by its exact operation-owned path. The
// persisted declaration is still checked against the original call binding by
// Read; this does not authorize a new writer or an unrelated blob lookup.
func (s *SandboxArtifactSink) ReadDeclared(ctx context.Context, path domain.SafeRelPath) (domain.PendingArtifact, bool, error) {
	if err := path.Validate(); err != nil {
		return domain.PendingArtifact{}, false, err
	}
	id := domain.CallRecordID(coordinatorMutationID("callrec", "sandbox-artifact", s.identity.RunID, s.identity.AttemptID, s.identity.LogicalOperationID, path))
	declID := domain.ArtifactDeclarationID(coordinatorMutationID("decl", id))
	decl, _, err := s.ledger.ReadArtifactWriter(ctx, declID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return domain.PendingArtifact{}, false, nil
	}
	if err != nil {
		return domain.PendingArtifact{}, false, err
	}
	if decl.RunID != s.identity.RunID || decl.StageName != s.identity.StageName || decl.AttemptID != s.identity.AttemptID || decl.CallRecordID != id || decl.LogicalPath != path {
		return domain.PendingArtifact{}, false, errors.New("sandbox recovery declaration scope differs")
	}
	return s.Read(ctx, port.ArtifactDeclaration{MediaType: decl.MediaType, Role: decl.Role, LogicalPath: decl.LogicalPath, MaxBytes: decl.MaxBytes, Provenance: decl.Provenance})
}

func (s *SandboxArtifactSink) complete(ctx context.Context, d domain.ArtifactDeclarationRecord, pending domain.PendingArtifact) error {
	p, err := s.ledger.LoadCall(ctx, d.CallRecordID)
	if err != nil {
		return err
	}
	if len(p.PhysicalCalls) != 1 || p.PhysicalCalls[0].ID != pending.CallID || pending.CallID != d.AttemptCallID {
		return errors.New("sandbox artifact physical receipt differs")
	}
	physical := p.PhysicalCalls[0]
	if physical.State == domain.PhysicalDispatching {
		grant, err := s.ledger.ResumeDispatch(ctx, s.identity.ExpectedRunVersion, physical.ID)
		if err != nil {
			return err
		}
		if err := s.ledger.MarkSent(ctx, grant, s.clock.Now().UTC()); err != nil {
			return err
		}
		physical.State = domain.PhysicalSent
	}
	if physical.State == domain.PhysicalSent {
		err := s.ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: d.RunID, ExpectedRunVersion: s.identity.ExpectedRunVersion, StageName: d.StageName, AttemptID: d.AttemptID, CallRecordID: d.CallRecordID, AttemptCallID: d.AttemptCallID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "local-artifact:" + string(pending.WriterTokenID), ResponseDigest: &pending.Blob.Digest, Usage: []domain.ReservationUsage{{ReservationID: d.ReservationID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: d.ReservationSubkey, Value: pending.PhysicalNewBytes, Verified: true}}, IdempotencyKey: coordinatorMutationID("complete", d.AttemptCallID), At: s.clock.Now().UTC()})
		if err != nil {
			return err
		}
	} else if physical.State != domain.PhysicalCompleted || physical.Outcome == nil || *physical.Outcome != domain.PhysicalOutcomeSuccess || physical.ResponseDigest == nil || *physical.ResponseDigest != pending.Blob.Digest {
		return errors.New("sandbox artifact lacks a successful publication receipt")
	}
	if p.Call.State != domain.CallRecordTerminal {
		_, err = s.ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: d.RunID, ExpectedRunVersion: s.identity.ExpectedRunVersion, StageName: d.StageName, AttemptID: d.AttemptID, CallRecordID: d.CallRecordID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &d.AttemptCallID, IdempotencyKey: coordinatorMutationID("finish", d.CallRecordID), At: s.clock.Now().UTC()})
	}
	return err
}

type sandboxArtifactWriter struct {
	port.ArtifactWriter
	sink        *SandboxArtifactSink
	declaration domain.ArtifactDeclarationRecord
	grant       domain.DispatchGrant
	pending     *domain.PendingArtifact
	aborted     bool
}

func (w *sandboxArtifactWriter) Finalize(ctx context.Context) (domain.PendingArtifact, error) {
	if w.aborted {
		return domain.PendingArtifact{}, errors.New("sandbox artifact was aborted")
	}
	if w.pending == nil {
		pending, err := w.ArtifactWriter.Finalize(ctx)
		if err != nil {
			return domain.PendingArtifact{}, err
		}
		w.pending = &pending
	}
	if err := w.sink.complete(ctx, w.declaration, *w.pending); err != nil {
		return domain.PendingArtifact{}, err
	}
	return *w.pending, nil
}

func (w *sandboxArtifactWriter) Abort(ctx context.Context) error {
	if w.pending != nil || w.aborted {
		return nil
	}
	if err := w.ArtifactWriter.Abort(ctx); err != nil {
		return err
	}
	d := w.declaration
	failure := &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	if err := w.sink.ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: d.RunID, ExpectedRunVersion: w.grant.ExpectedRunVersion, StageName: d.StageName, AttemptID: d.AttemptID, CallRecordID: d.CallRecordID, AttemptCallID: d.AttemptCallID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: failure, IdempotencyKey: coordinatorMutationID("abort", d.AttemptCallID), At: w.sink.clock.Now().UTC()}); err != nil {
		return err
	}
	_, err := w.sink.ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: d.RunID, ExpectedRunVersion: w.grant.ExpectedRunVersion, StageName: d.StageName, AttemptID: d.AttemptID, CallRecordID: d.CallRecordID, DispatchKind: domain.DispatchNone, Failure: failure, IdempotencyKey: coordinatorMutationID("finish", d.CallRecordID), At: w.sink.clock.Now().UTC()})
	if err == nil {
		w.aborted = true
	}
	return err
}
