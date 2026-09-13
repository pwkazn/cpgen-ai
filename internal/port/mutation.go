package port

import (
	"context"

	"cpgen/internal/domain"
)

type MutationAuthorizer interface {
	ClaimMutation(context.Context, domain.MutationClaimRequest) (domain.MutationGrant, error)
	RecordMutation(context.Context, domain.MutationRecordRequest) error
}

// MutationBudgetReader does not create accounts or consume a logical claim.
type MutationBudgetReader interface {
	ReadMutationBudget(context.Context, domain.RunID, domain.StageName) (domain.MutationBudgetSnapshot, error)
}

// MutationStageStore atomically retains the successful output and its complete
// mutation evidence before advancing. It neither creates a claim nor routes.
type MutationStageStore interface {
	FinishMutationStage(context.Context, domain.FinishMutationStageCommand) (domain.RunSnapshot, error)
}

// MutationRecordReader reconstructs immutable result metadata, not content or
// dispatch authorization. A claimed but unfinished mutation returns not found.
type MutationRecordReader interface {
	ReadMutationRecord(context.Context, domain.MutationGrant) (domain.MutationRecordRequest, error)
}

// GCMetadataStore is deliberately path-blind. It only plans and commits
// metadata transitions; the Blob store owns all filesystem operations.
type GCMetadataStore interface {
	PlanGarbage(context.Context) ([]domain.GCItem, error)
	CommitGarbage(context.Context, domain.GCItem, domain.GCCommit) error
	ListDeleting(context.Context) ([]domain.GCItem, error)
}
