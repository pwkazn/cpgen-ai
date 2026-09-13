package similarity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"cpgen/internal/domain"
)

// PhysicalProvider is the narrow boundary used by the durable application
// coordinator. The compatibility SearchEvidence method owns no ledger grant.
type PhysicalProvider interface {
	PlanSearch(Request) (PhysicalSearchPlan, error)
	ValidatePhysicalResponse(Request, Evidence) error
	SearchPhysical(context.Context, Request, domain.AttemptCallID) (PhysicalSearchResult, error)
}

type PhysicalSearchPlan struct {
	RequestDigest    domain.Digest
	PolicyDigest     domain.Digest
	Provider         string
	MaxResponseBytes int64
}

type PhysicalSearchResult struct {
	Execution domain.PhysicalExecution[Evidence]
	// Distinguishes an explicit verified zero from omitted cost. The durable
	// application uses its conservative ceiling when cost was not reported.
	CostVerified bool
}

var _ PhysicalProvider = (*HTTPAdapter)(nil)

// PlanSearch binds exact wire input to the effective physical adapter policy.
// It performs no credential lookup, network access, reservation or dispatch.
func (a *HTTPAdapter) PlanSearch(request Request) (PhysicalSearchPlan, error) {
	var result PhysicalSearchPlan
	if a == nil || a.endpoint == nil || a.config.MaxResponseBytes <= 0 || a.config.MaxResponseBytes > 64<<20 {
		return result, &Error{Code: ErrorConfiguration}
	}
	if request.Limit > a.config.MaxHits {
		return result, &Error{Code: ErrorConfiguration}
	}
	if err := a.checkEndpointPolicy(); err != nil {
		return result, err
	}
	wire, _, err := requestBody(request)
	if err != nil {
		return result, &Error{Code: ErrorConfiguration, cause: err}
	}
	// Standalone retry settings are intentionally absent: this boundary always
	// performs at most one HTTP exchange and the ledger owns every retry.
	policy, err := json.Marshal(struct {
		Revision          string   `json:"revision"`
		Endpoint          string   `json:"endpoint"`
		APIKeyEnv         string   `json:"api_key_env"`
		Provider          string   `json:"provider"`
		Service           string   `json:"service"`
		Timeout           string   `json:"timeout"`
		MaxResponseBytes  int64    `json:"max_response_bytes"`
		MaxHits           int      `json:"max_hits"`
		AllowedHosts      []string `json:"allowed_hosts"`
		AllowInsecureHTTP bool     `json:"allow_insecure_http"`
		AllowLoopback     bool     `json:"allow_loopback"`
	}{"cpgen.physical-similarity/v1", a.endpoint.String(), a.config.APIKeyEnv, a.config.ProviderIdentity, a.config.ServiceIdentity, a.config.Timeout.String(), a.config.MaxResponseBytes, a.config.MaxHits, append([]string(nil), a.config.AllowedHosts...), a.config.AllowInsecureHTTP, a.config.AllowLoopbackForTesting})
	if err != nil {
		return result, err
	}
	result.PolicyDigest = domain.SumBytes(policy)
	bound, err := json.Marshal(struct {
		Policy  domain.Digest   `json:"policy"`
		Request json.RawMessage `json:"request"`
	}{result.PolicyDigest, wire})
	if err != nil {
		return result, err
	}
	result.RequestDigest = domain.SumBytes(bound)
	result.Provider, result.MaxResponseBytes = strings.ToLower(a.endpoint.Hostname()), a.config.MaxResponseBytes
	return result, nil
}

func (a *HTTPAdapter) ValidatePhysicalResponse(request Request, evidence Evidence) error {
	if _, err := a.PlanSearch(request); err != nil {
		return err
	}
	if err := evidence.Validate(); err != nil {
		return err
	}
	digest, err := request.Digest()
	if err != nil {
		return err
	}
	if evidence.RequestDigest != digest || evidence.PolicyDigest != request.PolicyDigest || evidence.ServiceIdentity != a.config.ServiceIdentity || evidence.Cache.Kind != CacheLive || len(evidence.Hits) > request.Limit || evidence.CallTrace.LogicalOperationID != request.LogicalIdempotencyKey || evidence.CallTrace.DispatchKind != domain.DispatchDispatched || len(evidence.CallTrace.PhysicalAttemptCallIDs) != 1 {
		return errors.New("physical similarity evidence differs from the admitted request or provider")
	}
	return nil
}

// SearchPhysical accepts the ledger's physical identity and disables the
// compatibility retry loop on an invocation-local adapter copy. It never
// mutates the configured adapter or creates a new logical/physical identity.
func (a *HTTPAdapter) SearchPhysical(ctx context.Context, request Request, id domain.AttemptCallID) (PhysicalSearchResult, error) {
	var result PhysicalSearchResult
	if ctx == nil || id.Validate() != nil {
		return result, &Error{Code: ErrorConfiguration}
	}
	if _, err := a.PlanSearch(request); err != nil {
		return result, err
	}
	noSend := func(failure *domain.PortFailure) (PhysicalSearchResult, error) {
		return PhysicalSearchResult{Execution: domain.PhysicalExecution[Evidence]{Boundary: domain.BoundaryConfirmedNoSend, Failure: failure}}, nil
	}
	if ctx.Err() != nil {
		return noSend(&domain.PortFailure{Code: domain.FailureTransport, Class: domain.FailureRejected})
	}
	request.NormalizedTags = copyStrings(request.NormalizedTags)
	single := *a
	single.config.MaxAttempts = 1
	outcome, err := single.SearchEvidence(ctx, request)
	if err != nil {
		// SearchEvidence returns an error only before entering its exchange loop.
		// Post-send errors are typed outcomes with a retained dispatch boundary.
		typed := asError(err)
		return noSend(&domain.PortFailure{Code: failureCode(typed), Class: failureClass(typed)})
	}
	if err := outcome.Validate(); err != nil {
		return result, err
	}
	if outcome.Failure != nil {
		if outcome.CallTrace.DispatchKind == domain.DispatchNone {
			return noSend(outcome.Failure)
		}
		if outcome.Failure.Class == domain.FailureUnknown || outcome.Failure.Code == domain.FailureBoundaryUnknown || outcome.responseDigest == nil {
			result.Execution = domain.PhysicalExecution[Evidence]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}
			return result, nil
		}
		result.Execution = domain.PhysicalExecution[Evidence]{Boundary: domain.BoundaryCompleted, Failure: outcome.Failure, ProviderRequestID: "local-receipt:" + string(id), ResponseDigest: outcome.responseDigest}
		return result, nil
	}
	value := outcome.Value
	trace := domain.CallTrace{LogicalOperationID: request.LogicalIdempotencyKey, DispatchKind: domain.DispatchDispatched, PhysicalAttemptCallIDs: []domain.AttemptCallID{id}, ResultAttemptCallID: &id}
	evidence, err := NewEvidenceWithServiceIdentity(request, value.ProviderIdentity, value.ServiceIdentity, value.Hits, value.ObservedAt, value.Usage, value.UsageSource, value.ModelVersion, value.IndexVersion, CacheProvenance{Kind: CacheLive}, trace)
	if err != nil {
		return result, err
	}
	if err := a.ValidatePhysicalResponse(request, evidence); err != nil {
		return result, err
	}
	result.Execution = domain.PhysicalExecution[Evidence]{Boundary: domain.BoundaryCompleted, Value: &evidence, ProviderRequestID: "local-receipt:" + string(id), ResponseDigest: outcome.responseDigest}
	result.CostVerified = outcome.costVerified
	return result, nil
}
