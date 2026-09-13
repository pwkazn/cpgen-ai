package config

import (
	"errors"
	"math"
	"time"

	"cpgen/internal/workflow"
	"go.yaml.in/yaml/v3"
)

// WorkflowConfig explicitly selects an implemented live stage sequence.
// Nil preserves historical Fake configuration bytes. Retry, selection policy
// and prompt definitions are compiled, not arbitrary YAML-supplied programs.
type WorkflowConfig struct {
	Revision                         string `json:"revision" yaml:"revision"`
	IdeaCount                        int    `json:"idea_count" yaml:"idea_count"`
	LLMCostUpperBoundMicroUSD        int64  `json:"llm_cost_upper_bound_micro_usd" yaml:"llm_cost_upper_bound_micro_usd"`
	SimilarityCostUpperBoundMicroUSD int64  `json:"similarity_cost_upper_bound_micro_usd" yaml:"similarity_cost_upper_bound_micro_usd"`
}

type SimilarityConfig struct {
	Endpoint            string        `json:"endpoint" yaml:"endpoint"`
	APIKeyEnv           string        `json:"api_key_env" yaml:"api_key_env"`
	ProviderIdentity    string        `json:"provider_identity" yaml:"provider_identity"`
	ServiceIdentity     string        `json:"service_identity" yaml:"service_identity"`
	Timeout             time.Duration `json:"-" yaml:"-"`
	MaxResponseBytes    int64         `json:"max_response_bytes" yaml:"max_response_bytes"`
	Limit               int           `json:"limit" yaml:"limit"`
	PolicyRef           string        `json:"policy_ref" yaml:"policy_ref"`
	AcceptanceThreshold float64       `json:"acceptance_threshold" yaml:"acceptance_threshold"`
	RejectionThreshold  float64       `json:"rejection_threshold" yaml:"rejection_threshold"`
	MinimumHits         int           `json:"minimum_hits" yaml:"minimum_hits"`
}

type EffectiveSimilarity struct {
	Endpoint            string  `json:"endpoint"`
	APIKeyEnv           string  `json:"api_key_env"`
	ProviderIdentity    string  `json:"provider_identity"`
	ServiceIdentity     string  `json:"service_identity"`
	Timeout             string  `json:"timeout"`
	MaxResponseBytes    int64   `json:"max_response_bytes"`
	Limit               int     `json:"limit"`
	PolicyRef           string  `json:"policy_ref"`
	AcceptanceThreshold float64 `json:"acceptance_threshold"`
	RejectionThreshold  float64 `json:"rejection_threshold"`
	MinimumHits         int     `json:"minimum_hits"`
}

type rawWorkflowConfig struct {
	Revision                         string `yaml:"revision"`
	IdeaCount                        *int   `yaml:"idea_count"`
	LLMCostUpperBoundMicroUSD        int64  `yaml:"llm_cost_upper_bound_micro_usd"`
	SimilarityCostUpperBoundMicroUSD int64  `yaml:"similarity_cost_upper_bound_micro_usd"`
}

type rawSimilarityConfig struct {
	Endpoint            string   `yaml:"endpoint"`
	APIKeyEnv           string   `yaml:"api_key_env"`
	ProviderIdentity    string   `yaml:"provider_identity"`
	ServiceIdentity     string   `yaml:"service_identity"`
	Timeout             *string  `yaml:"timeout"`
	MaxResponseBytes    *int64   `yaml:"max_response_bytes"`
	Limit               *int     `yaml:"limit"`
	PolicyRef           string   `yaml:"policy_ref"`
	AcceptanceThreshold *float64 `yaml:"acceptance_threshold"`
	RejectionThreshold  *float64 `yaml:"rejection_threshold"`
	MinimumHits         *int     `yaml:"minimum_hits"`
}

func decodeWorkflow(raw *rawWorkflowConfig) (*WorkflowConfig, error) {
	if raw == nil {
		return nil, nil
	}
	c := &WorkflowConfig{Revision: raw.Revision, IdeaCount: 4, LLMCostUpperBoundMicroUSD: raw.LLMCostUpperBoundMicroUSD, SimilarityCostUpperBoundMicroUSD: raw.SimilarityCostUpperBoundMicroUSD}
	if raw.IdeaCount != nil {
		c.IdeaCount = *raw.IdeaCount
	}
	return c, c.Validate()
}

func (c WorkflowConfig) Validate() error {
	if c.Revision != workflow.LegacySimilarityCheckpointRevision && c.Revision != workflow.LegacySolutionCheckpointRevision && c.Revision != workflow.GenerationRevision {
		return field("workflow.revision", errors.New("must select a supported compiled live workflow revision"))
	}
	if c.IdeaCount < 2 || c.IdeaCount > 8 {
		return field("workflow.idea_count", errors.New("must be between 2 and 8"))
	}
	if c.LLMCostUpperBoundMicroUSD <= 0 || c.LLMCostUpperBoundMicroUSD > math.MaxInt64/8 {
		return field("workflow.llm_cost_upper_bound_micro_usd", errors.New("must be positive and fit eight bounded reservations"))
	}
	if c.SimilarityCostUpperBoundMicroUSD <= 0 || c.SimilarityCostUpperBoundMicroUSD > math.MaxInt64/8 {
		return field("workflow.similarity_cost_upper_bound_micro_usd", errors.New("must be positive and fit eight bounded reservations"))
	}
	return nil
}

