package similarity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const (
	defaultHTTPTimeout       = 10 * time.Second
	defaultHTTPResponseBytes = int64(1 << 20)
	defaultHTTPMaxHits       = 20
	defaultHTTPMaxAttempts   = 2
	defaultHTTPRetryBase     = 100 * time.Millisecond
	defaultHTTPRetryMax      = 2 * time.Second
)

type ErrorCode string

const (
	ErrorConfiguration  ErrorCode = "configuration"
	ErrorPolicy         ErrorCode = "policy_rejected"
	ErrorTransport      ErrorCode = "transport"
	ErrorHTTP           ErrorCode = "http"
	ErrorResponseTooBig ErrorCode = "response_too_large"
	ErrorProtocol       ErrorCode = "protocol"
	ErrorCredential     ErrorCode = "credential"
	ErrorCanceled       ErrorCode = "canceled"
)

// Error never stores a URL, response body, request content, or credential.
// The optional cause is intentionally not included in Error() and is only
// useful to trusted callers doing errors.Is for context cancellation.
type Error struct {
	Code            ErrorCode
	Status          int
	RetryAfter      time.Duration
	ConfirmedNoSend bool
	cause           error
}

func (e *Error) Error() string {
	if e == nil {
		return "similarity adapter error"
	}
	switch e.Code {
	case ErrorConfiguration:
		return "similarity adapter configuration is invalid"
	case ErrorPolicy:
		return "similarity adapter request violates endpoint policy"
	case ErrorTransport:
		return "similarity adapter transport failed"
	case ErrorHTTP:
		if e.Status > 0 {
			return "similarity provider returned HTTP status " + strconv.Itoa(e.Status)
		}
		return "similarity provider returned an HTTP error"
	case ErrorResponseTooBig:
		return "similarity provider response exceeds the configured size limit"
	case ErrorProtocol:
		return "similarity provider response did not satisfy the protocol"
	case ErrorCredential:
		return "similarity adapter credential is unavailable"
	case ErrorCanceled:
		return "similarity adapter request was canceled"
	default:
		return "similarity adapter request failed"
	}
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.Code == other.Code
}

// ErrConfirmedNoSend is an explicit transport boundary supplied by a test or
// trusted transport. It is the only transport failure eligible for retry.
var ErrConfirmedNoSend = errors.New("confirmed no send")

type Config struct {
	Endpoint         string
	APIKeyEnv        string
	AllowedHosts     []string
	ProviderIdentity string
	ServiceIdentity  string
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxHits          int
	MaxAttempts      int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration

	// These flags are intentionally explicit test-only escape hatches. HTTP
	// is accepted only for loopback and HTTPS loopback is otherwise rejected.
	AllowInsecureHTTP       bool
	AllowLoopbackForTesting bool
	HTTPClient              *http.Client
	Now                     func() time.Time
}

func (c Config) Validate() error {
	_, _, err := c.normalized()
	return err
}

