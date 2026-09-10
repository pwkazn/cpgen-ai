package application

import (
	"context"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// Only BeginDispatch reached from a PREPARED row can issue local send authority.
// Recovered DISPATCHING receipts never grant another provider send. Each
// foreground logical call owns its guard; physical operations remain serial.
type freshDispatchLedger struct {
	port.CallLedger
	grants map[domain.AttemptCallID]domain.DispatchGrant
}

func (l *freshDispatchLedger) BeginDispatch(ctx context.Context, request domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	prepared, err := l.LoadCall(ctx, request.CallRecordID)
	if err != nil {
		return domain.DispatchGrant{}, err
	}
	fresh := false
	for _, call := range prepared.PhysicalCalls {
		if call.ID == request.AttemptCallID {
			fresh = call.State == domain.PhysicalPrepared
		}
	}
	grant, err := l.CallLedger.BeginDispatch(ctx, request)
	if err == nil && fresh {
		l.grants[grant.AttemptCallID] = grant
	}
	return grant, err
}
