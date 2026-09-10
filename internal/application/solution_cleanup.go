package application

import (
	"context"
	"errors"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// Terminal cleanup preserves completed evidence and conservatively settles
// uncertain creates only after the exact Docker ownership has been cleaned.
// This path never compiles, runs, starts a model, or republishes host content.
func (s *LocalRunService) reconcileSolutionVerification(ctx context.Context, runID domain.RunID) error {
	return s.reconcileSandboxVerification(ctx, runID, "solution_verify")
}

func (s *LocalRunService) reconcileSandboxVerification(ctx context.Context, runID domain.RunID, stage domain.StageName) error {
	current, attempt, err := s.generation.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != stage || (stage != "solution_verify" && stage != "data_verify" && stage != "judge" && stage != "quality" && stage != "package") || s.reconciler == nil {
		return errors.New("sandbox verification cleanup requires its exact stage and Docker reconciler")
	}
	report, err := s.reconciler.ReconcileRun(ctx, runID)
	if err != nil {
		return err
	}
	if !report.Completed || cleanupEvidenceMatchesRun(runID, report) {
		return ErrCleanupPending
	}
	store, ok := s.generation.config.Store.(interface {
		ReadAttemptSandboxCalls(context.Context, domain.RunID, domain.StageName, domain.AttemptID) ([]domain.CallRecord, error)
	})
	if !ok {
		return errors.New("solution cleanup requires scoped call history")
	}
	calls, err := store.ReadAttemptSandboxCalls(ctx, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	ledger, err := NewRunBoundLLMLedger(s.generation.config.Store, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	identity := solutionVerificationIdentity(*attempt, current.Version)
	if stage == "data_verify" {
		identity = dataVerificationIdentity(*attempt, current.Version, false)
	}
	if stage == "judge" {
		identity = judgeVerificationIdentity(*attempt, current.Version)
	}
	if stage == "quality" {
		identity = qualityVerificationIdentity(*attempt, current.Version)
	}
	if stage == "package" {
		identity = packagePublicationIdentity(*attempt, current.Version)
	}
	for _, call := range calls {
		if call.State == domain.CallRecordTerminal {
			continue
		}
		if call.Provider != "blob" && call.Provider != "docker" {
			return errors.New("sandbox cleanup found an unsupported provider")
		}
		failure := &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
		if call.State == domain.CallRecordOpen {
			_, err := ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: failure, IdempotencyKey: coordinatorMutationID("finish", call.ID), At: s.clock.Now().UTC()})
			if err != nil {
				return err
			}
			continue
		}
		p, err := ledger.LoadCall(ctx, call.ID)
		if err != nil {
			return err
		}
		if len(p.PhysicalCalls) != 1 {
			return errors.New("sandbox cleanup call has multiple physical identities")
		}
		physical := p.PhysicalCalls[0]
		if call.Provider == "blob" && physical.State != domain.PhysicalCompleted && physical.State != domain.PhysicalAbortedNoDispatch {
			declID := domain.ArtifactDeclarationID(coordinatorMutationID("decl", call.ID))
			decl, token, err := ledger.ReadArtifactWriter(ctx, declID)
			if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
				return err
			}
			if err == nil {
				if token.State == domain.ArtifactWriterSealed {
					session, err := NewPreparedArtifactSession(ledger, s.generation.config.Blobs, p)
					if err != nil {
						return err
					}
					writer, err := session.Prepare(ctx, decl.ID)
					if err != nil {
						return err
					}
					if _, err := writer.Finalize(ctx); err != nil {
						return err
					}
					token.State = domain.ArtifactWriterFinalized
				}
				if token.State == domain.ArtifactWriterFinalized {
					pending, err := ledger.ReadPendingArtifact(ctx, decl.ID)
					if err != nil {
						return err
					}
					sink := &SandboxArtifactSink{ledger: ledger, blobs: s.generation.config.Blobs, clock: s.clock, identity: identity}
					if err := sink.complete(ctx, decl, pending); err != nil {
						return err
					}
					continue
				}
				if token.State != domain.ArtifactWriterReleased {
					if err := ledger.ReleaseArtifact(ctx, token.ID); err != nil {
						return err
					}
				}
			}
		}
		if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
			state, outcome := domain.PhysicalUnknown, domain.PhysicalOutcomeUnknown
			failure = &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
			if call.Provider == "blob" {
				state, outcome = domain.PhysicalAbortedNoDispatch, domain.PhysicalOutcomeNoSend
				failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
			}
			if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, State: state, Outcome: outcome, Failure: failure, IdempotencyKey: coordinatorMutationID("terminal_cleanup", physical.ID), At: s.clock.Now().UTC()}); err != nil {
				return err
			}
		}
		binding := port.SandboxResourceCall{CallRecordID: call.ID, AttemptCallID: physical.ID}
		if err := finishSandboxResourceCalls(ctx, ledger, s.clock, identity, []port.SandboxResourceCall{binding}); err != nil {
			return err
		}
	}
	return nil
}
