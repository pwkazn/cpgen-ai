package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// privateResponseSession owns only local publication slots. Payload validation
// and provider execution stay in their typed adapters. Prefix and schema values
// are selected by compiled constructors, preserving historical receipt IDs.
type privateResponseSession struct {
	privateResponseBinding
	ledger LLMArtifactLedger
	blobs  *blob.Store
	clock  clock.Clock
}

type privateResponseBinding struct {
	open                          domain.OpenCallRequest
	binding                       domain.Digest
	maxBytes                      int64
	callID                        domain.CallRecordID
	prefix, mediaType, pathPrefix string
	schema                        domain.SchemaVersion
	role                          domain.ArtifactRole
}

type PrivateReceiptReadStore interface {
	LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error)
	ReadArtifactWriter(context.Context, domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error)
	ReadPendingArtifact(context.Context, domain.ArtifactDeclarationID) (domain.PendingArtifact, error)
}

func releasePrivateResponseSlots(ctx context.Context, session *privateResponseSession, reconcile bool) error {
	if reconcile {
		reader, ok := session.ledger.(interface {
			ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
		})
		if !ok {
			return errors.New("response reconciliation requires original local call reads")
		}
		// A crash after the provider OPEN but before artifact planning leaves
		// no local child. Cleanup must not create one to release nonexistent work.
		if _, err := reader.ReadLogicalCall(ctx, session.callID); errors.Is(err, sqlite.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
	}
	return session.ledger.ReleaseUnwrittenArtifactReservations(ctx, session.artifactOpen())
}

func (s privateResponseBinding) declaration(ordinal int64) domain.ArtifactDeclarationRecord {
	role := s.role
	if role == "" {
		role = domain.ArtifactEvidence
	}
	providerID := domain.AttemptCallID(coordinatorMutationID("call", s.open.ID, ordinal))
	physicalID := domain.AttemptCallID(coordinatorMutationID("artifact", s.callID, ordinal))
	return domain.ArtifactDeclarationRecord{ID: domain.ArtifactDeclarationID(coordinatorMutationID("decl", s.prefix, providerID)), RunID: s.open.RunID, StageName: s.open.StageName, AttemptID: s.open.AttemptID,
		CallRecordID: s.callID, AttemptCallID: physicalID, ReservationID: domain.ReservationID(coordinatorMutationID("res", physicalID, "response")), ReservationSubkey: "response",
		MediaType: s.mediaType, Role: role, LogicalPath: domain.SafeRelPath(s.pathPrefix + string(providerID) + ".json"), MaxBytes: s.maxBytes,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: s.schema, Producer: string(s.schema), InputDigest: &s.binding}, CreatedAt: s.open.At}
}

func (s privateResponseBinding) artifactOpen() domain.OpenCallRequest {
	open := s.open
	open.ID, open.IdempotencyKey = s.callID, coordinatorMutationID("open", s.callID)
	open.LogicalOperationID, open.Provider, open.RequestDigest = s.prefix+":"+string(s.open.ID), "private-blob", s.binding
	return open
}

