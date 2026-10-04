package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// CacheKey is the immutable, content-addressed identity of a cache result.
// Kind remains a separate field so a digest accidentally produced for one
// CPGen operation cannot be consumed as another operation's result.
type CacheKey struct {
	Digest Digest `json:"digest"`
	Kind   string `json:"kind"`
}

func (v CacheKey) Validate() error {
	if err := v.Digest.Validate(); err != nil {
		return fmt.Errorf("cache key digest: %w", err)
	}
	if err := validateCacheKind(v.Kind); err != nil {
		return err
	}
	return nil
}

type CacheEntryState string

const (
	CacheEntryValid       CacheEntryState = "VALID"
	CacheEntryInvalidated CacheEntryState = "INVALIDATED"
)

func (v CacheEntryState) Valid() bool { return v == CacheEntryValid || v == CacheEntryInvalidated }

type InvalidationCause string

const (
	InvalidationCorruptBlob   InvalidationCause = "CORRUPT_BLOB"
	InvalidationMissingBlob   InvalidationCause = "MISSING_BLOB"
	InvalidationExpired       InvalidationCause = "EXPIRED"
	InvalidationPolicyChanged InvalidationCause = "POLICY_CHANGED"
	InvalidationManual        InvalidationCause = "MANUAL"
)

func (v InvalidationCause) Valid() bool {
	switch v {
	case InvalidationCorruptBlob, InvalidationMissingBlob, InvalidationExpired, InvalidationPolicyChanged, InvalidationManual:
		return true
	default:
		return false
	}
}

// CacheBlob is one immutable output of a cache entry. A complete set is
// required: a cache hit can never silently omit a referenced output.
type CacheBlob struct {
	Blob               BlobRef              `json:"blob"`
	SourceOccurrenceID ArtifactOccurrenceID `json:"source_occurrence_id,omitempty"`
	Role               ArtifactRole         `json:"role"`
	MediaType          string               `json:"media_type"`
	LogicalPath        SafeRelPath          `json:"logical_path"`
	Provenance         ProvenanceCandidate  `json:"provenance"`
}

func (v CacheBlob) Validate() error {
	if err := v.Blob.Validate(); err != nil {
		return err
	}
	if !v.Role.Valid() || v.MediaType == "" {
		return errors.New("cache blob role and media type are required")
	}
	if err := v.LogicalPath.Validate(); err != nil {
		return err
	}
	if v.SourceOccurrenceID != "" {
		if err := v.SourceOccurrenceID.Validate(); err != nil {
			return err
		}
	}
	return v.Provenance.Validate()
}

// CacheSource identifies the producing occurrence and logical call. The run
// is explicit because source IDs are otherwise easy to cross-wire in SQL.
type CacheSource struct {
	RunID        RunID                `json:"run_id"`
	CallRecordID CallRecordID         `json:"call_record_id"`
	OccurrenceID ArtifactOccurrenceID `json:"occurrence_id"`
	Digest       Digest               `json:"digest"`
}

