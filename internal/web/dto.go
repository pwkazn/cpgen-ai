package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cpgen/internal/domain"
)

type createRequestBody struct {
	OperationKey string          `json:"operation_key"`
	Request      json.RawMessage `json:"request"`
}
type runRequestDTO struct {
	SchemaVersion         string    `json:"schema_version"`
	Mode                  string    `json:"mode"`
	Brief                 string    `json:"brief"`
	Tags                  []string  `json:"tags"`
	NormalizedTags        []string  `json:"normalized_tags"`
	Language              string    `json:"language"`
	Difficulty            string    `json:"difficulty"`
	RequiredFeatures      []string  `json:"required_features"`
	ForbiddenFeatures     []string  `json:"forbidden_features"`
	TimeLimitMilliseconds string    `json:"time_limit_milliseconds"`
	MemoryLimitMegabytes  string    `json:"memory_limit_megabytes"`
	SolutionLanguage      string    `json:"solution_language"`
	Seed                  *string   `json:"seed,omitempty"`
	VerificationProfile   string    `json:"verification_profile"`
	ExportTargets         []string  `json:"export_targets"`
	BudgetLimits          budgetDTO `json:"budget_limits"`
}
type budgetDTO struct {
	MaxLLMTokens              string `json:"max_llm_tokens,omitempty"`
	MaxLLMCalls               string `json:"max_llm_calls,omitempty"`
	MaxSimilarityCalls        string `json:"max_similarity_calls,omitempty"`
	MaxLLMInputTokens         string `json:"max_llm_input_tokens,omitempty"`
	MaxLLMOutputTokens        string `json:"max_llm_output_tokens,omitempty"`
	MaxLLMCostUSD             string `json:"max_llm_cost_usd,omitempty"`
	MaxSimilarityCostUSD      string `json:"max_similarity_cost_usd,omitempty"`
	MaxSandboxCreates         string `json:"max_sandbox_creates,omitempty"`
	MaxArtifactBytes          string `json:"max_artifact_bytes,omitempty"`
	MaxPackageBytes           string `json:"max_package_bytes,omitempty"`
	MaxMutationsPerStage      string `json:"max_mutations_per_stage,omitempty"`
	MaxActiveTimeMilliseconds string `json:"max_active_time_milliseconds,omitempty"`
	tokenFieldPresent         bool
	legacyFieldPresent        bool
	fields                    map[string]json.RawMessage
}

func (b *budgetDTO) UnmarshalJSON(raw []byte) error {
	type plainBudgetDTO budgetDTO
	var value plainBudgetDTO
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*b = budgetDTO(value)
	b.fields = fields
	_, b.tokenFieldPresent = fields["max_llm_tokens"]
	b.legacyFieldPresent = len(fields) > 0 && (!b.tokenFieldPresent || len(fields) > 1)
	return nil
}

func (b budgetDTO) usesTokenBudget() (bool, error) {
	tokenBudget := b.tokenFieldPresent || b.MaxLLMTokens != ""
	legacyBudget := b.legacyFieldPresent || b.MaxLLMCalls != "" || b.MaxSimilarityCalls != "" ||
		b.MaxLLMInputTokens != "" || b.MaxLLMOutputTokens != "" || b.MaxLLMCostUSD != "" ||
		b.MaxSimilarityCostUSD != "" || b.MaxSandboxCreates != "" || b.MaxArtifactBytes != "" ||
		b.MaxPackageBytes != "" || b.MaxMutationsPerStage != "" || b.MaxActiveTimeMilliseconds != ""
	if tokenBudget && legacyBudget {
		return false, errors.New("max_llm_tokens cannot be combined with legacy budget limits")
	}
	return tokenBudget, nil
}

func (b budgetDTO) usesSplitTokenBudget() bool {
	if b.fields != nil {
		if len(b.fields) == 0 {
			return false
		}
		for name := range b.fields {
			if name != "max_llm_input_tokens" && name != "max_llm_output_tokens" {
				return false
			}
		}
		return true
	}
	return b.MaxLLMTokens == "" && (b.MaxLLMInputTokens != "" || b.MaxLLMOutputTokens != "") &&
		b.MaxLLMCalls == "" && b.MaxSimilarityCalls == "" && b.MaxLLMCostUSD == "" &&
		b.MaxSimilarityCostUSD == "" && b.MaxSandboxCreates == "" && b.MaxArtifactBytes == "" &&
		b.MaxPackageBytes == "" && b.MaxMutationsPerStage == "" && b.MaxActiveTimeMilliseconds == ""
}

