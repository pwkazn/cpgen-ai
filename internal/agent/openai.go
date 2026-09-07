// Package agent contains provider adapters used by the workflow.  The
// adapter in this file deliberately exposes only the provider-neutral model
// port; provider wire details never leave this package.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	defaultTimeout         = 30 * time.Second
	defaultMaxResponse     = int64(1 << 20)
	defaultMaxAttempts     = 2
	defaultRetryBaseDelay  = 100 * time.Millisecond
	defaultRetryMaxDelay   = 2 * time.Second
	defaultCompletionPath  = "/chat/completions"
	maxProviderModelLength = 256
)

// ErrorCode identifies a sanitized adapter failure.  Error values never
// retain response bodies, request payloads, URLs, or credentials.
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

// Error is intentionally small and safe to return to a caller or persist as
// sanitized evidence. Status is an HTTP status when one was received.
type Error struct {
	Code        ErrorCode
	Status      int
	RetryAfter  time.Duration
	UnwrapCause error
}

func (e *Error) Error() string {
	if e == nil {
		return "model adapter error"
	}
	switch e.Code {
	case ErrorConfiguration:
		return "model adapter configuration is invalid"
	case ErrorPolicy:
		return "model adapter request violates endpoint policy"
	case ErrorTransport:
		return "model adapter transport failed"
	case ErrorHTTP:
		if e.Status > 0 {
			return "model provider returned HTTP status " + strconv.Itoa(e.Status)
		}
		return "model provider returned an HTTP error"
	case ErrorResponseTooBig:
		return "model provider response exceeds the configured size limit"
	case ErrorProtocol:
		return "model provider response did not satisfy the protocol"
	case ErrorCredential:
		return "model adapter credential is unavailable"
	case ErrorCanceled:
		return "model adapter request was canceled"
	default:
		return "model adapter request failed"
	}
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.UnwrapCause
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.Code == other.Code
}

// Config is the redacted provider configuration consumed by the adapter.
// APIKeyEnv is an environment variable name, never the key itself. An empty
// AllowedHosts list means exactly the configured endpoint hostname, which is
// still an explicit host allowlist and keeps the safe default useful for a
// single provider.
type Config struct {
	Endpoint         string
	Model            string
	APIKeyEnv        string
	AllowedHosts     []string
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxAttempts      int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration
	CompletionPath   string

	// AllowInsecureHTTP is for an explicitly selected local test endpoint. It
	// is rejected for non-loopback hosts and is never enabled by default.
	AllowInsecureHTTP bool

	// HTTPClient is optional dependency injection for tests (for example, a
	// httptest TLS transport). Its transport is copied into a policy-controlled
	// client and its redirect policy is ignored.
	HTTPClient *http.Client
	Now        func() time.Time
}

// Validate applies the same endpoint, host, credential-reference, retry,
// and response-limit policy used by New without reading the credential or
// performing I/O.
func (c Config) Validate() error {
	_, _, err := c.normalized()
	return err
}

