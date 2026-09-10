package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

type RunState string

const (
	RunCreated     RunState = "CREATED"
	RunRunning     RunState = "RUNNING"
	RunBlocked     RunState = "BLOCKED"
	RunNeedsReview RunState = "NEEDS_REVIEW"
	RunReady       RunState = "READY"
	RunFailed      RunState = "FAILED"
	RunCancelled   RunState = "CANCELLED"
)

func (v RunState) Valid() bool {
	switch v {
	case RunCreated, RunRunning, RunBlocked, RunNeedsReview, RunReady, RunFailed, RunCancelled:
		return true
	}
	return false
}
func (v *RunState) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "RunState", func(raw string) bool { return RunState(raw).Valid() }, (*string)(v))
}

type StageState string

const (
	StagePending     StageState = "PENDING"
	StageRunning     StageState = "RUNNING"
	StageSucceeded   StageState = "SUCCEEDED"
	StageBlocked     StageState = "BLOCKED"
	StageNeedsReview StageState = "NEEDS_REVIEW"
	StageFailed      StageState = "FAILED"
	StageCancelled   StageState = "CANCELLED"
)

func (v StageState) Valid() bool {
	switch v {
	case StagePending, StageRunning, StageSucceeded, StageBlocked, StageNeedsReview, StageFailed, StageCancelled:
		return true
	}
	return false
}
func (v *StageState) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "StageState", func(raw string) bool { return StageState(raw).Valid() }, (*string)(v))
}

type StageAttemptState string

const (
	StageAttemptRunning     StageAttemptState = "RUNNING"
	StageAttemptSucceeded   StageAttemptState = "SUCCEEDED"
	StageAttemptBlocked     StageAttemptState = "BLOCKED"
	StageAttemptNeedsReview StageAttemptState = "NEEDS_REVIEW"
	StageAttemptFailed      StageAttemptState = "FAILED"
	StageAttemptCancelled   StageAttemptState = "CANCELLED"
	StageAttemptInterrupted StageAttemptState = "INTERRUPTED"
)

func (v StageAttemptState) Valid() bool {
	switch v {
	case StageAttemptRunning, StageAttemptSucceeded, StageAttemptBlocked, StageAttemptNeedsReview, StageAttemptFailed, StageAttemptCancelled, StageAttemptInterrupted:
		return true
	}
	return false
}
func (v *StageAttemptState) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "StageAttemptState", func(raw string) bool { return StageAttemptState(raw).Valid() }, (*string)(v))
}

type ReviewDecisionKind string

const (
	ReviewRevise ReviewDecisionKind = "REVISE"
	ReviewRetry  ReviewDecisionKind = "RETRY"
	ReviewWaive  ReviewDecisionKind = "WAIVE"
	ReviewReject ReviewDecisionKind = "REJECT"
)

func (v ReviewDecisionKind) Valid() bool {
	return v == ReviewRevise || v == ReviewRetry || v == ReviewWaive || v == ReviewReject
}
func (v *ReviewDecisionKind) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ReviewDecisionKind", func(raw string) bool { return ReviewDecisionKind(raw).Valid() }, (*string)(v))
}

type ReviewDecisionState string

const (
	ReviewPending  ReviewDecisionState = "PENDING"
	ReviewApplied  ReviewDecisionState = "APPLIED"
	ReviewRejected ReviewDecisionState = "REJECTED"
	ReviewStale    ReviewDecisionState = "STALE"
)

func (v ReviewDecisionState) Valid() bool {
	return v == ReviewPending || v == ReviewApplied || v == ReviewRejected || v == ReviewStale
}
func (v *ReviewDecisionState) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ReviewDecisionState", func(raw string) bool { return ReviewDecisionState(raw).Valid() }, (*string)(v))
}

var ErrStageBoundary = errors.New("run READY is unavailable at a stage boundary")

// ValidateStageBoundary reserves READY for the later atomic package-verification transaction.
func ValidateStageBoundary(state RunState) error {
	if !state.Valid() {
		return fmt.Errorf("invalid run state %q", state)
	}
	if state == RunReady {
		return ErrStageBoundary
	}
	return nil
}

func ValidateRunTransition(from, to RunState) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("invalid run transition %q -> %q", from, to)
	}
	if to == RunReady {
		return ErrStageBoundary
	}
	legal := map[[2]RunState]struct{}{
		{RunCreated, RunRunning}: {}, {RunCreated, RunCancelled}: {},
		{RunRunning, RunCreated}: {}, {RunRunning, RunRunning}: {}, {RunRunning, RunBlocked}: {},
		{RunRunning, RunNeedsReview}: {}, {RunRunning, RunFailed}: {},
		{RunRunning, RunCancelled}: {}, {RunBlocked, RunRunning}: {},
		{RunBlocked, RunCancelled}: {}, {RunNeedsReview, RunCreated}: {},
		{RunNeedsReview, RunFailed}: {}, {RunNeedsReview, RunCancelled}: {},
	}
	if _, ok := legal[[2]RunState{from, to}]; !ok {
		return fmt.Errorf("illegal run transition %s -> %s", from, to)
	}
	return nil
}

func ValidateStageTransition(from, to StageState) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("invalid stage transition %q -> %q", from, to)
	}
	legal := map[[2]StageState]struct{}{
		{StagePending, StageRunning}: {}, {StagePending, StageCancelled}: {},
		{StageRunning, StagePending}: {}, {StageRunning, StageSucceeded}: {}, {StageRunning, StageBlocked}: {},
		{StageRunning, StageNeedsReview}: {}, {StageRunning, StageFailed}: {},
		{StageRunning, StageCancelled}: {}, {StageBlocked, StageRunning}: {},
		{StageBlocked, StageCancelled}: {}, {StageNeedsReview, StagePending}: {},
		{StageNeedsReview, StageFailed}: {}, {StageNeedsReview, StageCancelled}: {},
	}
	if _, ok := legal[[2]StageState{from, to}]; !ok {
		return fmt.Errorf("illegal stage transition %s -> %s", from, to)
	}
	return nil
}

type StageName string

var stageNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (v StageName) Validate() error {
	if !stageNamePattern.MatchString(string(v)) {
		return fmt.Errorf("invalid stage name %q", v)
	}
	return nil
}
func (v *StageName) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode stage name: %w", err)
	}
	parsed := StageName(raw)
	if err := parsed.Validate(); err != nil {
		return err
	}
	*v = parsed
	return nil
}