func decodeRunRequest(raw []byte) (domain.RunRequest, error) {
	var dto runRequestDTO
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		return domain.RunRequest{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return domain.RunRequest{}, errors.New("trailing JSON value")
	}
	parse := func(name, value string) (int64, error) {
		if value == "" {
			return 0, errors.New(name + " is required")
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, errors.New(name + " must be a signed decimal integer string")
		}
		return n, nil
	}
	var req domain.RunRequest
	req.SchemaVersion = dto.SchemaVersion
	req.Mode = dto.Mode
	req.Brief = dto.Brief
	req.Tags = dto.Tags
	req.NormalizedTags = dto.NormalizedTags
	req.Language = dto.Language
	req.Difficulty = dto.Difficulty
	req.RequiredFeatures = dto.RequiredFeatures
	req.ForbiddenFeatures = dto.ForbiddenFeatures
	req.SolutionLanguage = dto.SolutionLanguage
	req.VerificationProfile = dto.VerificationProfile
	req.ExportTargets = dto.ExportTargets
	var err error
	if req.TimeLimitMilliseconds, err = parse("time_limit_milliseconds", dto.TimeLimitMilliseconds); err != nil {
		return req, err
	}
	if req.MemoryLimitMegabytes, err = parse("memory_limit_megabytes", dto.MemoryLimitMegabytes); err != nil {
		return req, err
	}
	if dto.Seed != nil {
		n, e := parse("seed", *dto.Seed)
		if e != nil {
			return req, e
		}
		req.Seed = &n
	}
	b := dto.BudgetLimits
	tokenBudget, err := b.usesTokenBudget()
	if err != nil {
		return req, err
	}
	if tokenBudget {
		tokens, err := parse("max_llm_tokens", b.MaxLLMTokens)
		if err != nil {
			return req, err
		}
		req.BudgetLimits, err = domain.NewTokenBudgetLimits(tokens)
		return req, err
	}
	if b.usesSplitTokenBudget() {
		input, err := parse("max_llm_input_tokens", b.MaxLLMInputTokens)
		if err != nil {
			return req, err
		}
		output, err := parse("max_llm_output_tokens", b.MaxLLMOutputTokens)
		if err != nil {
			return req, err
		}
		req.BudgetLimits, err = domain.NewSplitTokenBudgetLimits(input, output)
		return req, err
	}
	limits := []struct {
		name, value string
		dst         *int64
		money       bool
	}{
		{"max_llm_calls", b.MaxLLMCalls, &req.BudgetLimits.MaxLLMCalls, false}, {"max_similarity_calls", b.MaxSimilarityCalls, &req.BudgetLimits.MaxSimilarityCalls, false}, {"max_llm_input_tokens", b.MaxLLMInputTokens, &req.BudgetLimits.MaxLLMInputTokens, false}, {"max_llm_output_tokens", b.MaxLLMOutputTokens, &req.BudgetLimits.MaxLLMOutputTokens, false},
		{"max_llm_cost_usd", b.MaxLLMCostUSD, &req.BudgetLimits.MaxLLMCostMicroUSD, true}, {"max_similarity_cost_usd", b.MaxSimilarityCostUSD, &req.BudgetLimits.MaxSimilarityCostMicroUSD, true}, {"max_sandbox_creates", b.MaxSandboxCreates, &req.BudgetLimits.MaxSandboxCreates, false}, {"max_artifact_bytes", b.MaxArtifactBytes, &req.BudgetLimits.MaxArtifactBytes, false}, {"max_package_bytes", b.MaxPackageBytes, &req.BudgetLimits.MaxPackageBytes, false}, {"max_mutations_per_stage", b.MaxMutationsPerStage, &req.BudgetLimits.MaxMutationsPerStage, false}, {"max_active_time_milliseconds", b.MaxActiveTimeMilliseconds, &req.BudgetLimits.MaxActiveTimeMilliseconds, false},
	}
	for _, item := range limits {
		if item.money {
			*item.dst, err = parseUSDmicro(item.value)
		} else {
			*item.dst, err = parse(item.name, item.value)
		}
		if err != nil {
			return req, err
		}
	}
	return req, nil
}

