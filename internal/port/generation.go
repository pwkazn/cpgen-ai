package port

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cpgen/internal/domain"
)

type PromptRef struct {
	Step           string               `json:"step"`
	Version        string               `json:"version"`
	Digest         domain.Digest        `json:"digest"` // legacy alias for TemplateDigest
	TemplateDigest domain.Digest        `json:"template_digest,omitempty"`
	SchemaVersion  domain.SchemaVersion `json:"schema_version,omitempty"`
	SchemaDigest   domain.Digest        `json:"schema_digest,omitempty"`
}

func (r PromptRef) Validate() error {
	if r.Step == "" || r.Version == "" {
		return fmt.Errorf("prompt step and version are required")
	}
	if r.Digest != "" && r.TemplateDigest != "" && r.Digest != r.TemplateDigest {
		return fmt.Errorf("prompt template digests differ")
	}
	templateDigest := r.TemplateDigest
	if templateDigest == "" {
		templateDigest = r.Digest
	}
	if err := templateDigest.Validate(); err != nil {
		return err
	}
	if r.SchemaVersion != "" {
		if err := r.SchemaVersion.Validate(); err != nil {
			return err
		}
		if err := r.SchemaDigest.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type OutputSchemaRef struct {
	SchemaVersion domain.SchemaVersion `json:"schema_version"`
	Digest        domain.Digest        `json:"digest"`
}

func (r OutputSchemaRef) Validate() error {
	if err := r.SchemaVersion.Validate(); err != nil {
		return err
	}
	return r.Digest.Validate()
}

type SamplingPolicy struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
}

func (p SamplingPolicy) Validate() error {
	if p.Temperature < 0 || p.Temperature > 2 || p.TopP <= 0 || p.TopP > 1 {
		return fmt.Errorf("sampling policy is outside the allowed range")
	}
	return nil
}

type OutputLimit struct {
	Tokens int64 `json:"tokens"`
	Bytes  int64 `json:"bytes"`
}

func (l OutputLimit) Validate() error {
	if l.Tokens <= 0 || l.Bytes <= 0 {
		return fmt.Errorf("output token and byte limits must be positive")
	}
	return nil
}

type GenerateRequest struct {
	Prompt    PromptRef       `json:"prompt"`
	Schema    OutputSchemaRef `json:"schema"`
	Variables json.RawMessage `json:"variables"`
	Sampling  SamplingPolicy  `json:"sampling"`
	MaxOutput OutputLimit     `json:"max_output"`
}

func (r GenerateRequest) Validate() error {
	if err := r.Prompt.Validate(); err != nil {
		return err
	}
	if err := r.Schema.Validate(); err != nil {
		return err
	}
	if len(r.Variables) == 0 || !json.Valid(r.Variables) {
		return fmt.Errorf("variables must be valid JSON")
	}
	if err := r.Sampling.Validate(); err != nil {
		return err
	}
	return r.MaxOutput.Validate()
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type GenerateResponse struct {
	Structured   json.RawMessage         `json:"structured"`
	RawBlob      *domain.PendingArtifact `json:"raw_blob,omitempty"`
	ProviderMeta map[string]string       `json:"provider_meta,omitempty"`
	Usage        Usage                   `json:"usage"`
	CallTrace    domain.CallTrace        `json:"call_trace"`
}

func (r GenerateResponse) Validate() error {
	if len(r.Structured) == 0 || !json.Valid(r.Structured) {
		return fmt.Errorf("structured response must be valid JSON")
	}
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 {
		return fmt.Errorf("token usage must be non-negative")
	}
	if r.RawBlob != nil {
		if err := r.RawBlob.Validate(); err != nil {
			return err
		}
	}
	return r.CallTrace.Validate()
}

type MeteredLLM interface {
	Generate(ctx context.Context, request GenerateRequest) (domain.MeteredOutcome[GenerateResponse], error)
}

type SimilaritySearchRequest struct {
	Query       string        `json:"query"`
	QueryDigest domain.Digest `json:"query_digest"`
	Limit       int           `json:"limit"`
}

func (r SimilaritySearchRequest) Validate() error {
	if r.Query == "" || r.Limit <= 0 {
		return fmt.Errorf("similarity query and positive limit are required")
	}
	return r.QueryDigest.Validate()
}

type SimilarityHit struct {
	ExternalID string  `json:"external_id"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
}

type SimilarityEvidence struct {
	Provider         string                     `json:"provider"`
	ServiceIdentity  string                     `json:"service_identity"`
	QueryDigest      domain.Digest              `json:"query_digest"`
	Hits             []SimilarityHit            `json:"hits"`
	ModelVersion     string                     `json:"model_version"`
	IndexVersion     string                     `json:"index_version"`
	ResponseMetadata map[string]json.RawMessage `json:"response_metadata,omitempty"`
	RetrievedAt      time.Time                  `json:"retrieved_at"`
	CallTrace        domain.CallTrace           `json:"call_trace"`
}

func (e SimilarityEvidence) Validate() error {
	if e.Provider == "" || e.ServiceIdentity == "" {
		return fmt.Errorf("similarity provider and service identity are required")
	}
	if err := e.QueryDigest.Validate(); err != nil {
		return err
	}
	if e.RetrievedAt.IsZero() {
		return fmt.Errorf("similarity retrieval time is required")
	}
	for index, hit := range e.Hits {
		if hit.ExternalID == "" || hit.Title == "" {
			return fmt.Errorf("similarity hit %d is missing identity or title", index)
		}
	}
	return e.CallTrace.Validate()
}

type MeteredSimilarity interface {
	Search(ctx context.Context, request SimilaritySearchRequest) (domain.MeteredOutcome[SimilarityEvidence], error)
}