func (s *privateResponseSession) prepare(ctx context.Context) (*domain.PortFailure, error) {
	open := s.artifactOpen()
	// This logical operation records local response artifacts; only its
	// LOCAL_ARTIFACT_WRITE physical calls consume bytes, never provider-call budget.
	record, err := s.ledger.OpenCall(ctx, open)
	if err != nil {
		return nil, err
	}
	var prepared domain.PreparedCalls
	if record.State == domain.CallRecordOpen {
		calls := make([]domain.PhysicalCallPlan, s.open.RetryPolicy.MaxAttempts)
		for i := range calls {
			decl := s.declaration(int64(i + 1))
			calls[i] = domain.PhysicalCallPlan{ID: decl.AttemptCallID, Ordinal: int64(i + 1), RetryGroup: s.prefix, RetryOrdinal: int64(i + 1), Kind: domain.PhysicalLocalArtifactWrite, Provider: "private-blob", RequestDigest: s.binding, IdempotencyKey: coordinatorMutationID("physical", decl.AttemptCallID), Reservations: []domain.ReservationPlan{{ID: decl.ReservationID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: decl.ReservationSubkey, UpperBound: s.maxBytes}}}
		}
		raw, err := json.Marshal(calls)
		if err != nil {
			return nil, err
		}
		prepared, err = s.ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: open.RunID, ExpectedRunVersion: open.ExpectedRunVersion, StageName: open.StageName, AttemptID: open.AttemptID, CallRecordID: open.ID,
			PlanDigest: domain.SumBytes(raw), Calls: calls, IdempotencyKey: coordinatorMutationID("prepare", open.ID), At: s.clock.Now()})
		if err != nil {
			return nil, err
		}
	} else {
		prepared, err = s.ledger.LoadCall(ctx, record.ID)
		if err != nil {
			return nil, err
		}
	}
	if prepared.Failure != nil {
		return prepared.Failure, nil
	}
	if prepared.Call.State != domain.CallRecordPrepared {
		return nil, errors.New("response artifact call is not prepared")
	}
	for i := int64(1); i <= open.RetryPolicy.MaxAttempts; i++ {
		if err := s.ledger.CreateArtifactDeclaration(ctx, s.declaration(i)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s privateResponseBinding) boundDeclaration(ctx context.Context, ledger PrivateReceiptReadStore, grant domain.DispatchGrant) (domain.ArtifactDeclarationRecord, domain.PreparedCalls, error) {
	if grant.CallRecordID != s.open.ID || grant.RunID != s.open.RunID || grant.StageName != s.open.StageName || grant.AttemptID != s.open.AttemptID || grant.Ordinal < 1 || grant.Ordinal > s.open.RetryPolicy.MaxAttempts || grant.AttemptCallID != domain.AttemptCallID(coordinatorMutationID("call", s.open.ID, grant.Ordinal)) {
		return domain.ArtifactDeclarationRecord{}, domain.PreparedCalls{}, errors.New("response artifact scope differs from dispatch")
	}
	prepared, err := ledger.LoadCall(ctx, s.callID)
	if err != nil {
		return domain.ArtifactDeclarationRecord{}, prepared, err
	}
	if prepared.Call.RunID != s.open.RunID || prepared.Call.StageName != s.open.StageName || prepared.Call.AttemptID != s.open.AttemptID || prepared.Call.RequestDigest != s.binding || prepared.Call.PolicyDigest != s.open.PolicyDigest {
		return domain.ArtifactDeclarationRecord{}, prepared, errors.New("response artifact request binding differs")
	}
	return s.declaration(grant.Ordinal), prepared, nil
}

func (s *privateResponseSession) writer(ctx context.Context, grant domain.DispatchGrant) (port.ArtifactWriter, error) {
	decl, prepared, err := s.privateResponseBinding.boundDeclaration(ctx, s.ledger, grant)
	if err != nil {
		return nil, err
	}
	session, err := NewPreparedArtifactSession(s.ledger, s.blobs, prepared)
	if err != nil {
		return nil, err
	}
	writer, err := session.Prepare(ctx, decl.ID)
	if err != nil {
		return nil, err
	}
	if _, err := s.beginArtifact(ctx, decl); err != nil {
		_ = writer.Abort(context.WithoutCancel(ctx))
		return nil, err
	}
	return writer, nil
}

func (s *privateResponseSession) beginArtifact(ctx context.Context, decl domain.ArtifactDeclarationRecord) (domain.DispatchGrant, error) {
	return s.ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: s.open.RunID, ExpectedRunVersion: s.open.ExpectedRunVersion, StageName: s.open.StageName, AttemptID: s.open.AttemptID, CallRecordID: s.callID, AttemptCallID: decl.AttemptCallID, IdempotencyKey: coordinatorMutationID("begin", decl.AttemptCallID), At: s.clock.Now()})
}

// abort records that no private response was published. It also releases the
// current local slot before the provider's next transport attempt needs it.
func (s *privateResponseSession) abort(ctx context.Context, grant domain.DispatchGrant, writer port.ArtifactWriter) error {
	if err := writer.Abort(ctx); err != nil {
		return err
	}
	decl := s.declaration(grant.Ordinal)
	return s.ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: s.open.RunID, ExpectedRunVersion: s.open.ExpectedRunVersion, StageName: s.open.StageName, AttemptID: s.open.AttemptID, CallRecordID: s.callID, AttemptCallID: decl.AttemptCallID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: coordinatorMutationID("abort", decl.AttemptCallID), At: s.clock.Now()})
}