type runDTO struct {
	RunID                     domain.RunID                 `json:"run_id"`
	State                     domain.RunState              `json:"state"`
	Version                   string                       `json:"version"`
	WorkflowRevision          string                       `json:"workflow_revision"`
	SchemaVersion             domain.SchemaVersion         `json:"schema_version"`
	RequestDigest             domain.Digest                `json:"request_digest"`
	ConfigDigest              domain.Digest                `json:"config_digest"`
	WorkflowDigest            domain.Digest                `json:"workflow_digest"`
	CurrentStage              domain.StageName             `json:"current_stage"`
	CurrentStageOrdinal       string                       `json:"current_stage_ordinal"`
	CreatedAt                 string                       `json:"created_at"`
	UpdatedAt                 string                       `json:"updated_at"`
	ActiveElapsedNS           string                       `json:"active_elapsed_ns"`
	ActiveStartedAt           *string                      `json:"active_started_at,omitempty"`
	LastAccountingHeartbeatAt *string                      `json:"last_accounting_heartbeat_at,omitempty"`
	CancelSummary             string                       `json:"cancel_summary,omitempty"`
	FinalPackageOccurrenceID  *domain.ArtifactOccurrenceID `json:"final_package_occurrence_id,omitempty"`
}

func toRunDTO(r domain.RunSnapshot) runDTO {
	d := runDTO{RunID: r.RunID, State: r.State, Version: strconv.FormatInt(r.Version, 10), WorkflowRevision: r.WorkflowRevision, SchemaVersion: r.SchemaVersion, RequestDigest: r.RequestDigest, ConfigDigest: r.ConfigDigest, WorkflowDigest: r.WorkflowDigest, CurrentStage: r.CurrentStage, CurrentStageOrdinal: strconv.Itoa(r.CurrentStageOrdinal), CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339Nano), ActiveElapsedNS: strconv.FormatInt(int64(r.ActiveElapsed), 10), CancelSummary: r.CancelSummary, FinalPackageOccurrenceID: r.FinalPackageOccurrenceID}
	if r.ActiveStartedAt != nil {
		v := r.ActiveStartedAt.UTC().Format(time.RFC3339Nano)
		d.ActiveStartedAt = &v
	}
	if r.LastAccountingHeartbeatAt != nil {
		v := r.LastAccountingHeartbeatAt.UTC().Format(time.RFC3339Nano)
		d.LastAccountingHeartbeatAt = &v
	}
	return d
}

type runSummaryDTO struct {
	Brief        string           `json:"brief"`
	RunID        domain.RunID     `json:"run_id"`
	State        domain.RunState  `json:"state"`
	Version      string           `json:"version"`
	CurrentStage domain.StageName `json:"current_stage"`
	CreatedAt    string           `json:"created_at"`
	UpdatedAt    string           `json:"updated_at"`
}

func toRunSummaryDTO(r domain.RunSummary) runSummaryDTO {
	return runSummaryDTO{RunID: r.RunID, State: r.State, Version: strconv.FormatInt(r.Version, 10), CurrentStage: r.CurrentStage, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

type eventDTO struct {
	RunID          domain.RunID        `json:"run_id"`
	Version        string              `json:"version"`
	Type           domain.RunEventType `json:"type"`
	StageName      domain.StageName    `json:"stage_name,omitempty"`
	IdempotencyKey string              `json:"idempotency_key"`
	CommandDigest  domain.Digest       `json:"command_digest"`
	OccurredAt     string              `json:"occurred_at"`
}

func toEventDTO(e domain.RunEvent) eventDTO {
	return eventDTO{RunID: e.RunID, Version: strconv.FormatInt(e.Version, 10), Type: e.Type, StageName: e.StageName, IdempotencyKey: e.IdempotencyKey, CommandDigest: e.CommandDigest, OccurredAt: e.OccurredAt.UTC().Format(time.RFC3339Nano)}
}

func parseUSDmicro(value string) (int64, error) {
	if value == "" || strings.ContainsAny(value, "+-eE") {
		return 0, errors.New("USD budget must be a non-negative decimal string")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts) == 2 && len(parts[1]) > 6 {
		return 0, errors.New("USD budget supports at most six decimal places")
	}
	whole, ok := new(big.Int).SetString(parts[0], 10)
	if !ok || whole.Sign() < 0 {
		return 0, errors.New("USD budget is invalid")
	}
	whole.Mul(whole, big.NewInt(1_000_000))
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	for len(fraction) < 6 {
		fraction += "0"
	}
	if fraction != "" {
		minor, ok := new(big.Int).SetString(fraction, 10)
		if !ok {
			return 0, errors.New("USD budget is invalid")
		}
		whole.Add(whole, minor)
	}
	if !whole.IsInt64() {
		return 0, errors.New("USD budget overflows micro-USD")
	}
	return whole.Int64(), nil
}
