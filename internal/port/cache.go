package port

import (
	"context"

	"cpgen/internal/domain"
)

// CacheStore is the narrow cache ledger. It never returns an unverified Blob;
// callers must verify every candidate before committing reuse provenance.
type CacheStore interface {
	Lookup(context.Context, domain.CacheLookup) (domain.CacheCandidate, bool, error)
	CommitReuse(context.Context, domain.CommitCacheReuse) (domain.PendingCacheReuse, error)
	Invalidate(context.Context, domain.CacheKey, domain.InvalidationCause) error
}

// CacheReuseCollectionStore is implemented by stores that can commit every
// Blob/occurrence in a collection-shaped cache hit atomically. CacheStore's
// scalar method remains for compatibility with existing one-output callers.
type CacheReuseCollectionStore interface {
	CommitReuseCollection(context.Context, domain.CommitCacheReuse) ([]domain.PendingCacheReuse, error)
}

// CacheEntryWriter is intentionally separate from CacheStore so ordinary
// pipeline stages cannot mutate cache entries while looking up values.
type CacheEntryWriter interface {
	PutCacheEntry(context.Context, domain.CacheEntry) error
}
