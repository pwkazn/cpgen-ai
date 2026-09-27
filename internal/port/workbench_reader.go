package port

import (
	"context"

	"cpgen/internal/domain"
)

type WorkbenchRunSummary struct {
	domain.RunSummary
	Brief string
}

// WorkbenchReadSnapshot is the durable portion of a run detail page. A store
// must populate every field in one read transaction so versions cannot be
// mixed across concurrent updates.
type WorkbenchReadSnapshot struct {
	Run            domain.RunSnapshot
	RequestJSON    []byte
	ConfigJSON     []byte
	Budget         domain.BudgetSnapshot
	BudgetUsed     map[domain.BudgetDimension]int64
	BudgetReserved map[domain.BudgetDimension]int64
	Stages         []WorkbenchStageSnapshot
	PendingReview  *domain.ReviewDecision
	PendingCancel  *domain.ControlRequest
	Artifacts      []WorkbenchArtifactSnapshot
	RecentEvents   []domain.RunEvent
	BeforeVersion  int64
}

type WorkbenchStageSnapshot struct {
	Name     domain.StageName
	Ordinal  int
	State    domain.StageState
	Attempts []domain.StageAttempt
}

type WorkbenchArtifactSnapshot struct {
	domain.CommittedArtifactRef
	StageName domain.StageName
	MediaType string
}

type WorkbenchRunReader interface {
	ReadWorkbenchRun(context.Context, domain.RunID) (WorkbenchReadSnapshot, error)
	ReadWorkbenchArtifact(context.Context, domain.RunID, domain.ArtifactOccurrenceID) (WorkbenchArtifactSnapshot, error)
}
