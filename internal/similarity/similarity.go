// Package similarity contains the provider-neutral boundary for similarity
// checks.  It deliberately models only the package-safe candidate projection;
// private generation context has no type in this package and therefore cannot
// accidentally be included in a request.
package similarity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cpgen/internal/domain"
	"golang.org/x/text/unicode/norm"
)

const (
	ProjectionSchemaVersion domain.SchemaVersion = "cpgen.similarity-projection/v1"
	RequestSchemaVersion    domain.SchemaVersion = "cpgen.similarity-request/v1"
	EvidenceSchemaVersion   domain.SchemaVersion = "cpgen.similarity-evidence/v1"
	PolicySchemaVersion     domain.SchemaVersion = "cpgen.similarity-policy/v1"
	DecisionSchemaVersion   domain.SchemaVersion = "cpgen.similarity-decision/v1"
	ProtocolVersion                              = "cpgen-similarity-http/v1"
)

// PackageSafeProjection is the complete set of candidate data eligible for a
// similarity provider.  In particular it has no solution, tests, prompts,
// local paths, secrets, or generation provenance fields.
type PackageSafeProjection struct {
	SchemaVersion       domain.SchemaVersion `json:"schema_version"`
	NormalizedTitle     string               `json:"normalized_title"`
	NormalizedStatement string               `json:"normalized_statement"`
	NormalizedTags      []string             `json:"normalized_tags"`
	Language            string               `json:"language"`
}

// Projection is a short alias useful at call sites that already use the
// word projection for package-safe values.
type Projection = PackageSafeProjection

func NewPackageSafeProjection(title, statement string, tags []string, language string) (PackageSafeProjection, error) {
	p := PackageSafeProjection{
		SchemaVersion:       ProjectionSchemaVersion,
		NormalizedTitle:     cleanSingleLine(title),
		NormalizedStatement: cleanStatement(statement),
		NormalizedTags:      cleanTags(tags),
		Language:            cleanLanguage(language),
	}
	if err := p.Validate(); err != nil {
		return PackageSafeProjection{}, err
	}
	return p, nil
}

func (p PackageSafeProjection) Validate() error {
	if p.SchemaVersion != ProjectionSchemaVersion {
		return fmt.Errorf("similarity projection schema version must be %q", ProjectionSchemaVersion)
	}
	if err := validateText("projection title", p.NormalizedTitle, 1024, true, false); err != nil {
		return err
	}
	if err := validateText("projection statement", p.NormalizedStatement, 65536, true, true); err != nil {
		return err
	}
	if len(p.NormalizedTags) > 64 {
		return errors.New("projection has too many tags")
	}
	last := ""
	for i, tag := range p.NormalizedTags {
		if err := validateText(fmt.Sprintf("projection tag %d", i), tag, 128, true, false); err != nil {
			return err
		}
		if tag <= last {
			return errors.New("projection tags must be sorted and unique")
		}
		last = tag
	}
	if err := validateText("projection language", p.Language, 32, true, false); err != nil {
		return err
	}
	if cleanSingleLine(p.NormalizedTitle) != p.NormalizedTitle || cleanStatement(p.NormalizedStatement) != p.NormalizedStatement || cleanLanguage(p.Language) != p.Language || !slices.Equal(cleanTags(p.NormalizedTags), p.NormalizedTags) {
		return errors.New("projection fields are not normalized")
	}
	return nil
}

func (p PackageSafeProjection) CanonicalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