func (c Config) normalized() (Config, *url.URL, error) {
	if strings.TrimSpace(c.Endpoint) == "" || strings.TrimSpace(c.Endpoint) != c.Endpoint || !utf8.ValidString(c.Endpoint) || strings.IndexFunc(c.Endpoint, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) >= 0 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	hostname := strings.ToLower(endpoint.Hostname())
	if endpoint.Scheme != "https" {
		if endpoint.Scheme != "http" || !c.AllowInsecureHTTP || !isLoopbackHost(hostname) {
			return Config{}, nil, &Error{Code: ErrorPolicy}
		}
	} else if isLoopbackHost(hostname) && !c.AllowLoopbackForTesting {
		return Config{}, nil, &Error{Code: ErrorPolicy}
	}
	if c.APIKeyEnv == "" || !validEnvName(c.APIKeyEnv) {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultHTTPTimeout
	}
	if c.Timeout > 10*time.Minute {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = defaultHTTPResponseBytes
	}
	if c.MaxResponseBytes > 64<<20 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.MaxHits <= 0 {
		c.MaxHits = defaultHTTPMaxHits
	}
	if c.MaxHits > 10000 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultHTTPMaxAttempts
	}
	if c.MaxAttempts > 8 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.RetryBaseDelay < 0 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.RetryBaseDelay == 0 {
		c.RetryBaseDelay = defaultHTTPRetryBase
	}
	if c.RetryMaxDelay <= 0 {
		c.RetryMaxDelay = defaultHTTPRetryMax
	}
	if c.RetryMaxDelay < c.RetryBaseDelay || c.RetryMaxDelay > time.Minute {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.ProviderIdentity == "" {
		c.ProviderIdentity = hostname
	}
	if c.ServiceIdentity == "" {
		c.ServiceIdentity = endpoint.Host
	}
	if err := validateSafeIdentity(c.ProviderIdentity); err != nil {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if err := validateSafeIdentity(c.ServiceIdentity); err != nil {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	hosts := append([]string(nil), c.AllowedHosts...)
	if len(hosts) == 0 {
		hosts = []string{hostname}
	}
	for i := range hosts {
		hosts[i] = strings.ToLower(strings.TrimSpace(hosts[i]))
		if !validAllowedHost(hosts[i]) {
			return Config{}, nil, &Error{Code: ErrorConfiguration}
		}
	}
	c.AllowedHosts = hosts
	if !hostAllowed(endpoint, hosts) {
		return Config{}, nil, &Error{Code: ErrorPolicy}
	}
	return c, endpoint, nil
}

type HTTPAdapter struct {
	config   Config
	endpoint *url.URL
	client   *http.Client
}

// Adapter is a short alias for HTTPAdapter.
type Adapter = HTTPAdapter

func New(c Config) (*HTTPAdapter, error) {
	normalized, endpoint, err := c.normalized()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: normalized.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	base := http.DefaultTransport
	if normalized.HTTPClient != nil && normalized.HTTPClient.Transport != nil {
		base = normalized.HTTPClient.Transport
	}
	allowLoopback := normalized.AllowInsecureHTTP || normalized.AllowLoopbackForTesting
	if _, ok := base.(*http.Transport); !ok && !isLoopbackHost(endpoint.Hostname()) {
		// An arbitrary RoundTripper can choose a proxy or destination that is
		// unrelated to the policy-checked endpoint. Only permit it for explicit
		// loopback tests; public calls use the dial-enforced standard transport.
		return nil, &Error{Code: ErrorPolicy}
	}
	client.Transport = newPolicyTransport(base, allowLoopback, normalized.AllowedHosts)
	return &HTTPAdapter{config: normalized, endpoint: endpoint, client: client}, nil
}

func NewHTTPAdapter(c Config) (*HTTPAdapter, error) { return New(c) }

// EvidenceOutcome is the typed result of the richer Request boundary. A
// failure is returned as data so callers can persist its safe class and trace
// without having to stringify an adapter error.
type EvidenceOutcome struct {
	Value     *Evidence
	Failure   *domain.PortFailure
	CallTrace domain.CallTrace
}

func (o EvidenceOutcome) Validate() error {
	if (o.Value == nil) == (o.Failure == nil) {
		return errors.New("similarity outcome requires exactly one of value or failure")
	}
	if o.Failure != nil {
		if err := o.Failure.Validate(); err != nil {
			return err
		}
	}
	if err := o.CallTrace.Validate(); err != nil {
		return err
	}
	if o.Value != nil {
		if err := o.Value.Validate(); err != nil {
			return err
		}
		if !o.Value.CallTrace.Equal(o.CallTrace) {
			return errors.New("similarity value and wrapper traces differ")
		}
	}
	return nil
}

// SearchEvidence executes the richer provider-neutral contract.
func (a *HTTPAdapter) SearchEvidence(ctx context.Context, request Request) (EvidenceOutcome, error) {
	if a == nil {
		return EvidenceOutcome{}, &Error{Code: ErrorConfiguration}
	}
	return a.searchEvidence(ctx, request, request.Limit)
}

func (a *HTTPAdapter) searchEvidence(ctx context.Context, request Request, maxHits int) (EvidenceOutcome, error) {
	var empty EvidenceOutcome
	if a == nil || a.client == nil || a.endpoint == nil {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if err := request.Validate(); err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	if maxHits <= 0 || maxHits > a.config.MaxHits {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if err := ctx.Err(); err != nil {
		return empty, &Error{Code: ErrorCanceled, cause: err}
	}
	if err := a.checkEndpointPolicy(); err != nil {
		return empty, err
	}
	requestBytes, requestDigest, err := requestBody(request)
	if err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	logicalID := "similarity:" + strings.TrimPrefix(string(requestDigest), "sha256:")
	traceIDs := make([]domain.AttemptCallID, 0, a.config.MaxAttempts)
	key, ok := os.LookupEnv(a.config.APIKeyEnv)
	if !ok || !validCredential(key) {
		trace := noDispatchTrace(logicalID)
		failure := domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureBlocked}
		return EvidenceOutcome{Failure: &failure, CallTrace: trace}, nil
	}
	for attempt := 1; attempt <= a.config.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if len(traceIDs) > 0 {
				return unknownOutcome(logicalID, traceIDs), nil
			}
			return empty, &Error{Code: ErrorCanceled, cause: err}
		}
		physicalID := physicalCallID(requestDigest, attempt)
		body, status, retryAfter, sent, err := a.doRequest(ctx, requestBytes, key, logicalID)
		if sent {
			traceIDs = append(traceIDs, physicalID)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil && len(traceIDs) > 0 {
					return unknownOutcome(logicalID, traceIDs), nil
				}
			}
			adapterErr := asError(err)
			if adapterErr.ConfirmedNoSend && retryable(adapterErr) && attempt < a.config.MaxAttempts {
				if waitErr := a.waitRetry(ctx, attempt, adapterErr.RetryAfter); waitErr != nil {
					if len(traceIDs) > 0 {
						return unknownOutcome(logicalID, traceIDs), nil
					}
					return empty, waitErr
				}
				continue
			}
			return a.failureOutcome(logicalID, traceIDs, adapterErr), nil
		}
		if status < 200 || status >= 300 {
			adapterErr := &Error{Code: ErrorHTTP, Status: status, RetryAfter: retryAfter}
			if retryable(adapterErr) && attempt < a.config.MaxAttempts {
				if waitErr := a.waitRetry(ctx, attempt, retryAfter); waitErr != nil {
					if len(traceIDs) > 0 {
						return unknownOutcome(logicalID, traceIDs), nil
					}
					return empty, waitErr
				}
				continue
			}
			return a.failureOutcome(logicalID, traceIDs, adapterErr), nil
		}
		wire, usage, usageSource, parseErr := decodeResponse(body, requestBytes, maxHits)
		if parseErr != nil {
			return a.failureOutcome(logicalID, traceIDs, parseErr), nil
		}
		trace := dispatchedTrace(logicalID, traceIDs)
		provider := wire.ProviderIdentity
		if provider == "" {
			provider = wire.Provider
		}
		if provider == "" {
			provider = a.config.ProviderIdentity
		}
		hits := make([]Hit, 0, len(wire.Hits))
		for i, rawHit := range wire.Hits {
			hit, hitErr := wireHitToHit(rawHit)
			if hitErr != nil {
				return a.failureOutcome(logicalID, traceIDs, &Error{Code: ErrorProtocol, cause: fmt.Errorf("hit %d: %w", i, hitErr)}), nil
			}
			hits = append(hits, hit)
		}
		observedAt := a.now()
		evidence, evidenceErr := NewEvidenceWithMetadata(request, provider, hits, observedAt, usage, usageSource, wire.ModelVersion, wire.IndexVersion, CacheProvenance{Kind: CacheLive}, trace)
		if evidenceErr != nil {
			return a.failureOutcome(logicalID, traceIDs, &Error{Code: ErrorProtocol, cause: evidenceErr}), nil
		}
		result := EvidenceOutcome{Value: &evidence, CallTrace: trace}
		return result, nil
	}
	return empty, &Error{Code: ErrorTransport}
}

