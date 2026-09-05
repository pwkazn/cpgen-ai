package port

import (
	"context"
	"time"

	"cpgen/internal/domain"
)

// CallLedger persists CPGen-specific logical calls, physical dispatch boundaries,
// and their budget reservations. Implementations keep every method transactional;
// callers perform external work only between these methods.
type CallLedger interface {
	OpenCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error)
	LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error)
	PrepareCalls(context.Context, domain.PrepareCallsRequest) (domain.PreparedCalls, error)
	BeginDispatch(context.Context, domain.BeginDispatchRequest) (domain.DispatchGrant, error)
	ResumeDispatch(context.Context, int64, domain.AttemptCallID) (domain.DispatchGrant, error)
	MarkSent(context.Context, domain.DispatchGrant, time.Time) error
	CompletePhysical(context.Context, domain.CompletePhysicalRequest) error
	FinishCall(context.Context, domain.FinishCallRequest) (domain.CallTrace, error)
}
