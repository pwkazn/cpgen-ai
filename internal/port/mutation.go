package port

import (
	"context"

	"cpgen/internal/domain"
)

type MutationAuthorizer interface {
	ClaimMutation(context.Context, domain.MutationClaimRequest) (domain.MutationGrant, error)
	RecordMutation(context.Context, domain.MutationRecordRequest) error
}

// GCMetadataStore is deliberately path-blind. It only plans and commits
// metadata transitions; the Blob store owns all filesystem operations.
type GCMetadataStore interface {
	PlanGarbage(context.Context) ([]domain.GCItem, error)
	CommitGarbage(context.Context, domain.GCItem, domain.GCCommit) error
	ListDeleting(context.Context) ([]domain.GCItem, error)
}
