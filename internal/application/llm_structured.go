package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// FormatRepairPolicy is part of the immutable provider policy binding. JSON
// format repair has one optional allowance, independent of transport retries
// and the workflow's business-content repair budget.
type FormatRepairPolicy struct {
	MaxRepairs int64          `json:"max_repairs"`
	Prompt     port.PromptRef `json:"prompt"`
}

// StructuredLLMResult preserves the separate logical traces and private
// receipts of the original request and its optional format repair. Outcome
// belongs to the last call; Usage accounts for every call in this result.
type StructuredLLMResult struct {
	Outcome    domain.MeteredOutcome[port.GenerateResponse]
	CallTraces []domain.CallTrace
	Artifacts  []domain.PendingArtifact
	Usage      port.Usage
}

type StructuredLLMCalls struct {
	mu     sync.Mutex
	calls  *LLMCalls
	policy FormatRepairPolicy
}

func NewStructuredLLMCalls(calls *LLMCalls, policy FormatRepairPolicy) (*StructuredLLMCalls, error) {
	if calls == nil || calls.artifacts == nil {
		return nil, errors.New("structured LLM calls require private durable response storage")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &StructuredLLMCalls{calls: calls, policy: policy}, nil
}

// Generate must run under the foreground executor's run lock and artifact
// maintenance lock, as required by LLMCalls. Neither restart nor a transport
// retry replenishes the persisted format-repair allowance.
func (s *StructuredLLMCalls) Generate(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (StructuredLLMResult, error) {
	return s.generate(ctx, open, request, false)
}

// Reconcile restores the original call and an already opened format repair.
// A valid diagnostic alone cannot authorize a new repair during cleanup.
func (s *StructuredLLMCalls) Reconcile(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (StructuredLLMResult, error) {
	return s.generate(ctx, open, request, true)
}

func (s *StructuredLLMCalls) generate(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest, reconcile bool) (StructuredLLMResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result StructuredLLMResult
	if ctx == nil {
		return result, errors.New("structured LLM context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	open, request, err := s.bind(open, request)
	if err != nil {
		return result, err
	}
	if s.policy.MaxRepairs == 1 && !reconcile {
		// Resolve the repair prompt and schema before spending on the original
		// call. This synthetic diagnostic is only used for local preflight.
		probe := port.RepairInput{SchemaVersion: request.Schema.SchemaVersion, ErrorCodes: []port.StructuredOutputErrorCode{port.StructuredOutputInvalidJSON}}
		if _, _, err := s.repairRequest(open, request, probe); err != nil {
			return result, err
		}
	}
	call := s.calls.Generate
	if reconcile {
		call = s.calls.Reconcile
	}
	outcome, callErr := call(ctx, open, request)
	if err := s.appendResult(ctx, &result, open.ID, outcome); err != nil {
		return result, errors.Join(callErr, err)
	}
	if callErr != nil || outcome.Value != nil || s.policy.MaxRepairs == 0 {
		return result, callErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	diagnostic, err := s.calls.ReadFormatRepair(ctx, open, request)
	if err != nil || diagnostic == nil {
		return result, err
	}
	repairOpen, repairRequest, err := s.repairRequest(open, request, *diagnostic)
	if err != nil {
		return result, err
	}
	// Deliberately call the raw durable service once. A rejected repair cannot
	// re-enter this method and start a second content regeneration.
	if reconcile {
		reader, ok := s.calls.ledger.(CallReconciliationLedger)
		if !ok {
			return result, errors.New("repair reconciliation requires original call reads")
		}
		if _, err := reader.ReadLogicalCall(ctx, repairOpen.ID); errors.Is(err, sqlite.ErrNotFound) {
			return result, nil
		} else if err != nil {
			return result, err
		}
	}
	outcome, callErr = call(ctx, repairOpen, repairRequest)
	if err := s.appendResult(ctx, &result, repairOpen.ID, outcome); err != nil {
		return result, errors.Join(callErr, err)
	}
	return result, callErr
}

func (s *StructuredLLMCalls) bind(open domain.OpenCallRequest, request port.GenerateRequest) (domain.OpenCallRequest, port.GenerateRequest, error) {
	return bindStructuredRequest(s.calls.provider, s.policy, open, request)
}

func bindStructuredRequest(provider LLMReadPolicy, policy FormatRepairPolicy, open domain.OpenCallRequest, request port.GenerateRequest) (domain.OpenCallRequest, port.GenerateRequest, error) {
	if err := open.Validate(); err != nil {
		return open, request, err
	}
	request.Variables = append(json.RawMessage(nil), request.Variables...)
	plan, err := provider.PlanGenerate(request)
	if err != nil {
		return open, request, err
	}
	if open.Kind != domain.CallLLMGenerate || open.Provider != plan.Provider || open.RequestDigest != plan.RequestDigest || open.PolicyDigest != request.ProviderPolicyDigest || open.LogicalOperationID != request.LogicalIdempotencyKey || open.RetryPolicy.MaxAttempts > 8 {
		return open, request, errors.New("structured LLM call differs from admitted provider request")
	}
	raw, err := json.Marshal(struct {
		Revision string             `json:"revision"`
		Provider domain.Digest      `json:"provider"`
		Repair   FormatRepairPolicy `json:"repair"`
	}{"cpgen.structured-llm/v1", request.ProviderPolicyDigest, policy})
	if err != nil {
		return open, request, err
	}
	request.ProviderPolicyDigest = domain.SumBytes(raw)
	open.PolicyDigest = request.ProviderPolicyDigest
	plan, err = provider.PlanGenerate(request)
	open.RequestDigest = plan.RequestDigest
	return open, request, err
}

func (s *StructuredLLMCalls) repairRequest(open domain.OpenCallRequest, original port.GenerateRequest, repair port.RepairInput) (domain.OpenCallRequest, port.GenerateRequest, error) {
	return buildRepairRequest(s.calls.provider, s.policy, open, original, repair)
}

func buildRepairRequest(provider LLMReadPolicy, policy FormatRepairPolicy, open domain.OpenCallRequest, original port.GenerateRequest, repair port.RepairInput) (domain.OpenCallRequest, port.GenerateRequest, error) {
	if !validPrivateFormatRepair(&repair, original.Schema.SchemaVersion) {
		return open, original, errors.New("format repair input is not an allowed diagnostic")
	}
	variables, err := json.Marshal(struct {
		SchemaVersion domain.SchemaVersion `json:"schema_version"`
		OriginalInput json.RawMessage      `json:"original_input"`
		FormatRepair  port.RepairInput     `json:"format_repair"`
	}{formatRepairInputSchema, original.Variables, repair})
	if err != nil {
		return open, original, err
	}
	request := original
	request.Prompt = policy.Prompt
	request.Variables = variables
	request.LogicalIdempotencyKey = coordinatorMutationID("format-repair", open.ID, open.PolicyDigest, 1)
	open.ID = domain.CallRecordID(coordinatorMutationID("callrec", open.ID, "format-repair", open.PolicyDigest, 1))
	open.LogicalOperationID = request.LogicalIdempotencyKey
	open.IdempotencyKey = coordinatorMutationID("open", "format-repair", open.ID)
	// Preserve At: the ledger's idempotency binding includes the original
	// timestamp, so a restart must not supply a new wall-clock value.
	plan, err := provider.PlanGenerate(request)
	if err != nil {
		return open, request, err
	}
	open.Provider, open.RequestDigest = plan.Provider, plan.RequestDigest
	return open, request, open.Validate()
}

func (s *StructuredLLMCalls) appendResult(ctx context.Context, result *StructuredLLMResult, id domain.CallRecordID, outcome domain.MeteredOutcome[port.GenerateResponse]) error {
	result.Outcome = outcome
	if outcome.CallTrace.Validate() != nil {
		return nil
	}
	result.CallTraces = append(result.CallTraces, outcome.CallTrace)
	if outcome.Value != nil && outcome.Value.RawBlob != nil {
		result.Artifacts = append(result.Artifacts, *outcome.Value.RawBlob)
	}
	if outcome.Failure != nil {
		result.Artifacts = append(result.Artifacts, outcome.Failure.Evidence...)
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	prepared, err := s.calls.ledger.LoadCall(readCtx, id)
	if err != nil {
		return err
	}
	for _, reservation := range prepared.Reservations {
		if reservation.SettledValue == nil {
			continue
		}
		var target *int64
		switch reservation.Dimension {
		case domain.BudgetLLMInputTokens:
			target = &result.Usage.InputTokens
		case domain.BudgetLLMOutputTokens:
			target = &result.Usage.OutputTokens
		}
		if target != nil {
			value := *reservation.SettledValue
			if value < 0 || *target > math.MaxInt64-value {
				return errors.New("structured LLM settled usage overflows")
			}
			*target += value
		}
	}
	return nil
}

func (policy FormatRepairPolicy) Validate() error {
	if policy.MaxRepairs < 0 || policy.MaxRepairs > 1 {
		return errors.New("JSON format repair permits zero or one repair")
	}
	if policy.MaxRepairs == 1 {
		if err := policy.Prompt.Validate(); err != nil {
			return err
		}
	}
	return nil
}
