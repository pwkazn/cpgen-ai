package application

import (
	"context"

	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

type RunEvidenceReadStore interface {
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
}

// SandboxEvidenceReadStore is the complete read capability required to prove
// committed Solution, Data, Judge and Quality results. No runtime assertion or
// execution ledger is needed after composition.
type SandboxEvidenceReadStore interface {
	RunEvidenceReadStore
	port.SandboxLifecycleReader
	ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
}

type PackageEvidenceReadStore interface {
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
	ReadVerifiedPackage(context.Context, domain.RunID) (domain.VerifiedPackageRecord, error)
	ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
}
type SimilarityContentReadStore interface {
	RunEvidenceReadStore
	ReadStageInputDigest(context.Context, domain.RunID, domain.StageName) (domain.Digest, error)
	CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
}

// Composed once for the executing Similarity stage; readers retain only the
// smaller evidence interfaces declared above.
type SimilarityStageStore interface {
	durable.Store
	durable.SimilarityReadStore
	SimilarityContentReadStore
}