func (c Config) normalized() (Config, *url.URL, error) {
	if strings.TrimSpace(c.Endpoint) == "" || strings.TrimSpace(c.Endpoint) != c.Endpoint || strings.IndexFunc(c.Endpoint, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) >= 0 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	parsed, err := url.Parse(c.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !c.AllowInsecureHTTP || !isLoopbackHost(parsed.Hostname()) {
			return Config{}, nil, &Error{Code: ErrorPolicy}
		}
	}
	if strings.TrimSpace(c.Model) == "" || len(c.Model) > maxProviderModelLength || !utf8.ValidString(c.Model) || strings.IndexFunc(c.Model, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.APIKeyEnv == "" || !validEnvName(c.APIKeyEnv) {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.Timeout > 10*time.Minute {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = defaultMaxResponse
	}
	if c.MaxResponseBytes > 64<<20 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.MaxAttempts > 8 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.RetryBaseDelay < 0 {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.RetryBaseDelay == 0 {
		c.RetryBaseDelay = defaultRetryBaseDelay
	}
	if c.RetryMaxDelay <= 0 {
		c.RetryMaxDelay = defaultRetryMaxDelay
	}
	if c.RetryMaxDelay < c.RetryBaseDelay || c.RetryMaxDelay > time.Minute {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if c.CompletionPath == "" {
		c.CompletionPath = defaultCompletionPath
	}
	if !strings.HasPrefix(c.CompletionPath, "/") || strings.ContainsAny(c.CompletionPath, "?#") || strings.Contains(c.CompletionPath, "..") {
		return Config{}, nil, &Error{Code: ErrorConfiguration}
	}
	if !strings.HasSuffix(parsed.Path, c.CompletionPath) {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + c.CompletionPath
	}
	if parsed.Path == "" {
		parsed.Path = c.CompletionPath
	}
	hosts := c.AllowedHosts
	if len(hosts) == 0 {
		hosts = []string{parsed.Hostname()}
	}
	for _, host := range hosts {
		if !validAllowedHost(host) {
			return Config{}, nil, &Error{Code: ErrorConfiguration}
		}
	}
	if !hostAllowed(parsed, hosts) {
		return Config{}, nil, &Error{Code: ErrorPolicy}
	}
	c.AllowedHosts = append([]string(nil), hosts...)
	return c, parsed, nil
}

// New validates endpoint policy up front. It does not resolve DNS, read the
// credential, or make a network request.
func New(c Config) (*OpenAICompatible, error) {
	normalized, endpoint, err := c.normalized()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: normalized.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if normalized.HTTPClient != nil {
		client.Transport = normalized.HTTPClient.Transport
	}
	return &OpenAICompatible{config: normalized, endpoint: endpoint, client: client}, nil
}

// NewOpenAICompatible is an explicit constructor alias for callers that
// prefer naming the wire protocol at the call site.
func NewOpenAICompatible(c Config) (*OpenAICompatible, error) { return New(c) }

// OpenAIAdapter is retained as a descriptive provider-neutral alias.
type OpenAIAdapter = OpenAICompatible

// NewOpenAIAdapter is a descriptive constructor alias.
func NewOpenAIAdapter(c Config) (*OpenAIAdapter, error) { return New(c) }

// OpenAICompatible implements the provider-neutral MeteredLLM port for the
// common /chat/completions JSON protocol. It sends a stable idempotency key on
// every bounded retry and performs local strict structured-output validation.
type OpenAICompatible struct {
	config   Config
	endpoint *url.URL
	client   *http.Client
}

var _ port.MeteredLLM = (*OpenAICompatible)(nil)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature"`
	TopP           float64        `json:"top_p"`
	MaxTokens      int64          `json:"max_tokens"`
	ResponseFormat responseFormat `json:"response_format"`
	Stream         bool           `json:"stream"`
}

type providerResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// Generate performs one logical call. HTTP statuses with a known response
// boundary may be retried when transient; transport failures are retried with
// the same stable idempotency key, then conservatively returned as an unknown
// boundary. No retry is made for protocol, policy, or context failures.
func (a *OpenAICompatible) Generate(ctx context.Context, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	var empty domain.MeteredOutcome[port.GenerateResponse]
	if a == nil || a.client == nil || a.endpoint == nil {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if err := request.Validate(); err != nil {
		return empty, &Error{Code: ErrorConfiguration}
	}
	if err := ctx.Err(); err != nil {
		return empty, &Error{Code: ErrorCanceled, UnwrapCause: err}
	}
	if err := a.checkEndpointPolicy(); err != nil {
		return empty, err
	}
	apiKey, ok := os.LookupEnv(a.config.APIKeyEnv)
	if !ok || !validCredential(apiKey) {
		return empty, &Error{Code: ErrorCredential}
	}

	body, requestDigest, err := a.requestBody(request)
	if err != nil {
		return empty, &Error{Code: ErrorConfiguration}
	}
	logicalID := "llm:" + strings.TrimPrefix(string(requestDigest), "sha256:")
	idempotencyKey := "cpgen-llm-" + strings.TrimPrefix(string(requestDigest), "sha256:")
	physicalIDs := make([]domain.AttemptCallID, 0, a.config.MaxAttempts)
	for attempt := 1; attempt <= a.config.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return empty, &Error{Code: ErrorCanceled, UnwrapCause: err}
		}
		physicalID := physicalCallID(requestDigest, attempt)
		physicalIDs = append(physicalIDs, physicalID)
		responseBody, status, providerResponseID, retryAfter, err := a.doRequest(ctx, body, apiKey, idempotencyKey)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil {
					return empty, &Error{Code: ErrorCanceled, UnwrapCause: ctx.Err()}
				}
			}
			if adapterErr, ok := err.(*Error); ok && retryableAdapterError(adapterErr) && attempt < a.config.MaxAttempts {
				if err := a.waitRetry(ctx, attempt, adapterErr.RetryAfter); err != nil {
					return empty, err
				}
				continue
			}
			trace := dispatchedTrace(logicalID, physicalIDs)
			failure := domain.PortFailure{Code: failureCode(err), Class: failureClass(err)}
			if adapterErr, ok := err.(*Error); ok && adapterErr.RetryAfter > 0 {
				when := a.now().Add(adapterErr.RetryAfter)
				failure.RetryAfter = &when
			}
			return domain.MeteredOutcome[port.GenerateResponse]{Failure: &failure, CallTrace: trace}, nil
		}
		if status < 200 || status >= 300 {
			adapterErr := classifyHTTP(status, retryAfter)
			if retryableAdapterError(adapterErr) && attempt < a.config.MaxAttempts {
				if err := a.waitRetry(ctx, attempt, adapterErr.RetryAfter); err != nil {
					return empty, err
				}
				continue
			}
			trace := dispatchedTrace(logicalID, physicalIDs)
			failure := domain.PortFailure{Code: failureCode(adapterErr), Class: failureClass(adapterErr)}
			if adapterErr.RetryAfter > 0 {
				when := a.now().Add(adapterErr.RetryAfter)
				failure.RetryAfter = &when
			}
			return domain.MeteredOutcome[port.GenerateResponse]{Failure: &failure, CallTrace: trace}, nil
		}

		response, parseErr := a.decodeResponse(responseBody, body, request, requestDigest, logicalID, physicalIDs, providerResponseID)
		if parseErr != nil {
			trace := dispatchedTrace(logicalID, physicalIDs)
			return domain.MeteredOutcome[port.GenerateResponse]{Failure: &domain.PortFailure{Code: domain.FailureProtocol, Class: domain.FailureRejected}, CallTrace: trace}, nil
		}
		return domain.MeteredOutcome[port.GenerateResponse]{Value: &response, CallTrace: response.CallTrace}, nil
	}
	return empty, &Error{Code: ErrorTransport}
}