// Search adapts the richer contract to the existing MeteredSimilarity port.
// The compatibility request treats the old Query as a package-safe statement;
// callers needing policy-bound provenance should use SearchEvidence.
func (a *HTTPAdapter) Search(ctx context.Context, request port.SimilaritySearchRequest) (domain.MeteredOutcome[port.SimilarityEvidence], error) {
	var empty domain.MeteredOutcome[port.SimilarityEvidence]
	if a == nil {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if err := request.Validate(); err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	if request.QueryDigest != domain.SumBytes([]byte(request.Query)) {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if request.Limit > a.config.MaxHits {
		return empty, &Error{Code: ErrorConfiguration}
	}
	projection, err := NewPackageSafeProjection("similarity query", request.Query, nil, "unknown")
	if err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	policyDigest := domain.SumBytes([]byte("cpgen-similarity-compat-policy-v1"))
	logical := "compat_" + strings.TrimPrefix(string(request.QueryDigest), "sha256:")[:32]
	richRequest, err := NewRequest(projection, "compat/v1", policyDigest, logical)
	if err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	richRequest.Limit = request.Limit
	if err := richRequest.Validate(); err != nil {
		return empty, &Error{Code: ErrorConfiguration, cause: err}
	}
	outcome, err := a.searchEvidence(ctx, richRequest, request.Limit)
	if err != nil {
		return empty, err
	}
	if outcome.Value != nil {
		hits := make([]port.SimilarityHit, 0, len(outcome.Value.Hits))
		for _, hit := range outcome.Value.Hits {
			title := hit.ExternalID
			hits = append(hits, port.SimilarityHit{ExternalID: hit.ExternalID, Title: title, Score: hit.Score})
		}
		metadata := map[string]json.RawMessage{}
		putMetadata := func(key string, value any) {
			raw, _ := json.Marshal(value)
			metadata[key] = raw
		}
		putMetadata("usage_source", outcome.Value.UsageSource)
		putMetadata("input_tokens", outcome.Value.Usage.InputTokens)
		putMetadata("output_tokens", outcome.Value.Usage.OutputTokens)
		if outcome.Value.Usage.CostMicroUSD > 0 {
			putMetadata("cost_micro_usd", outcome.Value.Usage.CostMicroUSD)
		}
		evidence := port.SimilarityEvidence{Provider: outcome.Value.ProviderIdentity, ServiceIdentity: a.config.ServiceIdentity, QueryDigest: request.QueryDigest, Hits: hits, ModelVersion: "", IndexVersion: "", ResponseMetadata: metadata, RetrievedAt: outcome.Value.ObservedAt, CallTrace: outcome.CallTrace}
		return domain.MeteredOutcome[port.SimilarityEvidence]{Value: &evidence, CallTrace: outcome.CallTrace}, nil
	}
	return domain.MeteredOutcome[port.SimilarityEvidence]{Failure: outcome.Failure, CallTrace: outcome.CallTrace}, nil
}

var _ port.MeteredSimilarity = (*HTTPAdapter)(nil)

type wireResponse struct {
	ProviderIdentity string     `json:"provider_identity"`
	Provider         string     `json:"provider"`
	ModelVersion     string     `json:"model_version"`
	IndexVersion     string     `json:"index_version"`
	Hits             []wireHit  `json:"hits"`
	Usage            *wireUsage `json:"usage,omitempty"`
}

type wireHit struct {
	Source       string        `json:"source"`
	ExternalID   string        `json:"external_id"`
	Title        string        `json:"title,omitempty"`
	Score        float64       `json:"score"`
	CanonicalURL string        `json:"canonical_url,omitempty"`
	TitleDigest  domain.Digest `json:"title_digest,omitempty"`
}

type wireUsage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	CostMicroUSD *int64 `json:"cost_micro_usd,omitempty"`
}

func requestBody(request Request) ([]byte, domain.Digest, error) {
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	wire := struct {
		ProtocolVersion           string               `json:"protocol_version"`
		SchemaVersion             domain.SchemaVersion `json:"schema_version"`
		CandidateProjectionDigest domain.Digest        `json:"candidate_projection_digest"`
		NormalizedTitle           string               `json:"normalized_title"`
		NormalizedStatement       string               `json:"normalized_statement"`
		NormalizedTags            []string             `json:"normalized_tags"`
		Language                  string               `json:"language"`
		PolicyRef                 string               `json:"policy_ref"`
		PolicyDigest              domain.Digest        `json:"policy_digest"`
		LogicalID                 string               `json:"logical_idempotency_key"`
		Limit                     int                  `json:"limit"`
	}{ProtocolVersion, request.SchemaVersion, request.CandidateProjectionDigest, request.NormalizedTitle, request.NormalizedStatement, request.NormalizedTags, request.Language, request.PolicyRef, request.PolicyDigest, request.LogicalIdempotencyKey, request.Limit}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, "", err
	}
	return raw, domain.SumBytes(raw), nil
}

