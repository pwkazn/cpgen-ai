package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

type reconcileFunc func(context.Context, domain.RunID) (domain.SandboxReconcileReport, error)

func (f reconcileFunc) ReconcileRun(ctx context.Context, runID domain.RunID) (domain.SandboxReconcileReport, error) {
	return f(ctx, runID)
}

func TestCleanupEvidenceMatchesRunRequiresExplicitRows(t *testing.T) {
	runID := domain.RunID("run-1")
	cases := []struct {
		name   string
		report domain.SandboxReconcileReport
		want   bool
	}{
		{name: "zero value", report: domain.SandboxReconcileReport{}, want: false},
		{name: "wrong run", report: domain.SandboxReconcileReport{RunID: "run-2", Pending: 1, Executions: []domain.SandboxExecution{{ID: "exec-1"}}}, want: false},
		{name: "count without row", report: domain.SandboxReconcileReport{RunID: runID, Pending: 1}, want: false},
		{name: "execution row", report: domain.SandboxReconcileReport{RunID: runID, Pending: 1, Executions: []domain.SandboxExecution{{ID: "exec-1"}}}, want: true},
		{name: "resource row", report: domain.SandboxReconcileReport{RunID: runID, Pending: 1, Resources: []domain.SandboxResource{{ID: "resource-1"}}}, want: true},
		{name: "manual blocker", report: domain.SandboxReconcileReport{RunID: runID, ManualCleanup: []domain.SandboxCleanupBlocker{{ExecutionID: "exec-1", Reason: "identity evidence is unavailable", Manual: true}}}, want: true},
		{name: "incomplete blocker", report: domain.SandboxReconcileReport{RunID: runID, ManualCleanup: []domain.SandboxCleanupBlocker{{ExecutionID: "exec-1", Manual: true}}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanupEvidenceMatchesRun(runID, tc.report); got != tc.want {
				t.Fatalf("cleanupEvidenceMatchesRun() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileFailureWithoutEvidenceRemainsGeneric(t *testing.T) {
	runID := domain.RunID("run-1")
	rootErr := errors.New("inspection failed")
	service := &LocalRunService{reconciler: reconcileFunc(func(context.Context, domain.RunID) (domain.SandboxReconcileReport, error) {
		return domain.SandboxReconcileReport{}, rootErr
	})}

	if err := service.reconcileForTerminal(context.Background(), runID); err == nil {
		t.Fatal("reconcileForTerminal unexpectedly succeeded")
	} else {
		if errors.Is(err, ErrCleanupPending) {
			t.Fatalf("generic reconciliation error was classified as cleanup pending: %v", err)
		}
		if !errors.Is(err, rootErr) {
			t.Fatalf("error = %v, want wrapped inspection failure", err)
		}
	}

	snapshot := domain.RunSnapshot{RunID: runID}
	if _, err := (&LocalRunService{reconciler: service.reconciler}).recover(context.Background(), snapshot, "", false); err == nil {
		t.Fatal("recoverRunning unexpectedly succeeded")
	} else if errors.Is(err, ErrCleanupPending) || !errors.Is(err, rootErr) {
		t.Fatalf("recovery error = %v, want generic wrapped inspection failure", err)
	}
}

func TestReconcileFailureWithEvidenceRemainsCleanupPending(t *testing.T) {
	runID := domain.RunID("run-1")
	service := &LocalRunService{reconciler: reconcileFunc(func(context.Context, domain.RunID) (domain.SandboxReconcileReport, error) {
		return domain.SandboxReconcileReport{RunID: runID, Pending: 1, Executions: []domain.SandboxExecution{{ID: "exec-1"}}}, errors.New("cleanup still running")
	})}
	err := service.reconcileForTerminal(context.Background(), runID)
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("error = %v, want cleanup pending", err)
	}
	if !strings.Contains(err.Error(), "cleanup still running") {
		t.Fatalf("error = %v, want original reconciliation error", err)
	}
}

func TestZeroReconcileReportDoesNotBecomeCleanupPending(t *testing.T) {
	runID := domain.RunID("run-1")
	service := &LocalRunService{reconciler: reconcileFunc(func(context.Context, domain.RunID) (domain.SandboxReconcileReport, error) {
		return domain.SandboxReconcileReport{}, nil
	})}
	if err := service.reconcileForTerminal(context.Background(), runID); err == nil || errors.Is(err, ErrCleanupPending) {
		t.Fatalf("terminal reconciliation error = %v, want generic incomplete-report error", err)
	}
	if _, err := (&LocalRunService{reconciler: service.reconciler}).recover(context.Background(), domain.RunSnapshot{RunID: runID}, "", false); err == nil || errors.Is(err, ErrCleanupPending) {
		t.Fatalf("recovery reconciliation error = %v, want generic incomplete-report error", err)
	}
}
