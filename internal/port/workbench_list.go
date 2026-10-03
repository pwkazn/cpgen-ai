package port

import (
	"context"
	"errors"
	"fmt"

	"cpgen/internal/domain"
)

var ErrInvalidWorkbenchRunQuery = errors.New("invalid workbench run query")

// WorkbenchRunQuery describes a page of the most recently updated runs. Cursor
// is opaque to callers and may only be reused with the same state filter.
// Pagination is a live view: updates may move a run ahead of an earlier cursor.
type WorkbenchRunQuery struct {
	State  string
	Limit  int
	Cursor string
}

type WorkbenchRunPage struct {
	Runs       []WorkbenchRunSummary
	NextCursor string
}

type WorkbenchRunListReader interface {
	WorkbenchRuns(context.Context, WorkbenchRunQuery) (WorkbenchRunPage, error)
}

func (q WorkbenchRunQuery) Validate() error {
	if q.Limit < 1 || q.Limit > 1000 {
		return fmt.Errorf("%w: limit must be between 1 and 1000", ErrInvalidWorkbenchRunQuery)
	}
	_, err := q.States()
	return err
}

// States resolves the workbench aliases to a stable set of persisted states.
// A nil slice means all states; the CLI's single-state RunFilter is unchanged.
func (q WorkbenchRunQuery) States() ([]domain.RunState, error) {
	switch q.State {
	case "":
		return nil, nil
	case "active":
		return []domain.RunState{domain.RunCreated, domain.RunRunning}, nil
	case "ended":
		return []domain.RunState{domain.RunFailed, domain.RunCancelled}, nil
	case "ready":
		return []domain.RunState{domain.RunReady}, nil
	case "blocked":
		return []domain.RunState{domain.RunBlocked}, nil
	default:
		state := domain.RunState(q.State)
		if !state.Valid() {
			return nil, fmt.Errorf("%w: unknown run state", ErrInvalidWorkbenchRunQuery)
		}
		return []domain.RunState{state}, nil
	}
}
