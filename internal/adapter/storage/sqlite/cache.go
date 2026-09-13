package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
)

var _ interface {
	Lookup(context.Context, domain.CacheLookup) (domain.CacheCandidate, bool, error)
	CommitReuse(context.Context, domain.CommitCacheReuse) (domain.PendingCacheReuse, error)
	Invalidate(context.Context, domain.CacheKey, domain.InvalidationCause) error
} = (*Store)(nil)

// PutCacheEntry installs a complete, already verified cache result. The
// method is intentionally not part of the lookup capability consumed by
// stages; producers use this narrow writer after publishing immutable Blobs.
func (s *Store) PutCacheEntry(ctx context.Context, entry domain.CacheEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	if entry.State != domain.CacheEntryValid {
		return errors.New("only VALID cache entries may be inserted")
	}
	sources := entry.Sources
	if len(sources) == 0 {
		sources = []domain.CacheSource{entry.Source}
	}
	if len(sources) != len(entry.Blobs) {
		return errors.New("cache source set must match complete blob set")
	}
	// Encode and hash provenance before opening the SQLite write transaction.
	// The transaction only persists already-canonical immutable bytes.
	payloads := make([][]byte, len(entry.Blobs))
	for index, item := range entry.Blobs {
		payload, err := json.Marshal(item.Provenance)
		if err != nil {
			return fmt.Errorf("encode cache blob %d provenance: %w", index, err)
		}
		payloads[index] = payload
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var existingKind, existingSchema, existingPolicy, existingInput, existingState string
		err := tx.QueryRowContext(ctx, `SELECT kind, schema_version, policy_digest, input_digest, state FROM cache_entries WHERE cache_key_digest = ?`, entry.Key.Digest).Scan(&existingKind, &existingSchema, &existingPolicy, &existingInput, &existingState)
		if err == nil {
			if existingKind != entry.Kind || existingSchema != string(entry.SchemaVersion) || existingPolicy != string(entry.PolicyDigest) || existingInput != string(entry.InputDigest) || existingState != string(domain.CacheEntryValid) {
				return wrap(ErrConsistency, "cache key was reused with different immutable content", nil)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var expires any
		if entry.ExpiresAt != nil {
			expires = formatTime(*entry.ExpiresAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_entries(cache_key_digest, kind, schema_version, policy_digest, input_digest, state, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, 'VALID', ?, ?)`, entry.Key.Digest, entry.Kind, entry.SchemaVersion, entry.PolicyDigest, entry.InputDigest, expires, formatTime(entry.CreatedAt)); err != nil {
			return fmt.Errorf("insert cache entry: %w", err)
		}
		for index, source := range sources {
			if _, err := tx.ExecContext(ctx, `INSERT INTO cache_entry_sources(cache_key_digest, source_run_id, source_call_record_id, source_occurrence_id, source_digest)
				VALUES (?, ?, ?, ?, ?)`, entry.Key.Digest, source.RunID, source.CallRecordID, source.OccurrenceID, source.Digest); err != nil {
				return fmt.Errorf("insert cache source %d: %w", index, err)
			}
		}
		for index, item := range entry.Blobs {
			payload := payloads[index]
			if _, err := tx.ExecContext(ctx, `INSERT INTO cache_blob_refs(cache_key_digest, digest, size, role, media_type, logical_path, provenance_json, provenance_digest)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, entry.Key.Digest, item.Blob.Digest, item.Blob.Size, item.Role, item.MediaType, item.LogicalPath, payload, domain.SumBytes(payload)); err != nil {
				return fmt.Errorf("insert cache blob %d: %w", index, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO cache_blob_order(cache_key_digest, blob_ordinal, digest, size, role, source_occurrence_id)
				VALUES (?, ?, ?, ?, ?, ?)`, entry.Key.Digest, index+1, item.Blob.Digest, item.Blob.Size, item.Role, sources[index].OccurrenceID); err != nil {
				return fmt.Errorf("insert cache blob order %d: %w", index, err)
			}
		}
		return nil
	})
}

func (s *Store) CreateCacheEntry(ctx context.Context, entry domain.CacheEntry) error {
	return s.PutCacheEntry(ctx, entry)
}

func (s *Store) Lookup(ctx context.Context, lookup domain.CacheLookup) (domain.CacheCandidate, bool, error) {
	if err := lookup.Validate(); err != nil {
		return domain.CacheCandidate{}, false, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.CacheCandidate{}, false, err
	}
	entry, err := readCacheEntry(ctx, connection, lookup.Key)
	if errors.Is(err, sql.ErrNoRows) {
		_ = connection.Close()
		return domain.CacheCandidate{}, false, nil
	}
	if err != nil {
		_ = connection.Close()
		return domain.CacheCandidate{}, false, err
	}
	if entry.State != domain.CacheEntryValid {
		_ = connection.Close()
		return domain.CacheCandidate{}, false, nil
	}
	if entry.ExpiresAt != nil && !lookup.At.Before(*entry.ExpiresAt) {
		_ = connection.Close()
		if err := s.Invalidate(ctx, lookup.Key, domain.InvalidationExpired); err != nil && !errors.Is(err, ErrNotFound) {
			return domain.CacheCandidate{}, false, err
		}
		return domain.CacheCandidate{}, false, nil
	}
	if entry.Kind != lookup.Key.Kind || entry.SchemaVersion != lookup.SchemaVersion || entry.PolicyDigest != lookup.PolicyDigest || entry.InputDigest != lookup.InputDigest {
		_ = connection.Close()
		return domain.CacheCandidate{}, false, nil
	}
	candidate, err := readCacheCandidate(ctx, connection, entry)
	closeErr := connection.Close()
	if err != nil {
		return domain.CacheCandidate{}, false, err
	}
	if closeErr != nil {
		return domain.CacheCandidate{}, false, closeErr
	}
	if err := candidate.ValidateFor(lookup); err != nil {
		return domain.CacheCandidate{}, false, nil
	}
	if candidate.Entry.Key.Digest == "" {
		return domain.CacheCandidate{}, false, nil
	}
	return candidate, true, nil
}

func (s *Store) CommitReuse(ctx context.Context, request domain.CommitCacheReuse) (domain.PendingCacheReuse, error) {
	if err := request.Validate(); err != nil {
		return domain.PendingCacheReuse{}, err
	}
	// This entry point has scalar semantics.  Reject a collection-shaped
	// request and inspect the immutable cache cardinality before delegating to
	// the collection writer; otherwise CommitReuseCollection could persist
	// several rows and only then return the scalar cardinality error.
	if len(request.CacheReuseRecordIDs) != 0 {
		return domain.PendingCacheReuse{}, errors.New("scalar cache reuse cannot contain collection record identities")
	}
	candidate, err := s.readCacheCandidateSnapshot(ctx, request.Key)
	if err != nil {
		return domain.PendingCacheReuse{}, err
	}
	if len(candidate.Entry.Blobs) != 1 || len(request.SourceOccurrenceIDs) != 1 {
		return domain.PendingCacheReuse{}, errors.New("scalar cache reuse requires exactly one output occurrence")
	}
	results, err := s.CommitReuseCollection(ctx, request)
	if err != nil {
		return domain.PendingCacheReuse{}, err
	}
	if len(results) != 1 {
		return domain.PendingCacheReuse{}, errors.New("cache reuse collection requires CommitReuseCollection")
	}
	return results[0], nil
}

// CommitReuseCollection commits one immutable reuse record per source
// occurrence. The result order is the cache entry's durable blob order, so a
// caller can attach the complete collection without inventing or dropping
// output occurrences.
func (s *Store) CommitReuseCollection(ctx context.Context, request domain.CommitCacheReuse) ([]domain.PendingCacheReuse, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	// Read and canonicalize provenance before entering the write transaction.
	// The transaction below only checks that these immutable bytes did not
	// change and records the reuse rows.
	candidate, err := s.readCacheCandidateSnapshot(ctx, request.Key)
	if err != nil {
		return nil, err
	}
	canonicalPayloads := make([][]byte, len(candidate.Entry.Blobs))
	for index, item := range candidate.Entry.Blobs {
		canonicalPayload, err := json.Marshal(item.Provenance)
		if err != nil {
			return nil, fmt.Errorf("encode cache blob %d provenance: %w", index, err)
		}
		canonicalPayloads[index] = canonicalPayload
	}
	var results []domain.PendingCacheReuse
	err = s.immediate(ctx, func(tx *immediateTx) error {
		entry, err := readCacheEntry(ctx, tx, request.Key)
		if err != nil {
			return err
		}
		if entry.State != domain.CacheEntryValid || (entry.ExpiresAt != nil && !request.At.Before(*entry.ExpiresAt)) {
			return wrap(ErrInvalidTransition, "cache entry is not reusable", nil)
		}
		if entry.Kind != candidate.Entry.Kind || entry.SchemaVersion != candidate.Entry.SchemaVersion || entry.PolicyDigest != candidate.Entry.PolicyDigest || entry.InputDigest != candidate.Entry.InputDigest || entry.State != candidate.Entry.State {
			return wrap(ErrConsistency, "cache entry changed during reuse", nil)
		}
		if len(candidate.Entry.Blobs) == 0 || len(candidate.Entry.Blobs) != len(request.SourceOccurrenceIDs) {
			return errors.New("cache reuse requires the complete output occurrence collection")
		}
		var callRun, callStage, callAttempt, dispatch string
		if err := tx.QueryRowContext(ctx, `SELECT run_id, stage_name, attempt_id, COALESCE(dispatch_kind, '') FROM call_records WHERE call_record_id = ?`, request.CurrentCallRecordID).Scan(&callRun, &callStage, &callAttempt, &dispatch); err != nil {
			return err
		}
		if callRun != string(request.RunID) || callStage != string(request.StageName) || callAttempt != string(request.AttemptID) || dispatch != string(domain.DispatchCacheHit) {
			return wrap(ErrConsistency, "current call is not the cache-hit call for this attempt", nil)
		}
		ids := request.CacheReuseRecordIDs
		if len(ids) == 0 && request.CacheReuseRecordID != "" {
			ids = []domain.CacheReuseRecordID{request.CacheReuseRecordID}
		}
		if len(ids) != len(candidate.Entry.Blobs) {
			return errors.New("cache reuse requires one record identity per output occurrence")
		}
		for index, item := range candidate.Entry.Blobs {
			sourceID := request.SourceOccurrenceIDs[index]
			var sourceRun, sourceCall, sourceDigest string
			if err := tx.QueryRowContext(ctx, `SELECT source_run_id, source_call_record_id, source_digest FROM cache_entry_sources
				WHERE cache_key_digest = ? AND source_occurrence_id = ?`, request.Key.Digest, sourceID).Scan(&sourceRun, &sourceCall, &sourceDigest); err != nil {
				return err
			}
			if sourceCall != string(request.SourceCallRecordID) || sourceRun != string(request.RunID) || sourceDigest != string(item.Blob.Digest) {
				return wrap(ErrConsistency, "cache source is outside current run or blob order", nil)
			}
			if item.SourceOccurrenceID != sourceID {
				return wrap(ErrConsistency, "cache blob/source occurrence order drifted", nil)
			}
			var role, media, path, payload, provenanceDigest string
			var blobDigest string
			var blobSize int64
			if err := tx.QueryRowContext(ctx, `SELECT digest, size, role, media_type, logical_path, provenance_json, provenance_digest FROM cache_blob_refs WHERE cache_key_digest = ? AND digest = ? AND size = ? AND role = ?`, request.Key.Digest, item.Blob.Digest, item.Blob.Size, item.Role).Scan(&blobDigest, &blobSize, &role, &media, &path, &payload, &provenanceDigest); err != nil {
				return err
			}
			if blobDigest != string(item.Blob.Digest) || blobSize != item.Blob.Size {
				return wrap(ErrConsistency, "cache blob identity drifted", nil)
			}
			if string(canonicalPayloads[index]) != payload || domain.SumBytes(canonicalPayloads[index]) != domain.Digest(provenanceDigest) {
				return wrap(ErrConsistency, "cache blob provenance digest is invalid", nil)
			}
			reuseID := ids[index]
			if _, err := tx.ExecContext(ctx, `INSERT INTO cache_reuse_records(cache_reuse_record_id, run_id, stage_name, attempt_id, current_call_record_id, source_call_record_id, source_occurrence_id, digest, size, cache_key_digest, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, reuseID, request.RunID, request.StageName, request.AttemptID, request.CurrentCallRecordID, request.SourceCallRecordID, sourceID, blobDigest, blobSize, request.Key.Digest, formatTime(request.At)); err != nil {
				if !isConstraint(err) {
					return err
				}
				var existingRun, existingStage, existingAttempt, existingCurrentCall, existingSourceCall, existingSourceOccurrence, existingDigest, existingKey string
				var existingSize int64
				if scanErr := tx.QueryRowContext(ctx, `SELECT run_id, stage_name, attempt_id, current_call_record_id, source_call_record_id, source_occurrence_id, digest, size, cache_key_digest FROM cache_reuse_records WHERE cache_reuse_record_id = ?`, reuseID).Scan(&existingRun, &existingStage, &existingAttempt, &existingCurrentCall, &existingSourceCall, &existingSourceOccurrence, &existingDigest, &existingSize, &existingKey); scanErr != nil {
					return err
				}
				if existingRun != string(request.RunID) || existingStage != string(request.StageName) || existingAttempt != string(request.AttemptID) || existingCurrentCall != string(request.CurrentCallRecordID) || existingSourceCall != string(request.SourceCallRecordID) || existingSourceOccurrence != string(sourceID) || existingDigest != blobDigest || existingSize != blobSize || existingKey != string(request.Key.Digest) {
					return wrap(ErrConsistency, "cache reuse identity drifted", nil)
				}
			}
			pending := domain.PendingCacheReuse{CacheReuseRecordID: reuseID, SourceOccurrenceID: sourceID, SourceCallRecordID: request.SourceCallRecordID, CurrentCallRecordID: request.CurrentCallRecordID, Blob: domain.BlobRef{Digest: domain.Digest(blobDigest), Size: blobSize}, MediaType: media, Role: domain.ArtifactRole(role), LogicalPath: domain.SafeRelPath(path), Provenance: item.Provenance}
			if err := pending.Validate(); err != nil {
				return err
			}
			results = append(results, pending)
		}
		return nil
	})
	return results, err
}

func (s *Store) readCacheCandidateSnapshot(ctx context.Context, key domain.CacheKey) (domain.CacheCandidate, error) {
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.CacheCandidate{}, err
	}
	defer connection.Close()
	entry, err := readCacheEntry(ctx, connection, key)
	if err != nil {
		return domain.CacheCandidate{}, err
	}
	candidate, err := readCacheCandidate(ctx, connection, entry)
	if err != nil {
		return domain.CacheCandidate{}, err
	}
	if err := candidate.Validate(); err != nil {
		return domain.CacheCandidate{}, wrap(ErrConsistency, "stored cache entry is invalid", err)
	}
	return candidate, nil
}

func (s *Store) Invalidate(ctx context.Context, key domain.CacheKey, cause domain.InvalidationCause) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if !cause.Valid() {
		return fmt.Errorf("invalid cache invalidation cause %q", cause)
	}
	return s.immediate(ctx, func(tx *immediateTx) error { return s.invalidateTx(ctx, tx, key, cause) })
}

func (s *Store) invalidateTx(ctx context.Context, tx *immediateTx, key domain.CacheKey, cause domain.InvalidationCause) error {
	result, err := tx.ExecContext(ctx, `UPDATE cache_entries SET state = 'INVALIDATED', invalidation_cause = ? WHERE cache_key_digest = ? AND state = 'VALID'`, cause, key.Digest)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM cache_entries WHERE cache_key_digest = ?`, key.Digest).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "cache entry does not exist", err)
		} else if err != nil {
			return err
		}
	}
	return nil
}

func readCacheEntry(ctx context.Context, queryer rowQuerier, key domain.CacheKey) (domain.CacheEntry, error) {
	var entry domain.CacheEntry
	var state, expires, created string
	if err := queryer.QueryRowContext(ctx, `SELECT cache_key_digest, kind, schema_version, policy_digest, input_digest, state, COALESCE(expires_at, ''), created_at FROM cache_entries WHERE cache_key_digest = ?`, key.Digest).Scan(&entry.Key.Digest, &entry.Kind, &entry.SchemaVersion, &entry.PolicyDigest, &entry.InputDigest, &state, &expires, &created); err != nil {
		return entry, err
	}
	entry.Key.Kind, entry.State = entry.Kind, domain.CacheEntryState(state)
	var err error
	entry.CreatedAt, err = parseTime(created)
	if err != nil {
		return entry, err
	}
	if expires != "" {
		parsed, err := parseTime(expires)
		if err != nil {
			return entry, err
		}
		entry.ExpiresAt = &parsed
	}
	if !entry.State.Valid() {
		return entry, wrap(ErrConsistency, "stored cache entry state is invalid", nil)
	}
	return entry, nil
}

func readCacheCandidate(ctx context.Context, queryer interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, entry domain.CacheEntry) (domain.CacheCandidate, error) {
	// Sources are provenance for the ordered blob collection.  Ordering them
	// by occurrence ID is not canonical (IDs are random), and can pair a
	// source with the wrong output when the durable collection order differs.
	// The order projection is the single ordering authority for both slices.
	rows, err := queryer.QueryContext(ctx, `SELECT source.source_run_id, source.source_call_record_id, source.source_occurrence_id, source.source_digest
		FROM cache_entry_sources source
		JOIN cache_blob_order order_rows ON order_rows.cache_key_digest = source.cache_key_digest
			AND order_rows.source_occurrence_id = source.source_occurrence_id
		WHERE source.cache_key_digest = ? ORDER BY order_rows.blob_ordinal`, entry.Key.Digest)
	if err != nil {
		return domain.CacheCandidate{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var source domain.CacheSource
		if err := rows.Scan(&source.RunID, &source.CallRecordID, &source.OccurrenceID, &source.Digest); err != nil {
			return domain.CacheCandidate{}, err
		}
		entry.Sources = append(entry.Sources, source)
	}
	if err := rows.Err(); err != nil {
		return domain.CacheCandidate{}, err
	}
	rows, err = queryer.QueryContext(ctx, `SELECT refs.digest, refs.size, refs.role, refs.media_type, refs.logical_path, refs.provenance_json, refs.provenance_digest, order_rows.source_occurrence_id
		FROM cache_blob_order order_rows JOIN cache_blob_refs refs ON refs.cache_key_digest = order_rows.cache_key_digest AND refs.digest = order_rows.digest AND refs.size = order_rows.size AND refs.role = order_rows.role
		WHERE order_rows.cache_key_digest = ? ORDER BY order_rows.blob_ordinal`, entry.Key.Digest)
	if err != nil {
		return domain.CacheCandidate{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.CacheBlob
		var payload, provenanceDigest string
		var sourceOccurrenceID sql.NullString
		if err := rows.Scan(&item.Blob.Digest, &item.Blob.Size, &item.Role, &item.MediaType, &item.LogicalPath, &payload, &provenanceDigest, &sourceOccurrenceID); err != nil {
			return domain.CacheCandidate{}, err
		}
		if sourceOccurrenceID.Valid {
			item.SourceOccurrenceID = domain.ArtifactOccurrenceID(sourceOccurrenceID.String)
		}
		if err := json.Unmarshal([]byte(payload), &item.Provenance); err != nil {
			return domain.CacheCandidate{}, err
		}
		canonicalPayload, err := json.Marshal(item.Provenance)
		if err != nil || string(canonicalPayload) != payload || domain.SumBytes(canonicalPayload) != domain.Digest(provenanceDigest) {
			return domain.CacheCandidate{}, wrap(ErrConsistency, "cache blob provenance digest is invalid", nil)
		}
		entry.Blobs = append(entry.Blobs, item)
	}
	if err := rows.Err(); err != nil {
		return domain.CacheCandidate{}, err
	}
	if len(entry.Sources) > 0 {
		entry.Source = entry.Sources[0]
	}
	candidate := domain.CacheCandidate{Entry: entry}
	if len(entry.Sources) > 0 {
		candidate.SourceCallRecordID = entry.Sources[0].CallRecordID
	}
	for _, item := range entry.Blobs {
		candidate.SourceOccurrenceIDs = append(candidate.SourceOccurrenceIDs, item.SourceOccurrenceID)
	}
	return candidate, nil
}

func parseCacheTime(value string) (time.Time, error) { return parseTime(value) }

func isConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}
