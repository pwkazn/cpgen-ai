package execution

import (
	"context"
	"errors"
	"fmt"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type CacheReuseResult struct {
	Hit       bool
	Candidate domain.CacheCandidate
	Reuse     domain.PendingCacheReuse
	Reuses    []domain.PendingCacheReuse
	Trace     domain.CallTrace
}

type CacheVerificationError struct {
	Key   domain.CacheKey
	Cause error
}

func (e *CacheVerificationError) Error() string {
	return fmt.Sprintf("cache candidate %s failed verification: %v", e.Key.Digest, e.Cause)
}
func (e *CacheVerificationError) Unwrap() error { return e.Cause }

type CacheService struct {
	locks *runlock.Manager
	cache port.CacheStore
	calls port.CallLedger
	blobs port.VerifiedBlobReader
}

func NewCacheService(locks *runlock.Manager, cache port.CacheStore, calls port.CallLedger, blobs port.VerifiedBlobReader) (*CacheService, error) {
	if locks == nil || cache == nil || calls == nil || blobs == nil {
		return nil, errors.New("cache service dependencies are required")
	}
	return &CacheService{locks: locks, cache: cache, calls: calls, blobs: blobs}, nil
}

func (s *CacheService) Reuse(ctx context.Context, request domain.CacheReuseRequest) (CacheReuseResult, error) {
	return s.ReuseValidated(ctx, request, nil)
}

// ReuseValidated applies the owning adapter's local output validator after
// verified Blob reads and before any current-call or reuse record is created.
// The callback runs under the shared artifact lock, outside a write transaction.
func (s *CacheService) ReuseValidated(ctx context.Context, request domain.CacheReuseRequest, validate func(domain.CacheCandidate) error) (CacheReuseResult, error) {
	if err := request.Lookup.Validate(); err != nil {
		return CacheReuseResult{}, err
	}
	guard, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return CacheReuseResult{}, err
	}
	defer guard.Close()
	candidate, found, err := s.cache.Lookup(ctx, request.Lookup)
	if err != nil || !found {
		return CacheReuseResult{Hit: false}, err
	}
	if err := candidate.ValidateFor(request.Lookup); err != nil {
		return CacheReuseResult{}, err
	}
	for _, item := range candidate.Entry.Blobs {
		reader, verifyErr := s.blobs.OpenVerified(ctx, item.Blob)
		if verifyErr != nil {
			cause := domain.InvalidationMissingBlob
			if errors.Is(verifyErr, blob.ErrBlobCorrupt) {
				cause = domain.InvalidationCorruptBlob
			}
			if invalidateErr := s.cache.Invalidate(ctx, request.Lookup.Key, cause); invalidateErr != nil {
				verifyErr = errors.Join(verifyErr, invalidateErr)
			}
			return CacheReuseResult{}, &CacheVerificationError{Key: request.Lookup.Key, Cause: verifyErr}
		}
		if closeErr := reader.Close(); closeErr != nil {
			_ = s.cache.Invalidate(ctx, request.Lookup.Key, domain.InvalidationCorruptBlob)
			return CacheReuseResult{}, &CacheVerificationError{Key: request.Lookup.Key, Cause: closeErr}
		}
	}
	if validate != nil {
		if err := validate(candidate); err != nil {
			return CacheReuseResult{}, err
		}
	}
	open := request.OpenCall
	if open.RunID != request.Lookup.RunID || open.StageName == "" || open.Kind != domain.CallCacheReuse {
		return CacheReuseResult{}, errors.New("cache reuse logical call is not bound to lookup")
	}
	call, err := s.calls.OpenCall(ctx, open)
	if err != nil {
		return CacheReuseResult{}, err
	}
	finish := request.FinishCall
	finish.RunID, finish.StageName, finish.AttemptID, finish.CallRecordID = call.RunID, call.StageName, call.AttemptID, call.ID
	source := candidate.SourceCallRecordID
	finish.DispatchKind = domain.DispatchCacheHit
	finish.CacheSourceCallRecordID, finish.CacheHitCallRecordID = &source, &call.ID
	trace, err := s.calls.FinishCall(ctx, finish)
	if err != nil {
		return CacheReuseResult{}, err
	}
	reuse := request.Reuse
	reuse.RunID, reuse.StageName, reuse.AttemptID, reuse.CurrentCallRecordID = call.RunID, call.StageName, call.AttemptID, call.ID
	reuse.Key, reuse.SourceCallRecordID, reuse.SourceOccurrenceIDs = request.Lookup.Key, source, candidate.SourceOccurrenceIDs
	reuse.At = request.Lookup.At
	var reuses []domain.PendingCacheReuse
	if collection, ok := s.cache.(port.CacheReuseCollectionStore); ok {
		if len(reuse.CacheReuseRecordIDs) == 0 && reuse.CacheReuseRecordID == "" {
			if len(candidate.Entry.Blobs) == 1 {
				generated, genErr := domain.NewID("cache_reuse")
				if genErr != nil {
					return CacheReuseResult{}, genErr
				}
				reuse.CacheReuseRecordID = domain.CacheReuseRecordID(generated)
			} else {
				reuse.CacheReuseRecordIDs = make([]domain.CacheReuseRecordID, len(candidate.Entry.Blobs))
				for index := range reuse.CacheReuseRecordIDs {
					generated, genErr := domain.NewID("cache_reuse")
					if genErr != nil {
						return CacheReuseResult{}, genErr
					}
					reuse.CacheReuseRecordIDs[index] = domain.CacheReuseRecordID(generated)
				}
			}
		}
		if len(candidate.Entry.Blobs) > 1 {
			reuse.CacheReuseRecordID = ""
		}
		reuses, err = collection.CommitReuseCollection(ctx, reuse)
	} else {
		if len(candidate.Entry.Blobs) != 1 {
			return CacheReuseResult{}, errors.New("cache reuse collection is unsupported by the configured cache store")
		}
		if reuse.CacheReuseRecordID == "" {
			generated, genErr := domain.NewID("cache_reuse")
			if genErr != nil {
				return CacheReuseResult{}, genErr
			}
			reuse.CacheReuseRecordID = domain.CacheReuseRecordID(generated)
		}
		var pending domain.PendingCacheReuse
		pending, err = s.cache.CommitReuse(ctx, reuse)
		if err == nil {
			reuses = []domain.PendingCacheReuse{pending}
		}
	}
	if err != nil {
		return CacheReuseResult{}, err
	}
	if len(reuses) == 0 {
		return CacheReuseResult{}, errors.New("cache reuse returned an empty collection")
	}
	return CacheReuseResult{Hit: true, Candidate: candidate, Reuse: reuses[0], Reuses: reuses, Trace: trace}, nil
}