func (a *OpenAICompatible) requestBody(request port.GenerateRequest) ([]byte, domain.Digest, error) {
	variables, err := canonicalJSON(request.Variables)
	if err != nil {
		return nil, "", err
	}
	canonical := struct {
		Prompt    port.PromptRef       `json:"prompt"`
		Schema    port.OutputSchemaRef `json:"schema"`
		Variables json.RawMessage      `json:"variables"`
		Sampling  port.SamplingPolicy  `json:"sampling"`
		MaxOutput port.OutputLimit     `json:"max_output"`
		Model     string               `json:"model"`
	}{request.Prompt, request.Schema, variables, request.Sampling, request.MaxOutput, a.config.Model}
	identity, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", err
	}
	digest := domain.SumBytes(identity)
	wire := chatRequest{
		Model: a.config.Model,
		Messages: []chatMessage{
			{Role: "system", Content: "Return one JSON object using schema version " + string(request.Schema.SchemaVersion) + "."},
			{Role: "user", Content: string(variables)},
		},
		Temperature:    request.Sampling.Temperature,
		TopP:           request.Sampling.TopP,
		MaxTokens:      request.MaxOutput.Tokens,
		ResponseFormat: responseFormat{Type: "json_object"},
		Stream:         false,
	}
	body, err := json.Marshal(wire)
	return body, digest, err
}

func (a *OpenAICompatible) doRequest(ctx context.Context, body []byte, apiKey, idempotencyKey string) ([]byte, int, string, time.Duration, error) {
	requestContext, cancel := context.WithTimeout(ctx, a.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, a.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", 0, &Error{Code: ErrorConfiguration}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return nil, 0, "", 0, err
			}
			return nil, 0, "", 0, &Error{Code: ErrorTransport, UnwrapCause: err}
		}
		return nil, 0, "", 0, &Error{Code: ErrorTransport}
	}
	defer resp.Body.Close()
	if resp.ContentLength > a.config.MaxResponseBytes {
		return nil, resp.StatusCode, "", parseRetryAfter(resp.Header.Get("Retry-After")), &Error{Code: ErrorResponseTooBig}
	}
	limit := a.config.MaxResponseBytes
	if limit < int64(^uint(0)>>1) {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, "", parseRetryAfter(resp.Header.Get("Retry-After")), &Error{Code: ErrorTransport}
	}
	if int64(len(data)) > a.config.MaxResponseBytes {
		return nil, resp.StatusCode, "", parseRetryAfter(resp.Header.Get("Retry-After")), &Error{Code: ErrorResponseTooBig}
	}
	providerID := ""
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var envelope struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &envelope) == nil {
			providerID = safeMetadata(envelope.ID)
		}
	}
	return data, resp.StatusCode, providerID, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

