package application

import (
	"errors"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
)

// BuildSimilarityConfig maps credential references and the explicitly frozen
// decision policy without constructing a run or performing network access.
func BuildSimilarityConfig(cfg config.Config) (similarity.Config, similarity.DecisionPolicy, error) {
	if cfg.Similarity == nil {
		return similarity.Config{}, similarity.DecisionPolicy{}, &config.FieldError{Field: "similarity", Err: errors.New("provider configuration is required for explicit similarity assembly")}
	}
	provider := *cfg.Similarity
	if err := provider.Validate(); err != nil {
		return similarity.Config{}, similarity.DecisionPolicy{}, err
	}
	mapped := similarity.Config{Endpoint: provider.Endpoint, APIKeyEnv: provider.APIKeyEnv, ProviderIdentity: provider.ProviderIdentity, ServiceIdentity: provider.ServiceIdentity, Timeout: provider.Timeout, MaxResponseBytes: provider.MaxResponseBytes, MaxHits: provider.Limit, MaxAttempts: 1}
	if err := mapped.Validate(); err != nil {
		return similarity.Config{}, similarity.DecisionPolicy{}, err
	}
	policy, err := similarity.NewPolicy(provider.PolicyRef, provider.AcceptanceThreshold, provider.RejectionThreshold, provider.AcceptanceThreshold, provider.RejectionThreshold, provider.MinimumHits, nil)
	return mapped, policy, err
}

// BuildSlice2ExecutionSettings freezes application-owned content and transport
// rules. Adapter retries remain disabled; each of the two possible exchanges
// needs its own durable reservation/grant. Changing this compiled policy needs
// a compatible new workflow revision before it may reinterpret persisted runs.
func BuildSlice2ExecutionSettings(cfg config.Config) (GenerationReaderOptions, domain.RetryPolicy, error) {
	var content GenerationReaderOptions
	var retry domain.RetryPolicy
	if cfg.Workflow == nil {
		return content, retry, &config.FieldError{Field: "workflow", Err: errors.New("explicit live workflow selection is required")}
	}
	if err := cfg.Validate(); err != nil {
		return content, retry, err
	}
	_, output, err := BuildLLMConfig(cfg)
	if err != nil {
		return content, retry, err
	}
	digest := cfg.EffectiveDigest()
	if err := digest.Validate(); err != nil {
		return content, retry, err
	}
	content = GenerationReaderOptions{IdeaCount: cfg.Workflow.IdeaCount, SelectionPolicy: domain.SelectionOrdinalPolicyV1, StatementRevision: 1, ProviderPolicyDigest: digest, Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: output}
	retry = domain.RetryPolicy{MaxAttempts: 2, InitialBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second, JitterSeedDigest: domain.SumBytes([]byte("cpgen.slice2-transport/v1\x00" + string(digest)))}
	return content, retry, retry.Validate()
}
