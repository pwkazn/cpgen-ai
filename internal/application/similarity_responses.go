package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
)

const similarityResponseSchema domain.SchemaVersion = "cpgen.similarity-response/v1"
const similarityResponseMediaType = "application/vnd.cpgen.similarity-response+json"

// NewReplayableSimilarityCalls retains successful evidence on the private
// artifact protocol. Its caller holds the run and artifact-maintenance locks.
func NewReplayableSimilarityCalls(ledger LLMArtifactLedger, provider similarity.PhysicalProvider, blobs *blob.Store, source clock.Clock, costUpperBoundMicroUSD int64) (*SimilarityCalls, error) {
	if blobs == nil {
		return nil, errors.New("replayable similarity calls require private blob storage")
	}
	calls, err := NewSimilarityCalls(ledger, provider, source, costUpperBoundMicroUSD)
	if err != nil {
		return nil, err
	}
	calls.artifacts = &similarityResponseArtifacts{ledger, blobs, source}
	return calls, nil
}

type similarityResponseArtifacts struct {
	ledger LLMArtifactLedger
	blobs  *blob.Store
	clock  clock.Clock
}

type similarityResponseSession struct {
	*privateResponseSession
	request similarity.Request
}

func (s *similarityResponseArtifacts) session(open domain.OpenCallRequest, request similarity.Request, plan similarity.PhysicalSearchPlan) (*similarityResponseSession, error) {
	binding, err := similarityResponseBinding(open.ID, request, plan)
	if err != nil {
		return nil, err
	}
	core := &privateResponseSession{ledger: s.ledger, blobs: s.blobs, clock: s.clock, privateResponseBinding: privateResponseBinding{open: open, binding: binding, maxBytes: plan.MaxResponseBytes*6 + 16384,
		callID: domain.CallRecordID(coordinatorMutationID("callrec", "similarity-response", open.ID)), prefix: "similarity-response", mediaType: similarityResponseMediaType, pathPrefix: "private/similarity/", schema: similarityResponseSchema}}
	return &similarityResponseSession{core, request}, nil
}

