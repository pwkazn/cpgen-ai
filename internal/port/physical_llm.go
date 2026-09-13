package port

import (
	"context"

	"cpgen/internal/domain"
)

// LLMRequestPlan is deterministic and contains no credential or prompt text.
type LLMRequestPlan struct {
	RequestDigest         domain.Digest
	Provider              string
	InputTokenUpperBound  int64
	OutputTokenUpperBound int64
}

// PhysicalLLMResult retains accounting even when structured validation fails.
// Reservation IDs are supplied by the application after the provider returns.
type PhysicalLLMResult struct {
	Execution     domain.PhysicalExecution[GenerateResponse]
	Usage         Usage
	UsageVerified bool
	// FormatRepair contains only local, allowlisted validation codes. It never
	// includes raw provider text and is absent for envelope/transport/domain errors.
	FormatRepair *RepairInput
}

// PhysicalLLM performs at most one HTTP exchange. The application owns durable
// authorization, physical identity, retry, settlement and recovery.
type PhysicalLLM interface {
	PlanGenerate(GenerateRequest) (LLMRequestPlan, error)
	// ValidatePhysicalResponse rechecks local schema bindings without network I/O.
	ValidatePhysicalResponse(GenerateRequest, GenerateResponse) error
	GeneratePhysical(context.Context, GenerateRequest, domain.AttemptCallID) (PhysicalLLMResult, error)
}
