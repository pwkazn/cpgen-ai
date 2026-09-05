package docker

import (
	"context"
	"testing"

	"cpgen/internal/domain"
)

func TestSandboxReconcilerPublicSurfaceIsCleanupOnly(t *testing.T) {
	var reconciler SandboxReconciler = &narrowReconciler{}
	if reconciler == nil {
		t.Fatal("reconciler interface must be usable")
	}
}

type narrowReconciler struct{}

func (*narrowReconciler) ReconcileRun(context.Context, domain.RunID) (domain.SandboxReconcileReport, error) {
	return domain.SandboxReconcileReport{}, nil
}