func (s *privateResponseSession) markPublished(ctx context.Context, ordinal int64) error {
	decl := s.declaration(ordinal)
	prepared, err := s.ledger.LoadCall(ctx, s.callID)
	if err != nil {
		return err
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID != decl.AttemptCallID {
			continue
		}
		if physical.State == domain.PhysicalSent || physical.State == domain.PhysicalCompleted {
			return nil
		}
		var grant domain.DispatchGrant
		if physical.State == domain.PhysicalPrepared {
			// Older private receipts predate explicit local dispatch. Restoring
			// their publication can authorize only this local artifact slot.
			grant, err = s.beginArtifact(ctx, decl)
		} else if physical.State == domain.PhysicalDispatching {
			grant, err = s.ledger.ResumeDispatch(ctx, s.open.ExpectedRunVersion, physical.ID)
		} else {
			return errors.New("published artifact has no live local dispatch")
		}
		if err != nil {
			return err
		}
		return s.ledger.MarkSent(ctx, grant, s.clock.Now())
	}
	return errors.New("published artifact physical call is missing")
}

// readReceipt restores a sealed local publication and verifies its bytes. It
// never authorizes a provider exchange. Typed callers validate the receipt's
// schema, provider result and request binding before marking it published.
func (s *privateResponseSession) readReceipt(ctx context.Context, grant domain.DispatchGrant) ([]byte, domain.PendingArtifact, bool, error) {
	return s.privateResponseBinding.readReceipt(ctx, s.ledger, s.blobs, grant, func(decl domain.ArtifactDeclarationRecord, prepared domain.PreparedCalls) error {
		session, err := NewPreparedArtifactSession(s.ledger, s.blobs, prepared)
		if err != nil {
			return err
		}
		writer, err := session.Prepare(ctx, decl.ID)
		if err != nil {
			return err
		}
		if _, err := writer.Finalize(ctx); err != nil {
			return fmt.Errorf("%w: %w", errCallReceiptPending, err)
		}
		return nil
	})
}

func (s privateResponseBinding) readReceipt(ctx context.Context, ledger PrivateReceiptReadStore, blobs port.VerifiedBlobReader, grant domain.DispatchGrant, finalize func(domain.ArtifactDeclarationRecord, domain.PreparedCalls) error) ([]byte, domain.PendingArtifact, bool, error) {
	var empty domain.PendingArtifact
	decl, prepared, err := s.boundDeclaration(ctx, ledger, grant)
	if err != nil {
		return nil, empty, false, err
	}
	stored, token, err := ledger.ReadArtifactWriter(ctx, decl.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil, empty, false, nil
	}
	if err != nil {
		return nil, empty, false, err
	}
	if stored.CallRecordID != decl.CallRecordID || stored.AttemptCallID != decl.AttemptCallID || stored.ReservationID != decl.ReservationID || stored.LogicalPath != decl.LogicalPath || stored.MediaType != decl.MediaType || stored.MaxBytes != decl.MaxBytes || stored.Role != decl.Role || stored.Provenance.InputDigest == nil || *stored.Provenance.InputDigest != s.binding {
		return nil, empty, false, errors.New("response artifact declaration differs")
	}
	if token.State == domain.ArtifactWriterPrepared || token.State == domain.ArtifactWriterOpen || token.State == domain.ArtifactWriterReleased {
		return nil, empty, false, nil
	}
	if token.State == domain.ArtifactWriterSealed {
		if finalize == nil {
			return nil, empty, false, errors.New("committed receipt has an unfinished writer")
		}
		if err := finalize(decl, prepared); err != nil {
			return nil, empty, false, err
		}
	}

	pending, err := ledger.ReadPendingArtifact(ctx, decl.ID)
	if err != nil {
		return nil, empty, false, err
	}
	if pending.Blob.Size > s.maxBytes {
		return nil, empty, false, errors.New("private response artifact exceeds bound")
	}
	reader, err := blobs.OpenVerified(ctx, pending.Blob)
	if err != nil {
		return nil, empty, false, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, s.maxBytes+1))
	if closeErr := reader.Close(); err != nil || closeErr != nil {
		return nil, empty, false, errors.Join(err, closeErr)
	}
	return raw, pending, true, nil
}