func (p PackageSafeProjection) Digest() (domain.Digest, error) {
	raw, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

// Request is the immutable provider-neutral similarity input.  The repeated
// normalized fields bind the digest to exactly what is sent over the wire.
type Request struct {
	SchemaVersion             domain.SchemaVersion `json:"schema_version"`
	CandidateProjectionDigest domain.Digest        `json:"candidate_projection_digest"`
	NormalizedTitle           string               `json:"normalized_title"`
	NormalizedStatement       string               `json:"normalized_statement"`
	NormalizedTags            []string             `json:"normalized_tags"`
	Language                  string               `json:"language"`
	PolicyRef                 string               `json:"policy_ref"`
	PolicyDigest              domain.Digest        `json:"policy_digest"`
	LogicalIdempotencyKey     string               `json:"logical_idempotency_key"`
}

// SimilarityRequest is the descriptive name used by callers that prefer the
// design document's terminology.
type SimilarityRequest = Request

func NewRequest(projection PackageSafeProjection, policyRef string, policyDigest domain.Digest, logicalID string) (Request, error) {
	digest, err := projection.Digest()
	if err != nil {
		return Request{}, err
	}
	r := Request{
		SchemaVersion:             RequestSchemaVersion,
		CandidateProjectionDigest: digest,
		NormalizedTitle:           projection.NormalizedTitle,
		NormalizedStatement:       projection.NormalizedStatement,
		NormalizedTags:            copyStrings(projection.NormalizedTags),
		Language:                  projection.Language,
		PolicyRef:                 cleanSingleLine(policyRef),
		PolicyDigest:              policyDigest,
		LogicalIdempotencyKey:     strings.TrimSpace(logicalID),
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

func (r Request) Projection() PackageSafeProjection {
	return PackageSafeProjection{
		SchemaVersion:       ProjectionSchemaVersion,
		NormalizedTitle:     r.NormalizedTitle,
		NormalizedStatement: r.NormalizedStatement,
		NormalizedTags:      copyStrings(r.NormalizedTags),
		Language:            r.Language,
	}
}

func (r Request) Validate() error {
	if r.SchemaVersion != RequestSchemaVersion {
		return fmt.Errorf("similarity request schema version must be %q", RequestSchemaVersion)
	}
	if err := r.CandidateProjectionDigest.Validate(); err != nil {
		return fmt.Errorf("candidate projection digest: %w", err)
	}
	if err := r.Projection().Validate(); err != nil {
		return err
	}
	computed, err := r.Projection().Digest()
	if err != nil {
		return err
	}
	if computed != r.CandidateProjectionDigest {
		return errors.New("candidate projection digest does not match request fields")
	}
	if err := validateText("policy ref", r.PolicyRef, 256, true, false); err != nil {
		return err
	}
	if err := r.PolicyDigest.Validate(); err != nil {
		return fmt.Errorf("policy digest: %w", err)
	}
	if err := validateText("logical idempotency key", r.LogicalIdempotencyKey, 256, true, false); err != nil {
		return err
	}
	return nil
}

func (r Request) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func (r Request) Digest() (domain.Digest, error) {
	raw, err := r.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

// Hit is a package-safe, normalized result.  A title is represented by its
// digest so a provider cannot smuggle arbitrary third-party text into a
// package.  NewHit accepts the provider title only long enough to hash it.
type Hit struct {
	Source         string        `json:"source"`
	ExternalID     string        `json:"external_id"`
	Score          float64       `json:"score"`
	CanonicalURL   string        `json:"canonical_url,omitempty"`
	TitleDigest    domain.Digest `json:"title_digest,omitempty"`
	EvidenceDigest domain.Digest `json:"evidence_digest"`
}

func NewHit(source, externalID, title string, score float64, rawURL string) (Hit, error) {
	title = cleanSingleLine(title)
	h := Hit{Source: cleanSingleLine(source), ExternalID: cleanSingleLine(externalID), Score: score}
	if title != "" {
		h.TitleDigest = domain.SumBytes([]byte(title))
	}
	cleanURL, err := sanitizeURL(rawURL)
	if err != nil {
		return Hit{}, err
	}
	h.CanonicalURL = cleanURL
	h.EvidenceDigest = hitDigest(h)
	if err := h.Validate(); err != nil {
		return Hit{}, err
	}
	return h, nil
}

func (h Hit) Validate() error {
	if err := validateText("hit source", h.Source, 256, true, false); err != nil {
		return err
	}
	if err := validateText("hit external id", h.ExternalID, 512, true, false); err != nil {
		return err
	}
	if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) || h.Score < 0 || h.Score > 1 {
		return errors.New("hit score must be a finite number from 0 to 1")
	}
	if h.CanonicalURL != "" {
		if _, err := sanitizeURL(h.CanonicalURL); err != nil {
			return fmt.Errorf("hit canonical URL: %w", err)
		}
	}
	if h.TitleDigest != "" {
		if err := h.TitleDigest.Validate(); err != nil {
			return fmt.Errorf("hit title digest: %w", err)
		}
	}
	if err := h.EvidenceDigest.Validate(); err != nil {
		return fmt.Errorf("hit evidence digest: %w", err)
	}
	if hitDigest(h) != h.EvidenceDigest {
		return errors.New("hit evidence digest does not match hit")
	}
	return nil
}

// SortHits orders the strongest score first and uses all stable identity
// fields as tie breakers.  It copies the slice and never changes caller data.
func SortHits(hits []Hit) ([]Hit, error) {
	result := append([]Hit(nil), hits...)
	for i := range result {
		if err := result[i].Validate(); err != nil {
			return nil, fmt.Errorf("hit %d: %w", i, err)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		if left.ExternalID != right.ExternalID {
			return left.ExternalID < right.ExternalID
		}
		if left.CanonicalURL != right.CanonicalURL {
			return left.CanonicalURL < right.CanonicalURL
		}
		if left.TitleDigest != right.TitleDigest {
			return left.TitleDigest < right.TitleDigest
		}
		return left.EvidenceDigest < right.EvidenceDigest
	})
	return result, nil
}

type ScoreSummary struct {
	HitCount  int     `json:"hit_count"`
	MaxScore  float64 `json:"max_score"`
	MeanScore float64 `json:"mean_score"`
}

func summarize(hits []Hit) ScoreSummary {
	var result ScoreSummary
	result.HitCount = len(hits)
	for _, hit := range hits {
		if hit.Score > result.MaxScore {
			result.MaxScore = hit.Score
		}
		result.MeanScore += hit.Score
	}
	if len(hits) > 0 {
		result.MeanScore /= float64(len(hits))
	}
	return result
}

func (s ScoreSummary) Validate() error {
	if s.HitCount < 0 || math.IsNaN(s.MaxScore) || math.IsInf(s.MaxScore, 0) || math.IsNaN(s.MeanScore) || math.IsInf(s.MeanScore, 0) || s.MaxScore < 0 || s.MaxScore > 1 || s.MeanScore < 0 || s.MeanScore > 1 {
		return errors.New("invalid similarity score summary")
	}
	return nil
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicroUSD int64 `json:"cost_micro_usd,omitempty"`
}

const (
	UsageProviderVerified       = "provider_verified"
	UsageConservativeUpperBound = "conservative_upper_bound_v1"
)

func (u Usage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CostMicroUSD < 0 {
		return errors.New("similarity usage must be non-negative")
	}
	return nil
}

type CacheProvenanceKind string

const (
	CacheLive    CacheProvenanceKind = "LIVE"
	CacheHit     CacheProvenanceKind = "CACHE_HIT"
	CacheInvalid CacheProvenanceKind = "INVALID"
)

type CacheProvenance struct {
	Kind                 CacheProvenanceKind           `json:"kind"`
	CacheKeyDigest       domain.Digest                 `json:"cache_key_digest,omitempty"`
	SourceEvidenceDigest domain.Digest                 `json:"source_evidence_digest,omitempty"`
	SourceCallRecordID   domain.CallRecordID           `json:"source_call_record_id,omitempty"`
	SourceOccurrenceIDs  []domain.ArtifactOccurrenceID `json:"source_occurrence_ids,omitempty"`
	CurrentCallRecordID  domain.CallRecordID           `json:"current_call_record_id,omitempty"`
}

func (p CacheProvenance) Validate() error {
	switch p.Kind {
	case CacheLive:
		if p.CacheKeyDigest != "" || p.SourceEvidenceDigest != "" || p.SourceCallRecordID != "" || p.CurrentCallRecordID != "" || len(p.SourceOccurrenceIDs) != 0 {
			return errors.New("LIVE cache provenance cannot contain source fields")
		}
	case CacheHit:
		if err := p.CacheKeyDigest.Validate(); err != nil {
			return fmt.Errorf("cache key digest: %w", err)
		}
		if err := p.SourceEvidenceDigest.Validate(); err != nil {
			return fmt.Errorf("source evidence digest: %w", err)
		}
		if err := p.SourceCallRecordID.Validate(); err != nil {
			return fmt.Errorf("source call record id: %w", err)
		}
		if err := p.CurrentCallRecordID.Validate(); err != nil {
			return fmt.Errorf("current call record id: %w", err)
		}
		if len(p.SourceOccurrenceIDs) == 0 {
			return errors.New("cache hit requires source occurrences")
		}
		for i, id := range p.SourceOccurrenceIDs {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("source occurrence %d: %w", i, err)
			}
		}
	case CacheInvalid:
		return errors.New("invalid cache provenance cannot be used as evidence")
	default:
		return fmt.Errorf("invalid cache provenance kind %q", p.Kind)
	}
	return nil
}

// Evidence is an immutable, auditable result of a similarity call.  The
// digest covers the normalized hits, provider identity, policy, usage, call
// trace, observation time, and cache provenance.
type Evidence struct {
	SchemaVersion    domain.SchemaVersion `json:"schema_version"`
	RequestDigest    domain.Digest        `json:"request_digest"`
	ProviderIdentity string               `json:"provider_identity"`
	Hits             []Hit                `json:"hits"`
	ScoreSummary     ScoreSummary         `json:"score_summary"`
	ObservedAt       time.Time            `json:"observed_at"`
	PolicyDigest     domain.Digest        `json:"policy_digest"`
	Usage            Usage                `json:"usage"`
	UsageSource      string               `json:"usage_source"`
	ModelVersion     string               `json:"model_version,omitempty"`
	IndexVersion     string               `json:"index_version,omitempty"`
	Cache            CacheProvenance      `json:"cache_provenance"`
	CallTrace        domain.CallTrace     `json:"call_trace"`
	EvidenceDigest   domain.Digest        `json:"evidence_digest"`
}

func NewEvidence(request Request, providerIdentity string, hits []Hit, observedAt time.Time, usage Usage, cache CacheProvenance, trace domain.CallTrace) (Evidence, error) {
	return newEvidence(request, providerIdentity, hits, observedAt, usage, UsageProviderVerified, "", "", cache, trace)
}

// NewEvidenceWithMetadata retains provider usage/index metadata in the
// auditable evidence without exposing provider-specific wire fields.
func NewEvidenceWithMetadata(request Request, providerIdentity string, hits []Hit, observedAt time.Time, usage Usage, usageSource, modelVersion, indexVersion string, cache CacheProvenance, trace domain.CallTrace) (Evidence, error) {
	return newEvidence(request, providerIdentity, hits, observedAt, usage, usageSource, modelVersion, indexVersion, cache, trace)
}

func newEvidence(request Request, providerIdentity string, hits []Hit, observedAt time.Time, usage Usage, usageSource, modelVersion, indexVersion string, cache CacheProvenance, trace domain.CallTrace) (Evidence, error) {
	if err := request.Validate(); err != nil {
		return Evidence{}, err
	}
	sortedHits, err := SortHits(hits)
	if err != nil {
		return Evidence{}, err
	}
	if trace.LogicalOperationID == "" {
		reqDigest, _ := request.Digest()
		trace = domain.CallTrace{LogicalOperationID: "similarity:" + strings.TrimPrefix(string(reqDigest), "sha256:"), DispatchKind: domain.DispatchNone}
	}
	e := Evidence{
		SchemaVersion:    EvidenceSchemaVersion,
		RequestDigest:    mustDigest(request),
		ProviderIdentity: cleanSingleLine(providerIdentity),
		Hits:             sortedHits,
		ScoreSummary:     summarize(sortedHits),
		ObservedAt:       observedAt.UTC(),
		PolicyDigest:     request.PolicyDigest,
		Usage:            usage,
		UsageSource:      usageSource,
		ModelVersion:     cleanSingleLine(modelVersion),
		IndexVersion:     cleanSingleLine(indexVersion),
		Cache:            cache,
		CallTrace:        trace,
	}
	if err := e.validateWithoutDigest(); err != nil {
		return Evidence{}, err
	}
	e.EvidenceDigest = digestEvidence(e)
	return e, nil
}

func (e Evidence) Validate() error {
	if err := e.validateWithoutDigest(); err != nil {
		return err
	}
	if err := e.EvidenceDigest.Validate(); err != nil {
		return fmt.Errorf("evidence digest: %w", err)
	}
	if digestEvidence(e) != e.EvidenceDigest {
		return errors.New("evidence digest does not match evidence")
	}
	return nil
}

func (e Evidence) validateWithoutDigest() error {
	if e.SchemaVersion != EvidenceSchemaVersion {
		return fmt.Errorf("similarity evidence schema version must be %q", EvidenceSchemaVersion)
	}
	if err := e.RequestDigest.Validate(); err != nil {
		return fmt.Errorf("request digest: %w", err)
	}
	if err := validateText("provider identity", e.ProviderIdentity, 512, true, false); err != nil {
		return err
	}
	sorted, err := SortHits(e.Hits)
	if err != nil {
		return err
	}
	if !slices.Equal(sorted, e.Hits) {
		return errors.New("evidence hits are not deterministically sorted")
	}
	wantSummary := summarize(e.Hits)
	if e.ScoreSummary != wantSummary {
		return errors.New("evidence score summary does not match hits")
	}
	if err := e.ScoreSummary.Validate(); err != nil {
		return err
	}
	if e.ObservedAt.IsZero() || !e.ObservedAt.Equal(e.ObservedAt.UTC()) {
		return errors.New("evidence observation time must be a non-zero UTC time")
	}
	if err := e.PolicyDigest.Validate(); err != nil {
		return fmt.Errorf("evidence policy digest: %w", err)
	}
	if err := e.Usage.Validate(); err != nil {
		return err
	}
	if e.UsageSource != UsageProviderVerified && e.UsageSource != UsageConservativeUpperBound {
		return fmt.Errorf("invalid usage source %q", e.UsageSource)
	}
	if err := validateText("model version", e.ModelVersion, 256, false, false); err != nil {
		return err
	}
	if err := validateText("index version", e.IndexVersion, 256, false, false); err != nil {
		return err
	}
	if err := e.Cache.Validate(); err != nil {
		return err
	}
	return e.CallTrace.Validate()
}

func (e Evidence) CanonicalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return canonicalEvidenceJSON(e)
}

func (e Evidence) Digest() (domain.Digest, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	return e.EvidenceDigest, nil
}

type evidenceCanonical struct {
	SchemaVersion    domain.SchemaVersion `json:"schema_version"`
	RequestDigest    domain.Digest        `json:"request_digest"`
	ProviderIdentity string               `json:"provider_identity"`
	Hits             []Hit                `json:"hits"`
	ScoreSummary     ScoreSummary         `json:"score_summary"`
	ObservedAt       time.Time            `json:"observed_at"`
	PolicyDigest     domain.Digest        `json:"policy_digest"`
	Usage            Usage                `json:"usage"`
	UsageSource      string               `json:"usage_source"`
	ModelVersion     string               `json:"model_version,omitempty"`
	IndexVersion     string               `json:"index_version,omitempty"`
	Cache            CacheProvenance      `json:"cache_provenance"`
	CallTrace        domain.CallTrace     `json:"call_trace"`
}

func canonicalEvidenceJSON(e Evidence) ([]byte, error) {
	return json.Marshal(evidenceCanonical{e.SchemaVersion, e.RequestDigest, e.ProviderIdentity, e.Hits, e.ScoreSummary, e.ObservedAt.UTC(), e.PolicyDigest, e.Usage, e.UsageSource, e.ModelVersion, e.IndexVersion, e.Cache, e.CallTrace})
}

func digestEvidence(e Evidence) domain.Digest {
	raw, _ := canonicalEvidenceJSON(e)
	return domain.SumBytes(raw)
}

func hitDigest(h Hit) domain.Digest {
	wire := struct {
		Source       string        `json:"source"`
		ExternalID   string        `json:"external_id"`
		Score        float64       `json:"score"`
		CanonicalURL string        `json:"canonical_url,omitempty"`
		TitleDigest  domain.Digest `json:"title_digest,omitempty"`
	}{h.Source, h.ExternalID, h.Score, h.CanonicalURL, h.TitleDigest}
	raw, _ := json.Marshal(wire)
	return domain.SumBytes(raw)
}

// DecisionKind is deliberately not coupled to workflow state.  The caller
// can map these four typed results to a stage transition or review command.
type DecisionKind string

const (
	DecisionAccept      DecisionKind = "ACCEPT"
	DecisionReject      DecisionKind = "REJECT"
	DecisionNeedsReview DecisionKind = "NEEDS_REVIEW"
	DecisionBlocked     DecisionKind = "BLOCKED"
)

func (k DecisionKind) Valid() bool {
	return k == DecisionAccept || k == DecisionReject || k == DecisionNeedsReview || k == DecisionBlocked
}

type ExplanationCode string

const (
	ExplanationBelowAcceptance  ExplanationCode = "below_acceptance_threshold"
	ExplanationAtRejection      ExplanationCode = "at_or_above_rejection_threshold"
	ExplanationReviewBand       ExplanationCode = "within_review_band"
	ExplanationDesignatedSource ExplanationCode = "designated_review_source"
	ExplanationInsufficient     ExplanationCode = "insufficient_evidence"
	ExplanationPolicyMismatch   ExplanationCode = "policy_mismatch"
	ExplanationInvalidEvidence  ExplanationCode = "invalid_evidence"
)

func (c ExplanationCode) Valid() bool {
	switch c {
	case ExplanationBelowAcceptance, ExplanationAtRejection, ExplanationReviewBand, ExplanationDesignatedSource, ExplanationInsufficient, ExplanationPolicyMismatch, ExplanationInvalidEvidence:
		return true
	default:
		return false
	}
}

type Decision struct {
	SchemaVersion       domain.SchemaVersion `json:"schema_version"`
	Kind                DecisionKind         `json:"kind"`
	RuleID              string               `json:"rule_id"`
	EvidenceDigest      domain.Digest        `json:"evidence_digest,omitempty"`
	PolicyDigest        domain.Digest        `json:"policy_digest"`
	ExplanationCode     ExplanationCode      `json:"explanation_code"`
	ObservedScore       float64              `json:"observed_score,omitempty"`
	AcceptanceThreshold float64              `json:"acceptance_threshold,omitempty"`
	RejectionThreshold  float64              `json:"rejection_threshold,omitempty"`
}

func (d Decision) Validate() error {
	if d.SchemaVersion != DecisionSchemaVersion || !d.Kind.Valid() {
		return errors.New("invalid similarity decision schema or kind")
	}
	if err := validateText("decision rule id", d.RuleID, 256, true, false); err != nil {
		return err
	}
	if err := d.PolicyDigest.Validate(); err != nil {
		return fmt.Errorf("decision policy digest: %w", err)
	}
	if d.Kind != DecisionBlocked {
		if err := d.EvidenceDigest.Validate(); err != nil {
			return fmt.Errorf("decision evidence digest: %w", err)
		}
	}
	if !d.ExplanationCode.Valid() {
		return fmt.Errorf("invalid decision explanation code %q", d.ExplanationCode)
	}
	if math.IsNaN(d.ObservedScore) || math.IsInf(d.ObservedScore, 0) || d.ObservedScore < 0 || d.ObservedScore > 1 {
		return errors.New("decision observed score is invalid")
	}
	return nil
}

type DecisionPolicy struct {
	SchemaVersion       domain.SchemaVersion `json:"schema_version"`
	PolicyRef           string               `json:"policy_ref"`
	PolicyDigest        domain.Digest        `json:"policy_digest"`
	AcceptanceThreshold float64              `json:"acceptance_threshold"`
	RejectionThreshold  float64              `json:"rejection_threshold"`
	ReviewBandLower     float64              `json:"review_band_lower"`
	ReviewBandUpper     float64              `json:"review_band_upper"`
	MinimumHits         int                  `json:"minimum_hits"`
	ReviewSources       []string             `json:"review_sources,omitempty"`
}

// Policy is an alias retained for concise policy construction.
type Policy = DecisionPolicy

func NewPolicy(ref string, acceptance, rejection, reviewLower, reviewUpper float64, minimumHits int, reviewSources []string) (DecisionPolicy, error) {
	if reviewLower == 0 && reviewUpper == 0 {
		reviewLower, reviewUpper = acceptance, rejection
	}
	p := DecisionPolicy{SchemaVersion: PolicySchemaVersion, PolicyRef: cleanSingleLine(ref), AcceptanceThreshold: acceptance, RejectionThreshold: rejection, ReviewBandLower: reviewLower, ReviewBandUpper: reviewUpper, MinimumHits: minimumHits, ReviewSources: cleanTags(reviewSources)}
	if err := p.validateWithoutDigest(); err != nil {
		return DecisionPolicy{}, err
	}
	p.PolicyDigest = digestPolicy(p)
	return p, nil
}

func (p DecisionPolicy) Validate() error {
	if err := p.validateWithoutDigest(); err != nil {
		return err
	}
	if err := p.PolicyDigest.Validate(); err != nil {
		return fmt.Errorf("policy digest: %w", err)
	}
	if digestPolicy(p) != p.PolicyDigest {
		return errors.New("policy digest does not match policy")
	}
	return nil
}

func (p DecisionPolicy) validateWithoutDigest() error {
	if p.SchemaVersion != PolicySchemaVersion {
		return fmt.Errorf("similarity policy schema version must be %q", PolicySchemaVersion)
	}
	if err := validateText("policy ref", p.PolicyRef, 256, true, false); err != nil {
		return err
	}
	for name, value := range map[string]float64{"acceptance threshold": p.AcceptanceThreshold, "rejection threshold": p.RejectionThreshold, "review band lower": p.ReviewBandLower, "review band upper": p.ReviewBandUpper} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fmt.Errorf("%s is outside 0..1", name)
		}
	}
	if !(p.AcceptanceThreshold <= p.ReviewBandLower && p.ReviewBandLower <= p.ReviewBandUpper && p.ReviewBandUpper <= p.RejectionThreshold) {
		return errors.New("review band and thresholds are not ordered")
	}
	if p.MinimumHits <= 0 || p.MinimumHits > 10000 {
		return errors.New("minimum hits must be positive and bounded")
	}
	last := ""
	for i, source := range p.ReviewSources {
		if err := validateText(fmt.Sprintf("review source %d", i), source, 256, true, false); err != nil {
			return err
		}
		if source <= last {
			return errors.New("review sources must be sorted and unique")
		}
		last = source
	}
	return nil
}