func decodeSimilarity(raw *rawSimilarityConfig) (*SimilarityConfig, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.AcceptanceThreshold == nil {
		return nil, field("similarity.acceptance_threshold", errors.New("is required"))
	}
	if raw.RejectionThreshold == nil {
		return nil, field("similarity.rejection_threshold", errors.New("is required"))
	}
	c := &SimilarityConfig{Endpoint: raw.Endpoint, APIKeyEnv: raw.APIKeyEnv, ProviderIdentity: raw.ProviderIdentity, ServiceIdentity: raw.ServiceIdentity, Timeout: 30 * time.Second, MaxResponseBytes: 1 << 20, Limit: 20, PolicyRef: raw.PolicyRef, AcceptanceThreshold: *raw.AcceptanceThreshold, RejectionThreshold: *raw.RejectionThreshold, MinimumHits: 1}
	if raw.Timeout != nil {
		parsed, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, field("similarity.timeout", errors.New("must be a valid duration"))
		}
		c.Timeout = parsed
	}
	if raw.MaxResponseBytes != nil {
		c.MaxResponseBytes = *raw.MaxResponseBytes
	}
	if raw.Limit != nil {
		c.Limit = *raw.Limit
	}
	if raw.MinimumHits != nil {
		c.MinimumHits = *raw.MinimumHits
	}
	return c, c.Validate()
}

// Validate performs neither credential lookup nor DNS/network operations.
func (c SimilarityConfig) Validate() error {
	if !validLLMBaseURL(c.Endpoint) {
		return field("similarity.endpoint", errors.New("must be an HTTPS endpoint with a valid non-local host and no credentials, query, fragment or ambiguous path"))
	}
	if len(c.APIKeyEnv) > 256 || !llmEnvName.MatchString(c.APIKeyEnv) {
		return field("similarity.api_key_env", errors.New("must be an environment variable name of at most 256 bytes"))
	}
	for _, item := range []struct{ name, value string }{{"provider_identity", c.ProviderIdentity}, {"service_identity", c.ServiceIdentity}, {"policy_ref", c.PolicyRef}} {
		if !validLLMText(item.value, 256) {
			return field("similarity."+item.name, errors.New("must be bounded non-empty UTF-8 text without controls or interpolation"))
		}
	}
	if c.Timeout <= 0 || c.Timeout > 10*time.Minute {
		return field("similarity.timeout", errors.New("must be positive and at most 10m"))
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > 64<<20 {
		return field("similarity.max_response_bytes", errors.New("must be between 1 and 67108864"))
	}
	if c.Limit <= 0 || c.Limit > 10000 {
		return field("similarity.limit", errors.New("must be between 1 and 10000"))
	}
	if math.IsNaN(c.AcceptanceThreshold) || math.IsInf(c.AcceptanceThreshold, 0) || c.AcceptanceThreshold < 0 || c.AcceptanceThreshold > 1 || c.AcceptanceThreshold > c.RejectionThreshold {
		return field("similarity.acceptance_threshold", errors.New("must be finite, between zero and one, and no greater than rejection_threshold"))
	}
	if math.IsNaN(c.RejectionThreshold) || math.IsInf(c.RejectionThreshold, 0) || c.RejectionThreshold < 0 || c.RejectionThreshold > 1 {
		return field("similarity.rejection_threshold", errors.New("must be finite and between zero and one"))
	}
	if c.MinimumHits <= 0 || c.MinimumHits > c.Limit {
		return field("similarity.minimum_hits", errors.New("must be positive and no greater than limit"))
	}
	return nil
}

func (c Config) validateWorkflowDependencies() error {
	if c.Sandbox != nil {
		if err := c.Sandbox.Validate(); err != nil {
			return err
		}
	}
	if c.Similarity != nil {
		if err := c.Similarity.Validate(); err != nil {
			return err
		}
	}
	if c.Workflow == nil {
		return nil
	}
	if err := c.Workflow.Validate(); err != nil {
		return err
	}
	if c.LLM == nil {
		return field("llm", errors.New("is required by the explicit live workflow"))
	}
	if c.Similarity == nil {
		return field("similarity", errors.New("is required by the explicit live workflow"))
	}
	if workflow.HasSolutionStages(c.Workflow.Revision) && c.Sandbox == nil {
		return field("sandbox", errors.New("is required by the forward Solution workflow"))
	}
	return nil
}

func effectiveWorkflow(c *WorkflowConfig) *WorkflowConfig {
	if c == nil {
		return nil
	}
	copy := *c
	return &copy
}

func effectiveSimilarity(c *SimilarityConfig) *EffectiveSimilarity {
	if c == nil {
		return nil
	}
	return &EffectiveSimilarity{c.Endpoint, c.APIKeyEnv, c.ProviderIdentity, c.ServiceIdentity, c.Timeout.String(), c.MaxResponseBytes, c.Limit, c.PolicyRef, c.AcceptanceThreshold, c.RejectionThreshold, c.MinimumHits}
}

func inspectWorkflowScalar(path, name string, node *yaml.Node) error {
	expected := "!!str"
	if name == "idea_count" || name == "llm_cost_upper_bound_micro_usd" || name == "similarity_cost_upper_bound_micro_usd" || name == "max_response_bytes" || name == "limit" || name == "minimum_hits" {
		expected = "!!int"
	}
	if path == "similarity" && (name == "acceptance_threshold" || name == "rejection_threshold") {
		if node.Tag != "!!int" && node.Tag != "!!float" {
			return errors.New("must be a numeric scalar")
		}
		var value float64
		if err := node.Decode(&value); err != nil {
			return errors.New("must be a valid number")
		}
		return nil
	}
	if node.Tag != expected {
		return errors.New("incorrect scalar type")
	}
	if expected == "!!int" {
		var value int64
		if err := node.Decode(&value); err != nil {
			return errors.New("must fit a signed 64-bit integer")
		}
	}
	return nil
}