// BudgetLimits is deliberately integer-only so it can be persisted exactly.
type BudgetLimits struct {
	MaxLLMCalls               int64 `json:"max_llm_calls"`
	MaxSimilarityCalls        int64 `json:"max_similarity_calls"`
	MaxLLMInputTokens         int64 `json:"max_llm_input_tokens"`
	MaxLLMOutputTokens        int64 `json:"max_llm_output_tokens"`
	MaxLLMCostMicroUSD        int64 `json:"max_llm_cost_micro_usd"`
	MaxSimilarityCostMicroUSD int64 `json:"max_similarity_cost_micro_usd"`
	MaxSandboxCreates         int64 `json:"max_sandbox_creates"`
	MaxArtifactBytes          int64 `json:"max_artifact_bytes"`
	MaxPackageBytes           int64 `json:"max_package_bytes"`
	MaxMutationsPerStage      int64 `json:"max_mutations_per_stage"`
	MaxActiveTimeMilliseconds int64 `json:"max_active_time_milliseconds"`
}

func (v BudgetLimits) Validate() error {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"max llm calls", v.MaxLLMCalls}, {"max similarity calls", v.MaxSimilarityCalls},
		{"max llm input tokens", v.MaxLLMInputTokens}, {"max llm output tokens", v.MaxLLMOutputTokens},
		{"max llm cost micro USD", v.MaxLLMCostMicroUSD}, {"max similarity cost micro USD", v.MaxSimilarityCostMicroUSD},
		{"max sandbox creates", v.MaxSandboxCreates},
		{"max artifact bytes", v.MaxArtifactBytes}, {"max package bytes", v.MaxPackageBytes},
		{"max mutations per stage", v.MaxMutationsPerStage}, {"max active time milliseconds", v.MaxActiveTimeMilliseconds},
	} {
		if field.value < 0 {
			return fmt.Errorf("%s must not be negative", field.name)
		}
	}
	if v.MaxActiveTimeMilliseconds > math.MaxInt64/int64(time.Millisecond) {
		return errors.New("max active time milliseconds overflows nanoseconds")
	}
	return nil
}

// RunRequest is the immutable external generation request after strict decoding.
type RunRequest struct {
	SchemaVersion         string       `json:"schema_version"`
	Mode                  string       `json:"mode"`
	Brief                 string       `json:"brief"`
	Tags                  []string     `json:"tags"`
	NormalizedTags        []string     `json:"normalized_tags"`
	Language              string       `json:"language"`
	Difficulty            string       `json:"difficulty"`
	RequiredFeatures      []string     `json:"required_features"`
	ForbiddenFeatures     []string     `json:"forbidden_features"`
	TimeLimitMilliseconds int64        `json:"time_limit_milliseconds"`
	MemoryLimitMegabytes  int64        `json:"memory_limit_megabytes"`
	SolutionLanguage      string       `json:"solution_language"`
	Seed                  *int64       `json:"seed,omitempty"`
	VerificationProfile   string       `json:"verification_profile"`
	ExportTargets         []string     `json:"export_targets"`
	BudgetLimits          BudgetLimits `json:"budget_limits"`
}

func (v RunRequest) Validate() error {
	if strings.TrimSpace(v.SchemaVersion) == "" || strings.TrimSpace(v.Mode) == "" || strings.TrimSpace(v.Language) == "" || strings.TrimSpace(v.Difficulty) == "" || strings.TrimSpace(v.SolutionLanguage) == "" || strings.TrimSpace(v.VerificationProfile) == "" {
		return errors.New("run request has an empty required field")
	}
	// The admitted random/v1 mode can have no submitted hint. Preserve those
	// exact bytes through persistence instead of inserting a synthetic brief.
	if strings.TrimSpace(v.Brief) == "" && !(v.SchemaVersion == RequestSchemaV1 && v.Mode == RequestModeRandom) {
		return errors.New("run request brief is required outside random/v1 mode")
	}
	if v.TimeLimitMilliseconds <= 0 || v.MemoryLimitMegabytes <= 0 {
		return errors.New("run request limits must be positive")
	}
	if err := uniqueNonEmpty("tag", v.Tags); err != nil {
		return err
	}
	if err := uniqueNonEmpty("normalized tag", v.NormalizedTags); err != nil {
		return err
	}
	if !slices.IsSorted(v.NormalizedTags) {
		return errors.New("normalized tags must be sorted")
	}
	if err := uniqueNonEmpty("required feature", v.RequiredFeatures); err != nil {
		return err
	}
	if err := uniqueNonEmpty("forbidden feature", v.ForbiddenFeatures); err != nil {
		return err
	}
	if err := uniqueNonEmpty("export target", v.ExportTargets); err != nil {
		return err
	}
	return v.BudgetLimits.Validate()
}