func (a *OpenAICompatible) decodeResponse(raw, requestBody []byte, request port.GenerateRequest, requestDigest domain.Digest, logicalID string, physicalIDs []domain.AttemptCallID, providerID string) (port.GenerateResponse, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) || !jsonDocumentHasObject(raw) || hasDuplicateJSONFields(raw) {
		return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
	}
	var decoded providerResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&decoded); err != nil || len(decoded.Choices) == 0 {
		return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
	}
	content := bytes.TrimSpace(decoded.Choices[0].Message.Content)
	if len(content) == 0 {
		return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
	}
	var contentText string
	if content[0] == '"' {
		if err := json.Unmarshal(content, &contentText); err != nil {
			return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
		}
	} else {
		contentText = string(content)
	}
	structured := []byte(contentText)
	if err := port.ValidateStructuredOutput(structured, request.Schema.SchemaVersion, request.MaxOutput.Bytes); err != nil {
		return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
	}
	usage, usageSource := conservativeUsage(decoded.Usage, len(requestBody), request.MaxOutput.Tokens)
	responseDigest := domain.SumBytes(structured)
	meta := map[string]string{
		"adapter":          "openai-compatible-v1",
		"provider_host":    strings.ToLower(a.endpoint.Hostname()),
		"model":            safeMetadata(a.config.Model),
		"request_digest":   string(requestDigest),
		"response_digest":  string(responseDigest),
		"usage_source":     usageSource,
		"usage_settlement": usageSource,
		"attempt_count":    strconv.Itoa(len(physicalIDs)),
		"idempotency":      "stable",
	}
	if providerID == "" {
		providerID = safeMetadata(decoded.ID)
	}
	if providerID != "" {
		meta["provider_request_id"] = providerID
	}
	if decoded.Model != "" {
		meta["provider_model"] = safeMetadata(decoded.Model)
	}
	if reason := safeMetadata(decoded.Choices[0].FinishReason); reason != "" {
		meta["finish_reason"] = reason
	}
	trace := dispatchedTrace(logicalID, physicalIDs)
	response := port.GenerateResponse{Structured: append(json.RawMessage(nil), structured...), ProviderMeta: meta, Usage: usage, CallTrace: trace}
	if err := response.Validate(); err != nil {
		return port.GenerateResponse{}, &Error{Code: ErrorProtocol}
	}
	return response, nil
}

func conservativeUsage(usage *struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}, requestBytes int, maxOutput int64) (port.Usage, string) {
	if usage != nil && usage.PromptTokens != nil && usage.CompletionTokens != nil && *usage.PromptTokens >= 0 && *usage.CompletionTokens >= 0 {
		return port.Usage{InputTokens: *usage.PromptTokens, OutputTokens: *usage.CompletionTokens}, "provider_verified"
	}
	// Provider usage is optional. Bytes are a conservative token upper bound
	// for accounting purposes (one UTF-8 byte cannot encode more than one
	// token), and output is charged at the configured reservation ceiling.
	input := int64(requestBytes)
	if input < 0 {
		input = 0
	}
	return port.Usage{InputTokens: input, OutputTokens: maxOutput}, "conservative_upper_bound_v1"
}

func (a *OpenAICompatible) checkEndpointPolicy() error {
	if a.endpoint == nil || !hostAllowed(a.endpoint, a.config.AllowedHosts) {
		return &Error{Code: ErrorPolicy}
	}
	if a.endpoint.Scheme != "https" && !(a.config.AllowInsecureHTTP && isLoopbackHost(a.endpoint.Hostname())) {
		return &Error{Code: ErrorPolicy}
	}
	return nil
}

func (a *OpenAICompatible) waitRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := retryAfter
	if delay <= 0 {
		delay = a.config.RetryBaseDelay
		for i := 1; i < attempt && delay < a.config.RetryMaxDelay; i++ {
			if delay > a.config.RetryMaxDelay/2 {
				delay = a.config.RetryMaxDelay
				break
			}
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
		return &Error{Code: ErrorCanceled, UnwrapCause: ctx.Err()}
	case <-timer.C:
		return nil
	}
}

func (a *OpenAICompatible) now() time.Time {
	if a.config.Now != nil {
		return a.config.Now().UTC()
	}
	return time.Now().UTC()
}

func classifyHTTP(status int, retryAfter time.Duration) *Error {
	return &Error{Code: ErrorHTTP, Status: status, RetryAfter: retryAfter}
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(defaultRetryMaxDelay/time.Second) {
			return defaultRetryMaxDelay
		}
		delay := time.Duration(seconds) * time.Second
		if delay > defaultRetryMaxDelay {
			return defaultRetryMaxDelay
		}
		return delay
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			return 0
		}
		if delay > defaultRetryMaxDelay {
			return defaultRetryMaxDelay
		}
		return delay
	}
	return 0
}

