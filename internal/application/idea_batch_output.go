package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/domain"
)

const ideaBatchOutputSchema domain.SchemaVersion = "cpgen.idea-batch-publication/v1"

// CollectIdeaCandidatesWithOutput adds a typed batch OUTPUT to the private
// provider evidence. It is a primitive for a future compatible collection
// boundary; neither historical preview revision calls it. Its output requires
// a dedicated typed reader, rather than the historical provider-only reader.
func (s *GenerationExecutor) CollectIdeaCandidatesWithOutput(ctx context.Context, view domain.RunView, input domain.GenerationRequestSnapshotV1) (GenerationStageResult[domain.IdeaBatch], error) {
	result, err := s.CollectIdeaCandidates(ctx, view, input)
	if err != nil || result.Outcome.Value == nil {
		return result, err
	}
	batch := *result.Outcome.Value
	raw, err := batch.CanonicalJSON()
	if err != nil {
		return result, err
	}
	if len(raw) > 4<<20 {
		return result, errors.New("typed Idea batch publication exceeds its content bound")
	}
	// The first output policy requires an actual successful provider operation
	// in this attempt. Cache-derived publication needs its own source policy.
	calls, err := s.config.Store.ReadAttemptLLMCalls(ctx, view.RunID(), "idea", view.AttemptID())
	if err != nil {
		return result, err
	}
	var parent domain.CallRecord
	for _, call := range calls {
		if call.State == domain.CallRecordTerminal && call.Failure == nil {
			if parent.ID != "" {
				return result, errors.New("typed Idea output has ambiguous provider parents")
			}
			parent = call
		}
	}
	if parent.ID == "" || parent.ResultAttemptCallID == nil {
		return result, errors.New("typed Idea output requires a successful provider parent in the current attempt")
	}
	open, err := s.config.Store.ReadOpenCall(ctx, parent.ID)
	if err != nil {
		return result, err
	}
	ledger, err := NewRunBoundLLMLedger(s.config.Store, view.RunID(), "idea", view.AttemptID())
	if err != nil {
		return result, err
	}
	binding, err := json.Marshal(struct {
		Schema   domain.SchemaVersion `json:"schema"`
		Snapshot domain.Digest        `json:"snapshot"`
		Batch    domain.Digest        `json:"batch"`
		Blob     domain.Digest        `json:"blob"`
		Parent   domain.CallRecordID  `json:"parent"`
		Request  domain.Digest        `json:"request"`
	}{ideaBatchOutputSchema, input.SnapshotDigest, batch.BatchDigest, domain.SumBytes(raw), parent.ID, parent.RequestDigest})
	if err != nil {
		return result, err
	}
	session := &privateResponseSession{ledger: ledger, blobs: s.config.Blobs, clock: s.config.Clock, open: open,
		binding: domain.SumBytes(binding), maxBytes: int64(len(raw)), callID: domain.CallRecordID(coordinatorMutationID("callrec", "idea-batch-output", parent.ID)),
		prefix: "idea-batch-output", mediaType: "application/vnd.cpgen.idea-batch+json", pathPrefix: "private/idea-batches/", schema: ideaBatchOutputSchema, role: domain.ArtifactOutput}
	failure, err := session.prepare(ctx)
	if err != nil {
		return result, err
	}
	if failure != nil {
		reason := "idea_output_publication_unavailable"
		if failure.Code == domain.FailureBudgetExhausted {
			reason = "idea_output_budget_exhausted"
		}
		result.Outcome = domain.Review[domain.IdeaBatch](domain.ReviewRequest{EvidenceDigest: batch.BatchDigest,
			PolicyDigest: s.config.Content.ProviderPolicyDigest, Reason: reason})
		result.Occurrences, result.publication = nil, nil
		return result, nil
	}
	grant, err := ledger.ResumeDispatch(ctx, view.Version(), *parent.ResultAttemptCallID)
	if err != nil {
		return result, err
	}
	stored, pending, found, err := session.readReceipt(ctx, grant)
	if err != nil {
		return result, err
	}
	if found {
		if !bytes.Equal(stored, raw) {
			return result, errors.New("retained Idea output differs from verified provider content")
		}
	} else {
		writer, err := session.writer(ctx, grant)
		if err != nil {
			return result, err
		}
		if _, err := writer.Write(raw); err != nil {
			_ = writer.Abort(context.WithoutCancel(ctx))
			return result, err
		}
		pending, err = writer.Finalize(ctx)
		if err != nil {
			_, token, readErr := ledger.ReadArtifactWriter(ctx, session.declaration(grant.Ordinal).ID)
			if readErr == nil && (token.State == domain.ArtifactWriterSealed || token.State == domain.ArtifactWriterFinalized) {
				return result, fmt.Errorf("%w: typed Idea output: %w", errCallReceiptPending, err)
			}
			return result, err
		}
	}
	if err := session.markPublished(ctx, grant.Ordinal); err != nil {
		return result, fmt.Errorf("%w: %w", errCallReceiptPending, err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := releasePrivateResponseSlots(cleanupCtx, session, false); err != nil {
		return result, err
	}
	result.Occurrences = append(result.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
	// Provider cache indexing currently expects a provider-only stage. The
	// composing typed-output workflow must select its own indexing policy.
	result.publication = nil
	return result, nil
}