func (a *HTTPAdapter) doRequest(ctx context.Context, body []byte, key, logicalID string) ([]byte, int, time.Duration, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, false, &Error{Code: ErrorConfiguration}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", logicalID)
	response, err := a.client.Do(req)
	if err != nil {
		confirmed := errors.Is(err, ErrConfirmedNoSend)
		var typed *Error
		if errors.As(err, &typed) {
			return nil, 0, 0, !typed.ConfirmedNoSend, typed
		}
		if confirmed {
			return nil, 0, 0, false, &Error{Code: ErrorTransport, ConfirmedNoSend: true}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, 0, true, &Error{Code: ErrorTransport, cause: err}
		}
		return nil, 0, 0, true, &Error{Code: ErrorTransport, cause: err}
	}
	defer response.Body.Close()
	data, readErr := readCapped(response.Body, a.config.MaxResponseBytes)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return nil, response.StatusCode, 0, true, &Error{Code: ErrorTransport, cause: readErr}
		}
		if errors.Is(readErr, errResponseTooBig) {
			return nil, response.StatusCode, 0, true, &Error{Code: ErrorResponseTooBig}
		}
		return nil, response.StatusCode, 0, true, &Error{Code: ErrorTransport}
	}
	return data, response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After"), a.now()), true, nil
}