func similarityResponseBinding(id domain.CallRecordID, request similarity.Request, plan similarity.PhysicalSearchPlan) (domain.Digest, error) {
	if plan.MaxResponseBytes <= 0 || plan.MaxResponseBytes > 64<<20 {
		return "", errors.New("private similarity response limit must be within 64 MiB")
	}
	raw, err := json.Marshal(struct {
		Schema  domain.SchemaVersion          `json:"schema"`
		Call    domain.CallRecordID           `json:"call"`
		Request similarity.Request            `json:"request"`
		Plan    similarity.PhysicalSearchPlan `json:"plan"`
	}{similarityResponseSchema, id, request, plan})
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

type similarityResponseReceipt struct {
	SchemaVersion  domain.SchemaVersion                          `json:"schema_version"`
	RequestBinding domain.Digest                                 `json:"request_binding"`
	CallRecordID   domain.CallRecordID                           `json:"call_record_id"`
	AttemptCallID  domain.AttemptCallID                          `json:"attempt_call_id"`
	Execution      domain.PhysicalExecution[similarity.Evidence] `json:"execution"`
}

func (s *similarityResponseSession) validate(ctx context.Context, grant domain.DispatchGrant, execution domain.PhysicalExecution[similarity.Evidence], provider SimilarityReadPolicy) error {
	prepared, err := s.ledger.LoadCall(ctx, grant.CallRecordID)
	if err != nil {
		return err
	}
	return validateSimilarityReceiptExecution(prepared, s.request, grant.AttemptCallID, execution, provider)
}

func validateSimilarityReceiptExecution(prepared domain.PreparedCalls, request similarity.Request, id domain.AttemptCallID, execution domain.PhysicalExecution[similarity.Evidence], provider SimilarityReadPolicy) error {
	if execution.Validate() != nil || execution.Boundary != domain.BoundaryCompleted || execution.Value == nil {
		return errors.New("invalid successful similarity receipt")
	}
	if err := provider.ValidatePhysicalResponse(request, *execution.Value); err != nil {
		return err
	}
	trace := execution.Value.CallTrace
	if trace.ResultAttemptCallID == nil || *trace.ResultAttemptCallID != id || len(trace.PhysicalAttemptCallIDs) != 1 || trace.PhysicalAttemptCallIDs[0] != id {
		return errors.New("similarity receipt substituted physical identity")
	}
	// The receipt carries exactly the two admitted accounts for this exchange.
	// It cannot charge another attempt or reinterpret an omitted cost as zero.
	if len(execution.Usage) != 2 {
		return errors.New("similarity receipt lacks exact reservation usage")
	}
	matched := 0
	for _, reservation := range prepared.Reservations {
		if reservation.AttemptCallID != id {
			continue
		}
		count := 0
		for _, usage := range execution.Usage {
			if usage.ReservationID != reservation.ID {
				continue
			}
			count++
			if usage.Dimension != reservation.Dimension || usage.Subkey != reservation.Subkey || usage.Validate() != nil {
				return errors.New("similarity receipt usage binding differs")
			}
			switch reservation.Dimension {
			case domain.BudgetSimilarityCalls:
				if usage.Value != 1 || !usage.Verified {
					return errors.New("similarity receipt call count differs")
				}
			case domain.BudgetSimilarityCostMicroUSD:
				if (usage.Verified && usage.Value != execution.Value.Usage.CostMicroUSD) || (!usage.Verified && usage.Value != reservation.UpperBound) {
					return errors.New("similarity receipt cost differs")
				}
			default:
				return errors.New("similarity receipt contains a foreign account")
			}
		}
		if count != 1 {
			return errors.New("similarity receipt duplicates or omits an account")
		}
		matched++
	}
	if matched != 2 {
		return errors.New("similarity receipt has no admitted account pair")
	}
	return nil
}

func (s *similarityResponseSession) publish(ctx context.Context, grant domain.DispatchGrant, writer port.ArtifactWriter, execution domain.PhysicalExecution[similarity.Evidence], provider similarity.PhysicalProvider) (*domain.PendingArtifact, error) {
	if err := s.validate(ctx, grant, execution, provider); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(similarityResponseReceipt{similarityResponseSchema, s.binding, grant.CallRecordID, grant.AttemptCallID, execution})
	if err != nil {
		return nil, errors.New("cannot encode private similarity receipt")
	}
	if _, err := writer.Write(raw); err != nil {
		_ = writer.Abort(ctx)
		return nil, err
	}
	pending, err := writer.Finalize(ctx)
	if err != nil {
		_, token, readErr := s.ledger.ReadArtifactWriter(ctx, s.declaration(grant.Ordinal).ID)
		if readErr == nil && (token.State == domain.ArtifactWriterSealed || token.State == domain.ArtifactWriterFinalized) {
			return nil, fmt.Errorf("%w: %w", errCallReceiptPending, err)
		}
		return nil, err
	}
	if err := s.markPublished(ctx, grant.Ordinal); err != nil {
		return nil, fmt.Errorf("%w: %w", errCallReceiptPending, err)
	}
	return &pending, nil
}

func (s *similarityResponseSession) replay(ctx context.Context, grant domain.DispatchGrant, provider similarity.PhysicalProvider) (domain.PhysicalExecution[similarity.Evidence], *domain.PendingArtifact, bool, error) {
	var empty domain.PhysicalExecution[similarity.Evidence]
	raw, pending, found, err := s.readReceipt(ctx, grant)
	if err != nil || !found {
		return empty, nil, found, err
	}
	var receipt similarityResponseReceipt
	if err := port.DecodeStructuredOutput(raw, similarityResponseSchema, s.maxBytes, &receipt); err != nil {
		return empty, nil, false, err
	}
	if receipt.RequestBinding != s.binding || receipt.CallRecordID != grant.CallRecordID || receipt.AttemptCallID != grant.AttemptCallID {
		return empty, nil, false, errors.New("private similarity receipt binding differs")
	}
	if err := s.validate(ctx, grant, receipt.Execution, provider); err != nil {
		return empty, nil, false, err
	}
	if err := s.markPublished(ctx, grant.Ordinal); err != nil {
		return empty, nil, false, fmt.Errorf("%w: %w", errCallReceiptPending, err)
	}
	return receipt.Execution, &pending, true, nil
}