func retryableAdapterError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == ErrorTransport {
		return true
	}
	if err.Code != ErrorHTTP {
		return false
	}
	return err.Status == http.StatusRequestTimeout || err.Status == http.StatusTooEarly || err.Status == http.StatusTooManyRequests || err.Status == http.StatusInternalServerError || err.Status == http.StatusBadGateway || err.Status == http.StatusServiceUnavailable || err.Status == http.StatusGatewayTimeout
}

func failureCode(err error) domain.PortFailureCode {
	var adapterErr *Error
	if errors.As(err, &adapterErr) {
		switch adapterErr.Code {
		case ErrorTransport:
			return domain.FailureBoundaryUnknown
		case ErrorPolicy, ErrorCredential:
			return domain.FailurePolicyRejected
		case ErrorHTTP:
			if adapterErr.Status == http.StatusTooManyRequests || adapterErr.Status == http.StatusRequestTimeout || adapterErr.Status >= 500 {
				return domain.FailureUnavailable
			}
			return domain.FailureProtocol
		default:
			return domain.FailureProtocol
		}
	}
	return domain.FailureTransport
}

func failureClass(err error) domain.FailureClass {
	var adapterErr *Error
	if errors.As(err, &adapterErr) {
		if adapterErr.Code == ErrorTransport {
			return domain.FailureUnknown
		}
		if retryableAdapterError(adapterErr) {
			return domain.FailureRetryable
		}
		if adapterErr.Code == ErrorPolicy || adapterErr.Code == ErrorCredential {
			return domain.FailureBlocked
		}
	}
	return domain.FailureRejected
}

func dispatchedTrace(logicalID string, ids []domain.AttemptCallID) domain.CallTrace {
	copyIDs := append([]domain.AttemptCallID(nil), ids...)
	result := copyIDs[len(copyIDs)-1]
	return domain.CallTrace{LogicalOperationID: logicalID, DispatchKind: domain.DispatchDispatched, PhysicalAttemptCallIDs: copyIDs, ResultAttemptCallID: &result}
}

func physicalCallID(requestDigest domain.Digest, attempt int) domain.AttemptCallID {
	sum := domain.SumBytes([]byte(string(requestDigest) + ":" + strconv.Itoa(attempt)))
	return domain.AttemptCallID("llm_" + strings.TrimPrefix(string(sum), "sha256:")[:32])
}

func canonicalJSON(raw []byte) ([]byte, error) {
	if hasDuplicateJSONFields(raw) {
		return nil, errors.New("duplicate JSON field")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return json.Marshal(value)
}

func jsonDocumentHasObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func hasDuplicateJSONFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	return scanDuplicateValue(decoder)
}

func scanDuplicateValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return false
	}
	switch delimiter {
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
			if scanDuplicateValue(decoder) {
				return true
			}
		}
		_, _ = decoder.Token()
	case '[':
		for decoder.More() {
			if scanDuplicateValue(decoder) {
				return true
			}
		}
		_, _ = decoder.Token()
	}
	return false
}

func validEnvName(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for index, r := range value {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
		if index == 0 && r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}

func validCredential(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func validAllowedHost(raw string) bool {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "/?#") || strings.Contains(raw, "*") || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }) >= 0 {
		return false
	}
	host := raw
	if parsedHost, _, err := net.SplitHostPort(raw); err == nil {
		host = parsedHost
	} else if net.ParseIP(strings.Trim(raw, "[]")) != nil {
		host = strings.Trim(raw, "[]")
	} else if strings.Contains(raw, ":") {
		return false
	}
	return host != "" && utf8.ValidString(host)
}

func hostAllowed(endpoint *url.URL, allowed []string) bool {
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	portValue := endpoint.Port()
	if portValue == "" {
		switch endpoint.Scheme {
		case "http":
			portValue = "80"
		case "https":
			portValue = "443"
		}
	}
	for _, item := range allowed {
		candidate := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(item), "."))
		candidateHost := candidate
		candidatePort := ""
		if parsedHost, parsedPort, err := net.SplitHostPort(candidate); err == nil {
			candidateHost, candidatePort = strings.ToLower(strings.TrimSuffix(parsedHost, ".")), parsedPort
		} else if strings.HasPrefix(candidate, "[") && strings.HasSuffix(candidate, "]") {
			candidateHost = strings.Trim(candidate, "[]")
		}
		if candidateHost == host && (candidatePort == "" || candidatePort == portValue) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeMetadata(value string) string {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return value
}
