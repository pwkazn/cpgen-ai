package workflow

import (
	"context"

	"cpgen/internal/domain"
)

type Step[I any, O any] interface {
	Name() domain.StageName
	Run(context.Context, domain.RunView, I) (domain.AgentResult[O], error)
}