func (v CacheSource) Validate() error {
	for name, err := range map[string]error{
		"source run id": v.RunID.Validate(), "source call record id": v.CallRecordID.Validate(),
		"source occurrence id": v.OccurrenceID.Validate(), "source digest": v.Digest.Validate(),
	} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

type CacheEntry struct {
	Key           CacheKey        `json:"key"`
	Kind          string          `json:"kind"`
	SchemaVersion SchemaVersion   `json:"schema_version"`
	PolicyDigest  Digest          `json:"policy_digest"`
	InputDigest   Digest          `json:"input_digest"`
	State         CacheEntryState `json:"state"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	Source        CacheSource     `json:"source"`
	Sources       []CacheSource   `json:"sources,omitempty"`
	Blobs         []CacheBlob     `json:"blobs"`
}

func (v CacheEntry) Validate() error {
	if err := v.Key.Validate(); err != nil {
		return err
	}
	if v.Kind != v.Key.Kind {
		return errors.New("cache entry kind does not match cache key")
	}
	if err := validateCacheKind(v.Kind); err != nil {
		return err
	}
	if err := v.SchemaVersion.Validate(); err != nil {
		return err
	}
	for name, err := range map[string]error{"policy digest": v.PolicyDigest.Validate(), "input digest": v.InputDigest.Validate()} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(v.Sources) > 0 {
		for _, source := range v.Sources {
			if err := source.Validate(); err != nil {
				return err
			}
		}
	} else if err := v.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if !v.State.Valid() || len(v.Blobs) == 0 {
		return errors.New("cache entry state or blob set is invalid")
	}
	if err := validateUTCTime("cache created at", v.CreatedAt); err != nil {
		return err
	}
	if v.ExpiresAt != nil {
		if err := validateUTCTime("cache expiry", *v.ExpiresAt); err != nil {
			return err
		}
		if v.ExpiresAt.Before(v.CreatedAt) {
			return errors.New("cache expiry precedes creation")
		}
	}
	seen := make(map[string]struct{}, len(v.Blobs))
	for _, item := range v.Blobs {
		if err := item.Validate(); err != nil {
			return err
		}
		identity := fmt.Sprintf("%s/%d/%s", item.Blob.Digest, item.Blob.Size, item.Role)
		if _, ok := seen[identity]; ok {
			return errors.New("cache entry contains duplicate blob")
		}
		seen[identity] = struct{}{}
	}
	return nil
}

type CacheLookup struct {
	RunID         RunID         `json:"run_id"`
	Key           CacheKey      `json:"key"`
	SchemaVersion SchemaVersion `json:"schema_version"`
	PolicyDigest  Digest        `json:"policy_digest"`
	InputDigest   Digest        `json:"input_digest"`
	At            time.Time     `json:"at"`
}

func (v CacheLookup) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.Key.Validate(); err != nil {
		return err
	}
	if err := v.SchemaVersion.Validate(); err != nil {
		return err
	}
	if err := v.PolicyDigest.Validate(); err != nil {
		return err
	}
	if err := v.InputDigest.Validate(); err != nil {
		return err
	}
	return validateUTCTime("cache lookup at", v.At)
}

type CacheCandidate struct {
	Entry               CacheEntry             `json:"entry"`
	SourceCallRecordID  CallRecordID           `json:"source_call_record_id"`
	SourceOccurrenceIDs []ArtifactOccurrenceID `json:"source_occurrence_ids"`
}

type GCItem struct {
	Ref                   BlobRef `json:"ref"`
	CanonicalRelativePath string  `json:"canonical_relative_path"`
	PublicationGeneration int64   `json:"publication_generation"`
}

type GCCommit string

const (
	GCCommitRemoved GCCommit = "REMOVED"
	GCCommitRepair  GCCommit = "REPAIR"
)

func (v GCCommit) Valid() bool { return v == GCCommitRemoved || v == GCCommitRepair }

type GCReport struct {
	Planned  int `json:"planned"`
	Moved    int `json:"moved"`
	Removed  int `json:"removed"`
	Repaired int `json:"repaired"`
}

func (v CacheCandidate) Validate() error {
	if err := v.Entry.Validate(); err != nil {
		return err
	}
	if err := v.SourceCallRecordID.Validate(); err != nil {
		return err
	}
	if len(v.SourceOccurrenceIDs) != len(v.Entry.Blobs) {
		return errors.New("cache candidate source/blob cardinality differs")
	}
	sources := v.Entry.Sources
	if len(sources) == 0 {
		sources = []CacheSource{v.Entry.Source}
	}
	if len(sources) != len(v.Entry.Blobs) {
		return errors.New("cache candidate entry source/blob cardinality differs")
	}
	for _, id := range v.SourceOccurrenceIDs {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	for index, item := range v.Entry.Blobs {
		if item.SourceOccurrenceID != "" && item.SourceOccurrenceID != sources[index].OccurrenceID {
			return errors.New("cache candidate blob/source order differs")
		}
		if sources[index].OccurrenceID != v.SourceOccurrenceIDs[index] || sources[index].Digest != item.Blob.Digest {
			return errors.New("cache candidate source provenance differs")
		}
	}
	if v.SourceCallRecordID != sources[0].CallRecordID {
		return errors.New("cache candidate source call differs")
	}
	return nil
}

func (v CacheCandidate) ValidateFor(lookup CacheLookup) error {
	if err := lookup.Validate(); err != nil {
		return err
	}
	if err := v.Validate(); err != nil {
		return err
	}
	if v.Entry.Key != lookup.Key || v.Entry.Kind != lookup.Key.Kind || v.Entry.SchemaVersion != lookup.SchemaVersion || v.Entry.PolicyDigest != lookup.PolicyDigest || v.Entry.InputDigest != lookup.InputDigest {
		return errors.New("cache candidate does not match lookup identity")
	}
	if v.Entry.State != CacheEntryValid {
		return errors.New("cache candidate is invalidated")
	}
	if v.Entry.ExpiresAt != nil && !lookup.At.Before(*v.Entry.ExpiresAt) {
		return errors.New("cache candidate is expired")
	}
	return nil
}

type CommitCacheReuse struct {
	RunID               RunID                  `json:"run_id"`
	StageName           StageName              `json:"stage_name"`
	AttemptID           AttemptID              `json:"attempt_id"`
	CurrentCallRecordID CallRecordID           `json:"current_call_record_id"`
	CacheReuseRecordID  CacheReuseRecordID     `json:"cache_reuse_record_id"`
	CacheReuseRecordIDs []CacheReuseRecordID   `json:"cache_reuse_record_ids,omitempty"`
	Key                 CacheKey               `json:"key"`
	SourceCallRecordID  CallRecordID           `json:"source_call_record_id"`
	SourceOccurrenceIDs []ArtifactOccurrenceID `json:"source_occurrence_ids"`
	At                  time.Time              `json:"at"`
}

type CacheReuseRequest struct {
	Lookup     CacheLookup       `json:"lookup"`
	OpenCall   OpenCallRequest   `json:"open_call"`
	FinishCall FinishCallRequest `json:"finish_call"`
	Reuse      CommitCacheReuse  `json:"reuse"`
}

func (v CommitCacheReuse) Validate() error {
	for name, err := range map[string]error{"run id": v.RunID.Validate(), "attempt id": v.AttemptID.Validate(), "call record id": v.CurrentCallRecordID.Validate(), "source call record id": v.SourceCallRecordID.Validate()} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(v.CacheReuseRecordIDs) == 0 {
		if err := v.CacheReuseRecordID.Validate(); err != nil {
			return fmt.Errorf("cache reuse record id: %w", err)
		}
	} else {
		for index, id := range v.CacheReuseRecordIDs {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("cache reuse record id %d: %w", index, err)
			}
		}
		if v.CacheReuseRecordID != "" {
			return errors.New("cache reuse record id fields are ambiguous")
		}
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if err := v.Key.Validate(); err != nil {
		return err
	}
	if len(v.SourceOccurrenceIDs) == 0 {
		return errors.New("cache reuse requires source occurrences")
	}
	return validateUTCTime("cache reuse at", v.At)
}

func validateCacheKind(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("invalid cache kind %q", value)
	}
	return nil
}