func uniqueNonEmpty(kind string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is empty", kind)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate %s %q", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

type CreateRunRequest struct {
	RunID                         RunID         `json:"run_id"`
	SubmittedRequestJSON          []byte        `json:"submitted_request_json"`
	SubmittedRequestDigest        Digest        `json:"submitted_request_digest"`
	EffectiveSeed                 int64         `json:"effective_seed"`
	RedactedEffectiveConfigJSON   []byte        `json:"redacted_effective_config_json"`
	RedactedEffectiveConfigDigest Digest        `json:"redacted_effective_config_digest"`
	WorkflowRevision              string        `json:"workflow_revision"`
	SchemaVersion                 SchemaVersion `json:"schema_version"`
	WorkflowDigest                Digest        `json:"workflow_digest"`
	BudgetLimits                  BudgetLimits  `json:"budget_limits"`
	StageSequence                 []StageName   `json:"stage_sequence"`
	CreatedAt                     time.Time     `json:"created_at"`
	IdempotencyKey                string        `json:"idempotency_key"`
}

func (v CreateRunRequest) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := validateCanonicalDigest("submitted request", v.SubmittedRequestJSON, v.SubmittedRequestDigest); err != nil {
		return err
	}
	if err := validateCanonicalDigest("redacted effective config", v.RedactedEffectiveConfigJSON, v.RedactedEffectiveConfigDigest); err != nil {
		return err
	}
	if strings.TrimSpace(v.WorkflowRevision) == "" {
		return errors.New("workflow revision is empty")
	}
	if err := v.SchemaVersion.Validate(); err != nil {
		return fmt.Errorf("schema version: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(v.SubmittedRequestJSON))
	decoder.DisallowUnknownFields()
	var submitted RunRequest
	if err := decoder.Decode(&submitted); err != nil {
		return fmt.Errorf("decode submitted run request: %w", err)
	}
	if err := submitted.Validate(); err != nil {
		return fmt.Errorf("validate submitted run request: %w", err)
	}
	if SchemaVersion(submitted.SchemaVersion) != v.SchemaVersion {
		return errors.New("schema version does not match submitted request JSON")
	}
	if submitted.BudgetLimits != v.BudgetLimits {
		return errors.New("budget limits do not match submitted request JSON")
	}
	if err := v.WorkflowDigest.Validate(); err != nil {
		return fmt.Errorf("workflow digest: %w", err)
	}
	if err := v.BudgetLimits.Validate(); err != nil {
		return err
	}
	if len(v.StageSequence) == 0 {
		return errors.New("stage sequence is empty")
	}
	seen := map[StageName]struct{}{}
	for _, stage := range v.StageSequence {
		if err := stage.Validate(); err != nil {
			return err
		}
		if _, ok := seen[stage]; ok {
			return fmt.Errorf("duplicate stage %q", stage)
		}
		seen[stage] = struct{}{}
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	return ControlRequestID(v.IdempotencyKey).Validate()
}

func validateCanonicalDigest(name string, value []byte, digest Digest) error {
	if len(value) == 0 {
		return fmt.Errorf("%s JSON is empty", name)
	}
	if err := canonicalJSON(value); err != nil {
		return fmt.Errorf("%s JSON: %w", name, err)
	}
	if err := digest.Validate(); err != nil {
		return fmt.Errorf("%s digest: %w", name, err)
	}
	if SumBytes(value) != digest {
		return fmt.Errorf("%s digest does not match bytes", name)
	}
	return nil
}

func canonicalJSON(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("contains trailing JSON values")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	if !bytes.Equal(value, canonical) {
		return errors.New("is not canonical")
	}
	return nil
}

type RunSnapshot struct {
	RunID                     RunID                 `json:"run_id"`
	State                     RunState              `json:"state"`
	Version                   int64                 `json:"version"`
	WorkflowRevision          string                `json:"workflow_revision"`
	SchemaVersion             SchemaVersion         `json:"schema_version"`
	RequestDigest             Digest                `json:"request_digest"`
	ConfigDigest              Digest                `json:"config_digest"`
	WorkflowDigest            Digest                `json:"workflow_digest"`
	CurrentStage              StageName             `json:"current_stage"`
	CurrentStageOrdinal       int                   `json:"current_stage_ordinal"`
	CreatedAt                 time.Time             `json:"created_at"`
	UpdatedAt                 time.Time             `json:"updated_at"`
	ActiveElapsed             time.Duration         `json:"active_elapsed"`
	ActiveStartedAt           *time.Time            `json:"active_started_at,omitempty"`
	LastAccountingHeartbeatAt *time.Time            `json:"last_accounting_heartbeat_at,omitempty"`
	CancelSummary             string                `json:"cancel_summary,omitempty"`
	FinalPackageOccurrenceID  *ArtifactOccurrenceID `json:"final_package_occurrence_id,omitempty"`
}

func (v RunSnapshot) Validate() error {
	if (v.State == RunReady) != (v.FinalPackageOccurrenceID != nil) {
		return errors.New("READY requires its verified package occurrence")
	}
	if v.FinalPackageOccurrenceID != nil {
		if err := v.FinalPackageOccurrenceID.Validate(); err != nil {
			return err
		}
	}
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if !v.State.Valid() {
		return fmt.Errorf("invalid run state %q", v.State)
	}
	if v.Version <= 0 {
		return errors.New("run version must be positive")
	}
	if strings.TrimSpace(v.WorkflowRevision) == "" {
		return errors.New("run workflow revision is empty")
	}
	if err := v.SchemaVersion.Validate(); err != nil {
		return err
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	if err := validateUTCTime("updated at", v.UpdatedAt); err != nil {
		return err
	}
	if v.UpdatedAt.Before(v.CreatedAt) {
		return errors.New("updated at precedes created at")
	}
	if v.ActiveElapsed < 0 || (v.ActiveStartedAt == nil) != (v.LastAccountingHeartbeatAt == nil) {
		return errors.New("run active-time fields are invalid")
	}
	if v.ActiveStartedAt != nil && v.State != RunRunning {
		return errors.New("only a RUNNING run may have an active-time interval")
	}
	if v.ActiveStartedAt != nil {
		if err := validateUTCTime("active started at", *v.ActiveStartedAt); err != nil {
			return err
		}
		if err := validateUTCTime("last accounting heartbeat at", *v.LastAccountingHeartbeatAt); err != nil {
			return err
		}
		if v.LastAccountingHeartbeatAt.Before(*v.ActiveStartedAt) {
			return errors.New("last accounting heartbeat precedes active start")
		}
	}
	if (v.State == RunCancelled) != (strings.TrimSpace(v.CancelSummary) != "") {
		return errors.New("run cancellation summary does not match state")
	}
	for _, binding := range []struct {
		name   string
		digest Digest
	}{
		{"request digest", v.RequestDigest},
		{"config digest", v.ConfigDigest},
		{"workflow digest", v.WorkflowDigest},
	} {
		if err := binding.digest.Validate(); err != nil {
			return fmt.Errorf("%s: %w", binding.name, err)
		}
	}
	if v.CurrentStage != "" {
		if err := v.CurrentStage.Validate(); err != nil {
			return err
		}
		if v.CurrentStageOrdinal <= 0 {
			return errors.New("current stage ordinal must be positive")
		}
	}
	return nil
}

type StageSnapshot struct {
	RunID                 RunID      `json:"run_id"`
	Name                  StageName  `json:"name"`
	Ordinal               int        `json:"ordinal"`
	State                 StageState `json:"state"`
	Version               int64      `json:"version"`
	InputDigest           Digest     `json:"input_digest"`
	OutputDigest          *Digest    `json:"output_digest,omitempty"`
	AttemptCount          int        `json:"attempt_count"`
	CurrentAttemptID      *AttemptID `json:"current_attempt_id,omitempty"`
	LogicalIdempotencyKey string     `json:"logical_idempotency_key"`
	ReviewEvidenceDigest  *Digest    `json:"review_evidence_digest,omitempty"`
	ReviewPolicyDigest    *Digest    `json:"review_policy_digest,omitempty"`
	ReviewWaivable        *bool      `json:"review_waivable,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

func (v StageSnapshot) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.Name.Validate(); err != nil {
		return err
	}
	if v.Ordinal <= 0 || v.Version <= 0 || v.AttemptCount < 0 {
		return errors.New("stage ordinal, version, and attempt count are invalid")
	}
	if !v.State.Valid() {
		return fmt.Errorf("invalid stage state %q", v.State)
	}
	if err := v.InputDigest.Validate(); err != nil {
		return err
	}
	if v.OutputDigest != nil {
		if err := v.OutputDigest.Validate(); err != nil {
			return err
		}
	}
	for _, digest := range []*Digest{v.ReviewEvidenceDigest, v.ReviewPolicyDigest} {
		if digest != nil {
			if err := digest.Validate(); err != nil {
				return err
			}
		}
	}
	if v.CurrentAttemptID != nil {
		if err := v.CurrentAttemptID.Validate(); err != nil {
			return err
		}
	}
	if err := validateStageStateFields(v); err != nil {
		return err
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	if err := validateUTCTime("updated at", v.UpdatedAt); err != nil {
		return err
	}
	return nil
}

type StageAttempt struct {
	AttemptID    AttemptID         `json:"attempt_id"`
	RunID        RunID             `json:"run_id"`
	StageName    StageName         `json:"stage_name"`
	Ordinal      int               `json:"ordinal"`
	State        StageAttemptState `json:"state"`
	InputDigest  Digest            `json:"input_digest"`
	OutputDigest *Digest           `json:"output_digest,omitempty"`
	Cause        *ExecutionCause   `json:"cause,omitempty"`
	// BlockedBinding is retained for blocked attempts so resume can revalidate
	// the exact dependency and policy that produced the checkpoint.
	BlockedBinding *BlockedCheckpoint `json:"blocked_binding,omitempty"`
	StartedAt      time.Time          `json:"started_at"`
	FinishedAt     *time.Time         `json:"finished_at,omitempty"`
}

func (v StageAttempt) Validate() error {
	if err := v.AttemptID.Validate(); err != nil {
		return err
	}
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if v.Ordinal <= 0 {
		return errors.New("attempt ordinal must be positive")
	}
	if !v.State.Valid() {
		return fmt.Errorf("invalid attempt state %q", v.State)
	}
	if err := v.InputDigest.Validate(); err != nil {
		return fmt.Errorf("input digest: %w", err)
	}
	if v.OutputDigest != nil {
		if err := v.OutputDigest.Validate(); err != nil {
			return err
		}
	}
	if v.Cause != nil && !v.Cause.Valid() {
		return fmt.Errorf("invalid execution cause %q", *v.Cause)
	}
	if v.BlockedBinding != nil {
		if v.State != StageAttemptBlocked || v.BlockedBinding.RunID != v.RunID || v.BlockedBinding.StageName != v.StageName || v.BlockedBinding.StageInputDigest != v.InputDigest {
			return errors.New("blocked binding does not match attempt")
		}
		if err := v.BlockedBinding.Validate(); err != nil {
			return fmt.Errorf("blocked binding: %w", err)
		}
	}
	if err := validateUTCTime("started at", v.StartedAt); err != nil {
		return err
	}
	if err := validateStageAttemptStateFields(v); err != nil {
		return err
	}
	if v.FinishedAt != nil {
		if err := validateUTCTime("finished at", *v.FinishedAt); err != nil {
			return err
		}
		if v.FinishedAt.Before(v.StartedAt) {
			return errors.New("attempt finished before it started")
		}
	}
	return nil
}

type BlockedCheckpoint struct {
	RunID            RunID     `json:"run_id"`
	StageName        StageName `json:"stage_name"`
	StageInputDigest Digest    `json:"stage_input_digest"`
	DependencyID     string    `json:"dependency_id"`
	DependencyDigest Digest    `json:"dependency_digest"`
	PolicyDigest     Digest    `json:"policy_digest"`
	ErrorDigest      Digest    `json:"error_digest"`
	RetryAfter       time.Time `json:"retry_after"`
	CreatedAt        time.Time `json:"created_at"`
}

func (v BlockedCheckpoint) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(v.DependencyID) == "" {
		return errors.New("dependency id is empty")
	}
	for _, digest := range []Digest{v.StageInputDigest, v.DependencyDigest, v.PolicyDigest, v.ErrorDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if err := validateUTCTime("retry after", v.RetryAfter); err != nil {
		return err
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	return nil
}

type ReviewDecision struct {
	ID                      ReviewDecisionID    `json:"id"`
	RunID                   RunID               `json:"run_id"`
	Kind                    ReviewDecisionKind  `json:"kind"`
	State                   ReviewDecisionState `json:"state"`
	ExpectedRunVersion      int64               `json:"expected_run_version"`
	RunVersion              int64               `json:"run_version"`
	WorkflowRevision        string              `json:"workflow_revision"`
	StageName               StageName           `json:"stage_name"`
	StageInputDigest        Digest              `json:"stage_input_digest"`
	EvidenceDigest          Digest              `json:"evidence_digest"`
	PolicyDigest            Digest              `json:"policy_digest"`
	RequestedEditsDigest    *Digest             `json:"requested_edits_digest,omitempty"`
	WaiverScopeDigest       *Digest             `json:"waiver_scope_digest,omitempty"`
	ExternalConditionDigest *Digest             `json:"external_condition_digest,omitempty"`
	BudgetIncrease          BudgetLimits        `json:"budget_increase"`
	WaivableGate            bool                `json:"waivable_gate"`
	Reviewer                string              `json:"reviewer"`
	Reason                  string              `json:"reason"`
	CreatedAt               time.Time           `json:"created_at"`
	AppliedAt               *time.Time          `json:"applied_at,omitempty"`
}

func (v ReviewDecision) Validate() error {
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if !v.Kind.Valid() || !v.State.Valid() {
		return errors.New("invalid review kind or state")
	}
	if v.ExpectedRunVersion <= 0 || v.RunVersion <= 0 || strings.TrimSpace(v.WorkflowRevision) == "" {
		return errors.New("invalid review version binding")
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	for _, digest := range []Digest{v.StageInputDigest, v.EvidenceDigest, v.PolicyDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if v.RequestedEditsDigest != nil {
		if err := v.RequestedEditsDigest.Validate(); err != nil {
			return err
		}
	}
	if v.WaiverScopeDigest != nil {
		if err := v.WaiverScopeDigest.Validate(); err != nil {
			return err
		}
	}
	if v.ExternalConditionDigest != nil {
		if err := v.ExternalConditionDigest.Validate(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(v.Reviewer) == "" || strings.TrimSpace(v.Reason) == "" {
		return errors.New("reviewer and reason are required")
	}
	if err := validateReviewDecisionPayload(v.Kind, v.RequestedEditsDigest, v.WaiverScopeDigest, v.ExternalConditionDigest, v.BudgetIncrease, v.WaivableGate); err != nil {
		return err
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	if v.State == ReviewPending && v.AppliedAt != nil {
		return errors.New("pending review has application time")
	}
	if v.State != ReviewPending && v.AppliedAt == nil {
		return errors.New("completed review has no application time")
	}
	if v.AppliedAt != nil {
		if err := validateUTCTime("applied at", *v.AppliedAt); err != nil {
			return err
		}
		if v.AppliedAt.Before(v.CreatedAt) {
			return errors.New("review applied before creation")
		}
	}
	return nil
}

type RunFilter struct {
	State *RunState `json:"state,omitempty"`
	Limit int       `json:"limit,omitempty"`
}

func (v RunFilter) Validate() error {
	if v.State != nil && !v.State.Valid() {
		return fmt.Errorf("invalid run filter state %q", *v.State)
	}
	if v.Limit < 0 || v.Limit > 1000 {
		return errors.New("run filter limit must be between zero and 1000")
	}
	return nil
}

type RunSummary struct {
	RunID        RunID     `json:"run_id"`
	State        RunState  `json:"state"`
	Version      int64     `json:"version"`
	CurrentStage StageName `json:"current_stage"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (v RunSummary) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if !v.State.Valid() || v.Version <= 0 {
		return errors.New("run summary state or version is invalid")
	}
	if err := v.CurrentStage.Validate(); err != nil {
		return err
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	if err := validateUTCTime("updated at", v.UpdatedAt); err != nil {
		return err
	}
	return nil
}

type RunEventType string

const (
	EventRunCreated          RunEventType = "RUN_CREATED"
	EventStageBegan          RunEventType = "STAGE_BEGAN"
	EventStageFinished       RunEventType = "STAGE_FINISHED"
	EventStageInterrupted    RunEventType = "STAGE_INTERRUPTED"
	EventCancelRequested     RunEventType = "CANCEL_REQUESTED"
	EventCancelFinalized     RunEventType = "CANCEL_FINALIZED"
	EventActiveTimeAccounted RunEventType = "ACTIVE_TIME_ACCOUNTED"
	EventReviewCreated       RunEventType = "REVIEW_CREATED"
	EventReviewApplied       RunEventType = "REVIEW_APPLIED"
)

func (v RunEventType) Valid() bool {
	switch v {
	case EventRunCreated, EventStageBegan, EventStageFinished, EventStageInterrupted,
		EventCancelRequested, EventCancelFinalized, EventActiveTimeAccounted, EventReviewCreated, EventReviewApplied:
		return true
	}
	return false
}

type RunEvent struct {
	RunID          RunID        `json:"run_id"`
	Version        int64        `json:"version"`
	Type           RunEventType `json:"type"`
	StageName      StageName    `json:"stage_name,omitempty"`
	IdempotencyKey string       `json:"idempotency_key"`
	CommandDigest  Digest       `json:"command_digest"`
	OccurredAt     time.Time    `json:"occurred_at"`
}

func (v RunEvent) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if v.Version <= 0 || !v.Type.Valid() {
		return errors.New("event version or type is invalid")
	}
	if v.StageName != "" {
		if err := v.StageName.Validate(); err != nil {
			return err
		}
	}
	if err := validateIdempotencyKey(v.IdempotencyKey); err != nil {
		return err
	}
	if err := v.CommandDigest.Validate(); err != nil {
		return err
	}
	return validateUTCTime("occurred at", v.OccurredAt)
}

type BeginStageCommand struct {
	RunID              RunID     `json:"run_id"`
	ExpectedRunVersion int64     `json:"expected_run_version"`
	StageName          StageName `json:"stage_name"`
	AttemptID          AttemptID `json:"attempt_id"`
	InputDigest        Digest    `json:"input_digest"`
	IdempotencyKey     string    `json:"idempotency_key"`
	At                 time.Time `json:"at"`
}

func (v BeginStageCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if err := v.AttemptID.Validate(); err != nil {
		return err
	}
	return v.InputDigest.Validate()
}

type FinishStageCommand struct {
	RunID                RunID             `json:"run_id"`
	ExpectedRunVersion   int64             `json:"expected_run_version"`
	StageName            StageName         `json:"stage_name"`
	AttemptID            AttemptID         `json:"attempt_id"`
	AttemptState         StageAttemptState `json:"attempt_state"`
	RunState             RunState          `json:"run_state"`
	OutputDigest         *Digest           `json:"output_digest,omitempty"`
	NextStage            StageName         `json:"next_stage,omitempty"`
	NextInputDigest      *Digest           `json:"next_input_digest,omitempty"`
	ReviewEvidenceDigest *Digest           `json:"review_evidence_digest,omitempty"`
	ReviewPolicyDigest   *Digest           `json:"review_policy_digest,omitempty"`
	ReviewGateWaivable   bool              `json:"review_gate_waivable,omitempty"`
	// BlockedBinding preserves the exact dependency and policy checkpoint that
	// caused a stage to block. It is optional only for legacy callers; new
	// blocked outcomes must carry it so resume can revalidate the same input.
	BlockedBinding *BlockedCheckpoint  `json:"blocked_binding,omitempty"`
	Cause          *ExecutionCause     `json:"cause,omitempty"`
	Occurrences    []PendingOccurrence `json:"occurrences,omitempty"`
	IdempotencyKey string              `json:"idempotency_key"`
	At             time.Time           `json:"at"`
}

func (v FinishStageCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if err := v.AttemptID.Validate(); err != nil {
		return err
	}
	if !v.AttemptState.Valid() || v.AttemptState == StageAttemptRunning || v.AttemptState == StageAttemptInterrupted {
		return errors.New("finish attempt state is invalid")
	}
	if err := ValidateStageBoundary(v.RunState); err != nil {
		return err
	}
	if v.OutputDigest != nil {
		if err := v.OutputDigest.Validate(); err != nil {
			return err
		}
	}
	if v.NextInputDigest != nil {
		if err := v.NextInputDigest.Validate(); err != nil {
			return err
		}
	}
	for _, digest := range []*Digest{v.ReviewEvidenceDigest, v.ReviewPolicyDigest} {
		if digest != nil {
			if err := digest.Validate(); err != nil {
				return err
			}
		}
	}
	for index, occurrence := range v.Occurrences {
		if err := occurrence.Validate(); err != nil {
			return fmt.Errorf("occurrence %d: %w", index, err)
		}
	}
	switch v.AttemptState {
	case StageAttemptSucceeded:
		if v.RunState != RunRunning || v.OutputDigest == nil || v.NextStage == "" || v.NextInputDigest == nil ||
			v.ReviewEvidenceDigest != nil || v.ReviewPolicyDigest != nil || v.ReviewGateWaivable || v.BlockedBinding != nil || v.Cause != nil {
			return errors.New("successful finish requires RUNNING next-stage binding")
		}
		if err := v.NextStage.Validate(); err != nil {
			return err
		}
	case StageAttemptBlocked:
		if v.RunState != RunBlocked || hasSuccessOnlyFinishFields(v) || hasReviewFinishFields(v) || v.Cause != nil || len(v.Occurrences) != 0 {
			return errors.New("blocked finish fields are invalid")
		}
		if v.BlockedBinding != nil {
			if v.BlockedBinding.RunID != v.RunID || v.BlockedBinding.StageName != v.StageName {
				return errors.New("blocked finish binding does not match run or stage")
			}
			if err := v.BlockedBinding.Validate(); err != nil {
				return fmt.Errorf("blocked finish binding: %w", err)
			}
		}
	case StageAttemptNeedsReview:
		if v.RunState != RunNeedsReview || hasSuccessOnlyFinishFields(v) || v.ReviewEvidenceDigest == nil || v.ReviewPolicyDigest == nil || v.Cause != nil || len(v.Occurrences) != 0 || v.BlockedBinding != nil {
			return errors.New("review finish fields are invalid")
		}
	case StageAttemptFailed:
		if v.RunState != RunFailed || hasSuccessOnlyFinishFields(v) || hasReviewFinishFields(v) || v.Cause != nil || len(v.Occurrences) != 0 || v.BlockedBinding != nil {
			return errors.New("failed finish fields are invalid")
		}
	case StageAttemptCancelled:
		if v.RunState != RunCancelled || hasSuccessOnlyFinishFields(v) || hasReviewFinishFields(v) || v.Cause == nil || *v.Cause != CauseUserCancel || len(v.Occurrences) != 0 || v.BlockedBinding != nil {
			return errors.New("cancelled finish fields are invalid")
		}
	}
	return nil
}

func hasSuccessOnlyFinishFields(v FinishStageCommand) bool {
	return v.OutputDigest != nil || v.NextStage != "" || v.NextInputDigest != nil
}

func hasReviewFinishFields(v FinishStageCommand) bool {
	return v.ReviewEvidenceDigest != nil || v.ReviewPolicyDigest != nil || v.ReviewGateWaivable
}

// ValidateAgainst binds the command timestamp to the persisted attempt and
// current projection timestamps that the store rereads under its write lock.
func (v FinishStageCommand) ValidateAgainst(attemptStartedAt, currentUpdatedAt time.Time) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if err := validateUTCTime("attempt started at", attemptStartedAt); err != nil {
		return err
	}
	if err := validateUTCTime("current projection updated at", currentUpdatedAt); err != nil {
		return err
	}
	if v.At.Before(attemptStartedAt) {
		return errors.New("finish time precedes attempt start")
	}
	if v.At.Before(currentUpdatedAt) {
		return errors.New("finish time precedes current projection update")
	}
	return nil
}

type InterruptStageCommand struct {
	RunID              RunID          `json:"run_id"`
	ExpectedRunVersion int64          `json:"expected_run_version"`
	StageName          StageName      `json:"stage_name"`
	AttemptID          AttemptID      `json:"attempt_id"`
	Cause              ExecutionCause `json:"cause"`
	IdempotencyKey     string         `json:"idempotency_key"`
	At                 time.Time      `json:"at"`
}

func (v InterruptStageCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if err := v.AttemptID.Validate(); err != nil {
		return err
	}
	if !v.Cause.Valid() {
		return fmt.Errorf("invalid interruption cause %q", v.Cause)
	}
	return nil
}

type CancelRequest struct {
	ID                 ControlRequestID `json:"id"`
	RunID              RunID            `json:"run_id"`
	ExpectedRunVersion int64            `json:"expected_run_version"`
	Reason             string           `json:"reason"`
	IdempotencyKey     string           `json:"idempotency_key"`
	At                 time.Time        `json:"at"`
}

// FinalizeCancelCommand is the named projection transition invoked only after
// the caller has completed its later exact-resource reconciliation boundary.
type FinalizeCancelCommand struct {
	RunID                RunID            `json:"run_id"`
	ExpectedRunVersion   int64            `json:"expected_run_version"`
	ControlRequestID     ControlRequestID `json:"control_request_id"`
	ReconciliationDigest Digest           `json:"reconciliation_digest"`
	IdempotencyKey       string           `json:"idempotency_key"`
	At                   time.Time        `json:"at"`
}

func (v FinalizeCancelCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.ControlRequestID.Validate(); err != nil {
		return err
	}
	return v.ReconciliationDigest.Validate()
}

func (v CancelRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(v.Reason) == "" {
		return errors.New("cancel reason is empty")
	}
	return nil
}

type ControlRequest struct {
	ID         ControlRequestID `json:"id"`
	RunID      RunID            `json:"run_id"`
	Reason     string           `json:"reason"`
	Active     bool             `json:"active"`
	RunVersion int64            `json:"run_version"`
	CreatedAt  time.Time        `json:"created_at"`
	AppliedAt  *time.Time       `json:"applied_at,omitempty"`
}

func (v ControlRequest) Validate() error {
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(v.Reason) == "" || v.RunVersion <= 0 {
		return errors.New("control request reason or version is invalid")
	}
	if err := validateUTCTime("created at", v.CreatedAt); err != nil {
		return err
	}
	if v.Active != (v.AppliedAt == nil) {
		return errors.New("control request active state is invalid")
	}
	return nil
}

type ActiveTimeAction string

const (
	ActiveTimeStart     ActiveTimeAction = "START"
	ActiveTimeHeartbeat ActiveTimeAction = "HEARTBEAT"
	ActiveTimeStop      ActiveTimeAction = "STOP"
	ActiveTimeRecover   ActiveTimeAction = "RECOVER"
)

func (v ActiveTimeAction) Valid() bool {
	return v == ActiveTimeStart || v == ActiveTimeHeartbeat || v == ActiveTimeStop || v == ActiveTimeRecover
}

type ActiveTimeCommand struct {
	RunID              RunID            `json:"run_id"`
	ExpectedRunVersion int64            `json:"expected_run_version"`
	Action             ActiveTimeAction `json:"action"`
	HeartbeatInterval  time.Duration    `json:"heartbeat_interval,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key"`
	At                 time.Time        `json:"at"`
}

func (v ActiveTimeCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if !v.Action.Valid() {
		return errors.New("active-time action is invalid")
	}
	if v.Action == ActiveTimeRecover {
		if v.HeartbeatInterval <= 0 {
			return errors.New("recovery heartbeat interval must be positive")
		}
	} else if v.HeartbeatInterval != 0 {
		return errors.New("heartbeat interval is recovery-only")
	}
	return nil
}

type ActiveTimeResult struct {
	RunID         RunID         `json:"run_id"`
	RunVersion    int64         `json:"run_version"`
	ActiveElapsed time.Duration `json:"active_elapsed"`
	Remaining     time.Duration `json:"remaining"`
	Deadline      time.Time     `json:"deadline"`
	Active        bool          `json:"active"`
	Exhausted     bool          `json:"exhausted"`
}

func (v ActiveTimeResult) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if v.RunVersion <= 0 || v.ActiveElapsed < 0 || v.Remaining < 0 || v.Exhausted != (v.Remaining == 0) {
		return errors.New("active-time result is invalid")
	}
	return validateUTCTime("deadline", v.Deadline)
}

type CreateReviewRequest struct {
	ID                      ReviewDecisionID   `json:"id"`
	RunID                   RunID              `json:"run_id"`
	ExpectedRunVersion      int64              `json:"expected_run_version"`
	Kind                    ReviewDecisionKind `json:"kind"`
	WorkflowRevision        string             `json:"workflow_revision"`
	StageName               StageName          `json:"stage_name"`
	StageInputDigest        Digest             `json:"stage_input_digest"`
	EvidenceDigest          Digest             `json:"evidence_digest"`
	PolicyDigest            Digest             `json:"policy_digest"`
	RequestedEditsDigest    *Digest            `json:"requested_edits_digest,omitempty"`
	WaiverScopeDigest       *Digest            `json:"waiver_scope_digest,omitempty"`
	ExternalConditionDigest *Digest            `json:"external_condition_digest,omitempty"`
	BudgetIncrease          BudgetLimits       `json:"budget_increase"`
	Reviewer                string             `json:"reviewer"`
	Reason                  string             `json:"reason"`
	IdempotencyKey          string             `json:"idempotency_key"`
	At                      time.Time          `json:"at"`
}

func (v CreateReviewRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if !v.Kind.Valid() || strings.TrimSpace(v.WorkflowRevision) == "" || strings.TrimSpace(v.Reviewer) == "" || strings.TrimSpace(v.Reason) == "" {
		return errors.New("review kind, revision, reviewer, or reason is invalid")
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	for _, digest := range []Digest{v.StageInputDigest, v.EvidenceDigest, v.PolicyDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	for _, digest := range []*Digest{v.RequestedEditsDigest, v.WaiverScopeDigest, v.ExternalConditionDigest} {
		if digest != nil {
			if err := digest.Validate(); err != nil {
				return err
			}
		}
	}
	return validateReviewRequestPayload(v.Kind, v.RequestedEditsDigest, v.WaiverScopeDigest, v.ExternalConditionDigest, v.BudgetIncrease)
}

type ApplyReviewCommand struct {
	RunID              RunID            `json:"run_id"`
	ExpectedRunVersion int64            `json:"expected_run_version"`
	ReviewDecisionID   ReviewDecisionID `json:"review_decision_id"`
	StageName          StageName        `json:"stage_name"`
	StageInputDigest   Digest           `json:"stage_input_digest"`
	EvidenceDigest     Digest           `json:"evidence_digest"`
	PolicyDigest       Digest           `json:"policy_digest"`
	NewInputDigest     *Digest          `json:"new_input_digest,omitempty"`
	NewConfigJSON      []byte           `json:"new_config_json,omitempty"`
	NewConfigDigest    *Digest          `json:"new_config_digest,omitempty"`
	InvalidatedStages  []StageName      `json:"invalidated_stages,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key"`
	At                 time.Time        `json:"at"`
}

func (v ApplyReviewCommand) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	if err := v.ReviewDecisionID.Validate(); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	for _, digest := range []Digest{v.StageInputDigest, v.EvidenceDigest, v.PolicyDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if (v.NewInputDigest == nil) != (v.NewConfigDigest == nil) || (v.NewConfigDigest == nil) != (len(v.NewConfigJSON) == 0) {
		return errors.New("revised input and canonical config binding must be supplied together")
	}
	if v.NewInputDigest != nil {
		if err := v.NewInputDigest.Validate(); err != nil {
			return err
		}
		if err := validateCanonicalDigest("revised redacted effective config", v.NewConfigJSON, *v.NewConfigDigest); err != nil {
			return err
		}
		if len(v.InvalidatedStages) == 0 {
			return errors.New("revision must invalidate stages")
		}
	}
	seen := map[StageName]struct{}{}
	for _, stage := range v.InvalidatedStages {
		if err := stage.Validate(); err != nil {
			return err
		}
		if _, ok := seen[stage]; ok {
			return fmt.Errorf("duplicate invalidated stage %q", stage)
		}
		seen[stage] = struct{}{}
	}
	if v.NewInputDigest != nil {
		if _, ok := seen[v.StageName]; !ok {
			return errors.New("revision must invalidate the current stage")
		}
	}
	return nil
}

func validateReviewRequestPayload(kind ReviewDecisionKind, edits, waiver, condition *Digest, budget BudgetLimits) error {
	if err := budget.Validate(); err != nil {
		return err
	}
	hasBudget := budget != (BudgetLimits{})
	switch kind {
	case ReviewRevise:
		if edits == nil || waiver != nil || condition != nil || hasBudget {
			return errors.New("REVISE payload is invalid")
		}
	case ReviewRetry:
		if edits != nil || waiver != nil || (!hasBudget && condition == nil) {
			return errors.New("RETRY requires a positive budget increase or external condition")
		}
		if hasBudget && !budgetIncreasePositive(budget) {
			return errors.New("RETRY budget increase must be positive")
		}
	case ReviewWaive:
		if edits != nil || waiver == nil || condition != nil || hasBudget {
			return errors.New("WAIVE requires a scope")
		}
	case ReviewReject:
		if edits != nil || waiver != nil || condition != nil || hasBudget {
			return errors.New("REJECT payload is invalid")
		}
	default:
		return errors.New("review kind is invalid")
	}
	return nil
}

func validateReviewDecisionPayload(kind ReviewDecisionKind, edits, waiver, condition *Digest, budget BudgetLimits, waivable bool) error {
	if err := validateReviewRequestPayload(kind, edits, waiver, condition, budget); err != nil {
		return err
	}
	if kind == ReviewWaive && !waivable {
		return errors.New("WAIVE decision is not bound to a waivable gate")
	}
	if kind != ReviewWaive && waivable {
		return errors.New("non-WAIVE decision carries a waivable-gate assertion")
	}
	return nil
}

func budgetIncreasePositive(value BudgetLimits) bool {
	return value.MaxLLMCalls > 0 || value.MaxSimilarityCalls > 0 || value.MaxLLMInputTokens > 0 ||
		value.MaxLLMOutputTokens > 0 || value.MaxLLMCostMicroUSD > 0 || value.MaxSimilarityCostMicroUSD > 0 || value.MaxSandboxCreates > 0 ||
		value.MaxArtifactBytes > 0 || value.MaxPackageBytes > 0 || value.MaxMutationsPerStage > 0 ||
		value.MaxActiveTimeMilliseconds > 0
}

func validateMutation(runID RunID, expectedVersion int64, idempotencyKey string, at time.Time) error {
	if err := runID.Validate(); err != nil {
		return err
	}
	if expectedVersion <= 0 {
		return errors.New("expected run version must be positive")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	return validateUTCTime("command time", at)
}

func validateIdempotencyKey(value string) error {
	return ControlRequestID(value).Validate()
}

func validateUTCTime(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be a canonical UTC timestamp", name)
	}
	return nil
}

func validateStageStateFields(v StageSnapshot) error {
	hasReviewBinding := v.ReviewEvidenceDigest != nil || v.ReviewPolicyDigest != nil || v.ReviewWaivable != nil
	switch v.State {
	case StagePending:
		if v.CurrentAttemptID != nil || v.OutputDigest != nil || hasReviewBinding {
			return errors.New("pending stage has execution fields")
		}
	case StageRunning:
		if v.AttemptCount <= 0 || v.CurrentAttemptID == nil || v.OutputDigest != nil || hasReviewBinding {
			return errors.New("running stage has invalid execution fields")
		}
	case StageSucceeded:
		if v.AttemptCount <= 0 || v.CurrentAttemptID != nil || v.OutputDigest == nil || hasReviewBinding {
			return errors.New("succeeded stage has invalid execution fields")
		}
	case StageBlocked, StageFailed:
		if v.AttemptCount <= 0 || v.CurrentAttemptID != nil || v.OutputDigest != nil || hasReviewBinding {
			return fmt.Errorf("%s stage has invalid execution fields", v.State)
		}
	case StageNeedsReview:
		if v.AttemptCount <= 0 || v.CurrentAttemptID != nil || v.OutputDigest != nil ||
			v.ReviewEvidenceDigest == nil || v.ReviewPolicyDigest == nil || v.ReviewWaivable == nil {
			return errors.New("NEEDS_REVIEW stage has invalid review binding")
		}
	case StageCancelled:
		if v.CurrentAttemptID != nil || v.OutputDigest != nil || hasReviewBinding {
			return errors.New("cancelled stage has invalid execution fields")
		}
	default:
		return fmt.Errorf("invalid stage state %q", v.State)
	}
	return nil
}

func validateStageAttemptStateFields(v StageAttempt) error {
	terminal := v.FinishedAt != nil
	switch v.State {
	case StageAttemptRunning:
		if terminal || v.OutputDigest != nil || v.Cause != nil {
			return errors.New("running attempt has terminal fields")
		}
	case StageAttemptSucceeded:
		if !terminal || v.OutputDigest == nil || v.Cause != nil {
			return errors.New("succeeded attempt has invalid terminal fields")
		}
	case StageAttemptBlocked, StageAttemptNeedsReview, StageAttemptFailed:
		if !terminal || v.OutputDigest != nil || v.Cause != nil {
			return fmt.Errorf("%s attempt has invalid terminal fields", v.State)
		}
	case StageAttemptCancelled, StageAttemptInterrupted:
		if !terminal || v.OutputDigest != nil || v.Cause == nil {
			return fmt.Errorf("%s attempt has invalid terminal fields", v.State)
		}
	default:
		return fmt.Errorf("invalid attempt state %q", v.State)
	}
	return nil
}