func (p DecisionPolicy) CanonicalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return canonicalPolicyJSON(p), nil
}

func (p DecisionPolicy) Digest() (domain.Digest, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	return p.PolicyDigest, nil
}

func canonicalPolicyJSON(p DecisionPolicy) []byte {
	wire := struct {
		SchemaVersion       domain.SchemaVersion `json:"schema_version"`
		PolicyRef           string               `json:"policy_ref"`
		AcceptanceThreshold float64              `json:"acceptance_threshold"`
		RejectionThreshold  float64              `json:"rejection_threshold"`
		ReviewBandLower     float64              `json:"review_band_lower"`
		ReviewBandUpper     float64              `json:"review_band_upper"`
		MinimumHits         int                  `json:"minimum_hits"`
		ReviewSources       []string             `json:"review_sources,omitempty"`
	}{p.SchemaVersion, p.PolicyRef, p.AcceptanceThreshold, p.RejectionThreshold, p.ReviewBandLower, p.ReviewBandUpper, p.MinimumHits, p.ReviewSources}
	raw, _ := json.Marshal(wire)
	return raw
}

func digestPolicy(p DecisionPolicy) domain.Digest { return domain.SumBytes(canonicalPolicyJSON(p)) }

// Evaluate applies the policy in a fail-closed order.  At the exact
// acceptance boundary the result is review, while at the exact rejection
// boundary it is reject.  A designated source always routes to review.
func Evaluate(policy DecisionPolicy, evidence Evidence) Decision {
	policyDigest := policy.PolicyDigest
	if policyDigest == "" {
		if policy.validateWithoutDigest() == nil {
			policyDigest = digestPolicy(policy)
		}
	}
	decision := Decision{SchemaVersion: DecisionSchemaVersion, Kind: DecisionBlocked, RuleID: "similarity-policy-v1", PolicyDigest: policyDigest, ExplanationCode: ExplanationInvalidEvidence}
	if err := policy.validateWithoutDigest(); err != nil || policyDigest == "" {
		decision.ExplanationCode = ExplanationInvalidEvidence
		return decision
	}
	if policy.PolicyDigest != "" && policy.PolicyDigest != policyDigest {
		decision.ExplanationCode = ExplanationInvalidEvidence
		return decision
	}
	if err := evidence.Validate(); err != nil {
		decision.ExplanationCode = ExplanationInvalidEvidence
		return decision
	}
	decision.EvidenceDigest = evidence.EvidenceDigest
	decision.ObservedScore = evidence.ScoreSummary.MaxScore
	decision.AcceptanceThreshold = policy.AcceptanceThreshold
	decision.RejectionThreshold = policy.RejectionThreshold
	if evidence.PolicyDigest != policyDigest {
		decision.ExplanationCode = ExplanationPolicyMismatch
		return decision
	}
	if evidence.ScoreSummary.HitCount < policy.MinimumHits {
		decision.ExplanationCode = ExplanationInsufficient
		return decision
	}
	for _, hit := range evidence.Hits {
		if slices.Contains(policy.ReviewSources, hit.Source) {
			decision.Kind = DecisionNeedsReview
			decision.ExplanationCode = ExplanationDesignatedSource
			return decision
		}
	}
	score := evidence.ScoreSummary.MaxScore
	switch {
	case score >= policy.RejectionThreshold:
		decision.Kind, decision.ExplanationCode = DecisionReject, ExplanationAtRejection
	case score < policy.AcceptanceThreshold:
		decision.Kind, decision.ExplanationCode = DecisionAccept, ExplanationBelowAcceptance
	case score >= policy.ReviewBandLower && score <= policy.ReviewBandUpper:
		decision.Kind, decision.ExplanationCode = DecisionNeedsReview, ExplanationReviewBand
	default:
		// A gap outside the configured band is ambiguous; fail closed.
		decision.Kind, decision.ExplanationCode = DecisionNeedsReview, ExplanationReviewBand
	}
	return decision
}