var errResponseTooBig = errors.New("response too large")

func readCapped(reader io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, errResponseTooBig
	}
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errResponseTooBig
	}
	return data, nil
}

func decodeResponse(raw, requestBytes []byte, maxHits int) (wireResponse, Usage, string, *Error) {
	var response wireResponse
	if int64(len(raw)) == 0 || !utf8.Valid(raw) || hasDuplicateJSONFields(raw) {
		return response, Usage{}, "", &Error{Code: ErrorProtocol}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, Usage{}, "", &Error{Code: ErrorProtocol}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return response, Usage{}, "", &Error{Code: ErrorProtocol}
	}
	if len(response.Hits) > maxHits {
		return response, Usage{}, "", &Error{Code: ErrorProtocol}
	}
	if response.ProviderIdentity == "" && response.Provider == "" {
		// The adapter supplies configured identity when it is omitted.
	}
	usage := Usage{}
	usageSourceValue := UsageConservativeUpperBound
	if response.Usage != nil && response.Usage.InputTokens != nil && response.Usage.OutputTokens != nil && *response.Usage.InputTokens >= 0 && *response.Usage.OutputTokens >= 0 {
		usage.InputTokens = *response.Usage.InputTokens
		usage.OutputTokens = *response.Usage.OutputTokens
		if response.Usage.CostMicroUSD != nil {
			if *response.Usage.CostMicroUSD < 0 {
				return response, Usage{}, "", &Error{Code: ErrorProtocol}
			}
			usage.CostMicroUSD = *response.Usage.CostMicroUSD
		}
		usageSourceValue = "provider_verified"
	} else {
		usage.InputTokens = int64(len(requestBytes))
		usage.OutputTokens = int64(len(raw))
	}
	return response, usage, usageSourceValue, nil
}

func wireHitToHit(raw wireHit) (Hit, error) {
	title := cleanSingleLine(raw.Title)
	hit, err := NewHit(raw.Source, raw.ExternalID, title, raw.Score, raw.CanonicalURL)
	if err != nil {
		return Hit{}, err
	}
	if raw.TitleDigest != "" {
		if err := raw.TitleDigest.Validate(); err != nil {
			return Hit{}, err
		}
		if hit.TitleDigest != "" && raw.TitleDigest != hit.TitleDigest {
			return Hit{}, errors.New("provider title digest does not match title")
		}
		hit.TitleDigest = raw.TitleDigest
		hit.EvidenceDigest = hitDigest(hit)
	}
	return hit, nil
}

func (a *HTTPAdapter) failureOutcome(logicalID string, ids []domain.AttemptCallID, err *Error) EvidenceOutcome {
	trace := traceForIDs(logicalID, ids)
	failure := &domain.PortFailure{Code: failureCode(err), Class: failureClass(err)}
	if err.RetryAfter > 0 {
		when := a.now().Add(err.RetryAfter)
		failure.RetryAfter = &when
	}
	return EvidenceOutcome{Failure: failure, CallTrace: trace}
}

func unknownOutcome(logicalID string, ids []domain.AttemptCallID) EvidenceOutcome {
	failure := &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
	return EvidenceOutcome{Failure: failure, CallTrace: traceForIDs(logicalID, ids)}
}

func asError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return &Error{Code: ErrorTransport}
}

func (a *HTTPAdapter) checkEndpointPolicy() error {
	if a.endpoint == nil || !hostAllowed(a.endpoint, a.config.AllowedHosts) {
		return &Error{Code: ErrorPolicy}
	}
	if a.endpoint.Scheme != "https" && !(a.config.AllowInsecureHTTP && isLoopbackHost(a.endpoint.Hostname())) {
		return &Error{Code: ErrorPolicy}
	}
	return nil
}

func (a *HTTPAdapter) waitRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := retryAfter
	if delay <= 0 {
		delay = a.config.RetryBaseDelay
		for i := 1; i < attempt && delay < a.config.RetryMaxDelay; i++ {
			delay *= 2
		}
		if delay > a.config.RetryMaxDelay {
			delay = a.config.RetryMaxDelay
		}
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &Error{Code: ErrorCanceled, cause: ctx.Err()}
	case <-timer.C:
		return nil
	}
}

func (a *HTTPAdapter) now() time.Time {
	if a.config.Now != nil {
		return a.config.Now().UTC()
	}
	return time.Now().UTC()
}

func retryable(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == ErrorTransport {
		return err.ConfirmedNoSend
	}
	return err.Code == ErrorHTTP && (err.Status == http.StatusRequestTimeout || err.Status == http.StatusTooEarly || err.Status == http.StatusTooManyRequests || err.Status == http.StatusInternalServerError || err.Status == http.StatusBadGateway || err.Status == http.StatusServiceUnavailable || err.Status == http.StatusGatewayTimeout)
}

func failureCode(err *Error) domain.PortFailureCode {
	if err == nil {
		return domain.FailureTransport
	}
	switch err.Code {
	case ErrorPolicy, ErrorCredential:
		return domain.FailurePolicyRejected
	case ErrorResponseTooBig, ErrorProtocol:
		return domain.FailureProtocol
	case ErrorHTTP:
		if err.Status == http.StatusTooManyRequests {
			return domain.FailureRateLimited
		}
		if err.Status == http.StatusRequestTimeout || err.Status >= 500 {
			return domain.FailureUnavailable
		}
		if err.Status == http.StatusUnauthorized || err.Status == http.StatusForbidden {
			return domain.FailurePolicyRejected
		}
		return domain.FailureProtocol
	case ErrorTransport:
		if err.ConfirmedNoSend {
			return domain.FailureTransport
		}
		return domain.FailureBoundaryUnknown
	default:
		return domain.FailureTransport
	}
}

func failureClass(err *Error) domain.FailureClass {
	if err == nil {
		return domain.FailureRejected
	}
	if err.Code == ErrorTransport {
		if err.ConfirmedNoSend {
			return domain.FailureRetryable
		}
		return domain.FailureUnknown
	}
	if err.Code == ErrorPolicy || err.Code == ErrorCredential {
		return domain.FailureBlocked
	}
	if err.Code == ErrorHTTP && (err.Status == http.StatusUnauthorized || err.Status == http.StatusForbidden) {
		return domain.FailureBlocked
	}
	if retryable(err) {
		return domain.FailureRetryable
	}
	return domain.FailureRejected
}

func noDispatchTrace(logicalID string) domain.CallTrace {
	return domain.CallTrace{LogicalOperationID: logicalID, DispatchKind: domain.DispatchNone}
}

func traceForIDs(logicalID string, ids []domain.AttemptCallID) domain.CallTrace {
	if len(ids) == 0 {
		return noDispatchTrace(logicalID)
	}
	return dispatchedTrace(logicalID, ids)
}