func (d Decision) IsAccept() bool      { return d.Kind == DecisionAccept }
func (d Decision) IsReject() bool      { return d.Kind == DecisionReject }
func (d Decision) IsNeedsReview() bool { return d.Kind == DecisionNeedsReview }
func (d Decision) IsBlocked() bool     { return d.Kind == DecisionBlocked }

func validateText(name, value string, maxBytes int, required, multiline bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid or too long", name)
	}
	for _, r := range value {
		if r == '\x00' || r == '\r' || (!multiline && (r == '\n' || r == '\t')) || (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return fmt.Errorf("%s contains disallowed control characters", name)
		}
	}
	return nil
}

func cleanSingleLine(value string) string {
	value = norm.NFC.String(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), " ")
}

func cleanStatement(value string) string {
	value = norm.NFC.String(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")))
	return value
}

func cleanLanguage(value string) string { return strings.ToLower(cleanSingleLine(value)) }

func cleanTags(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = cleanSingleLine(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func sanitizeURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" {
		return "", errors.New("hit URL must be an HTTPS URL without credentials, query, or fragment")
	}
	u.Scheme = "https"
	u.Path = strings.TrimRight(u.EscapedPath(), "/")
	return u.String(), nil
}

func mustDigest(r Request) domain.Digest { d, _ := r.Digest(); return d }