func dispatchedTrace(logicalID string, ids []domain.AttemptCallID) domain.CallTrace {
	copyIDs := append([]domain.AttemptCallID(nil), ids...)
	result := copyIDs[len(copyIDs)-1]
	return domain.CallTrace{LogicalOperationID: logicalID, DispatchKind: domain.DispatchDispatched, PhysicalAttemptCallIDs: copyIDs, ResultAttemptCallID: &result}
}

func physicalCallID(requestDigest domain.Digest, attempt int) domain.AttemptCallID {
	digest := domain.SumBytes([]byte(string(requestDigest) + ":" + strconv.Itoa(attempt)))
	return domain.AttemptCallID("sim_" + strings.TrimPrefix(string(digest), "sha256:")[:32])
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		if delay > defaultHTTPRetryMax {
			return defaultHTTPRetryMax
		}
		return delay
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := when.Sub(now)
		if delay < 0 {
			return 0
		}
		if delay > defaultHTTPRetryMax {
			return defaultHTTPRetryMax
		}
		return delay
	}
	return 0
}

type policyTransport struct {
	base          http.RoundTripper
	allowLoopback bool
	hosts         []string
	dialEnforced  bool
}

func newPolicyTransport(base http.RoundTripper, allowLoopback bool, hosts []string) *policyTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok {
		clone := transport.Clone()
		// Proxies receive the request before the endpoint dial and therefore can
		// bypass target DNS/IP policy. Similarity calls are direct only.
		clone.Proxy = nil
		originalDial := clone.DialContext
		if originalDial == nil {
			dialer := &net.Dialer{}
			originalDial = dialer.DialContext
		}
		clone.DialTLSContext = nil
		clone.DialContext = similarityPolicyDialContext(originalDial, allowLoopback)
		return &policyTransport{base: clone, allowLoopback: allowLoopback, hosts: append([]string(nil), hosts...), dialEnforced: true}
	}
	return &policyTransport{base: base, allowLoopback: allowLoopback, hosts: append([]string(nil), hosts...)}
}

func similarityPolicyDialContext(original func(context.Context, string, string) (net.Conn, error), allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
		}
		if isLoopbackHost(host) {
			if !allowLoopback {
				return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
			}
			return original(ctx, network, address)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
		}
		var lastErr error
		for _, resolved := range addresses {
			if !isPublicIP(resolved.IP) || (network == "tcp4" && resolved.IP.To4() == nil) || (network == "tcp6" && resolved.IP.To4() != nil) {
				return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
			}
			connection, dialErr := original(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func (t *policyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || !hostAllowed(request.URL, t.hosts) {
		return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
	}
	host := request.URL.Hostname()
	if isLoopbackHost(host) {
		if !t.allowLoopback {
			return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
		}
	} else if !t.dialEnforced {
		addresses, err := net.DefaultResolver.LookupIPAddr(request.Context(), host)
		if err != nil || len(addresses) == 0 {
			return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
		}
		for _, address := range addresses {
			if !isPublicIP(address.IP) {
				return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
			}
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

func hostAllowed(endpoint *url.URL, hosts []string) bool {
	if endpoint == nil {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	for _, allowed := range hosts {
		if strings.EqualFold(host, strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

func validAllowedHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || strings.ContainsAny(host, "/\\?#:@[]") || strings.ContainsAny(host, " \t\r\n") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.Contains(host, ".") || host == "localhost"
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	address, err := netip.ParseAddr(ip.String())
	if err != nil {
		return false
	}
	for _, prefix := range similarityNonRoutablePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var similarityNonRoutablePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:2::/48"), netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("fec0::/10"),
}

func validateSafeIdentity(value string) error {
	return validateText("identity", value, 512, true, false)
}

func validEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func validCredential(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n")
}

// hasDuplicateJSONFields scans objects at every nesting level. It is kept
// local so provider response validation does not depend on model adapter code.
func hasDuplicateJSONFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	return scanDuplicateJSONValue(decoder)
}

func scanDuplicateJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return false
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok {
				return false
			}
			if _, exists := seen[name]; exists {
				return true
			}
			seen[name] = struct{}{}
			if scanDuplicateJSONValue(decoder) {
				return true
			}
		}
		_, _ = decoder.Token()
	case '[':
		for decoder.More() {
			if scanDuplicateJSONValue(decoder) {
				return true
			}
		}
		_, _ = decoder.Token()
	}
	return false
}
