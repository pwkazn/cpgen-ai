package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// This fixture proves transaction and ownership invariants only. Its payload
// is not a semantically verified package; the Docker application test supplies
// the actual Quality-to-archive proof that must precede this storage command.
func packageCommitFixture(t *testing.T) (*Store, domain.FinalizeVerifiedPackageCommand) {
	t.Helper()
	ctx := context.Background()
	s := openRuntimeStore(t, filepath.Join(t.TempDir(), "package.db"), clock.NewFake(testNow))
	r := testCreateRunRequest(testRunID, testNow, time.Minute)
	r.WorkflowRevision = "mvp.idea.statement.similarity.solution.data.judge.package.v1"
	r.WorkflowDigest = domain.SumBytes([]byte(r.WorkflowRevision))
	r.StageSequence = []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}
	mustCreateRun(t, s, r)
	digest := domain.SumBytes([]byte("storage fixture quality"))
	var attempt domain.StageAttempt
	var version int64 = 1
	for i, stage := range r.StageSequence {
		attempt = mustBeginStage(t, s, r.RunID, domain.AttemptID(fmt.Sprintf("attempt_%032x", i+1)), version, stage, digest, testNow, meteringID("begin", fmt.Sprintf("package-%d", i)))
		version++
		if stage == "package" {
			break
		}
		result, err := s.FinishStage(ctx, domain.FinishStageCommand{RunID: r.RunID, ExpectedRunVersion: version, StageName: stage, AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &digest, NextStage: r.StageSequence[i+1], NextInputDigest: &digest, IdempotencyKey: meteringID("finish", fmt.Sprintf("package-%d", i)), At: testNow})
		if err != nil {
			t.Fatal(err)
		}
		version = result.Version
	}
	f := meteringFixture{store: s, runID: r.RunID, stage: "package", attemptID: attempt.AttemptID, now: testNow}
	open := openCallRequest(f, 1, domain.CallSandboxCompile)
	open.ExpectedRunVersion, open.Provider = version, "blob"
	call, err := s.OpenCall(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	prepare := prepareOneRequest(f, call, 1, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64)
	prepare.ExpectedRunVersion = version
	prepared, err := s.PrepareCalls(ctx, prepare)
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	declaration := domain.ArtifactDeclarationRecord{ID: "decl_00000000000000000000000000000071", RunID: r.RunID, StageName: "package", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "application/zip", Role: domain.ArtifactOutput, LogicalPath: "package/problem.zip", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.package/v2", Producer: "mvp-package", InputDigest: &digest}, CreatedAt: testNow}
	if err := s.CreateArtifactDeclaration(ctx, declaration); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := artifactsession.NewPreparedArtifactSession(s, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	w, err := session.Prepare(ctx, declaration.ID)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: r.RunID, ExpectedRunVersion: version, StageName: "package", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, IdempotencyKey: meteringID("dispatch", "package"), At: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSent(ctx, grant, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("owned storage fixture archive")); err != nil {
		t.Fatal(err)
	}
	pending, err := w.Finalize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: r.RunID, ExpectedRunVersion: version, StageName: "package", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "local-artifact:" + string(pending.WriterTokenID), ResponseDigest: &pending.Blob.Digest, Usage: []domain.ReservationUsage{{ReservationID: reservation.ID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: reservation.Subkey, Value: pending.PhysicalNewBytes, Verified: true}}, IdempotencyKey: meteringID("complete", "package"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishCall(ctx, domain.FinishCallRequest{RunID: r.RunID, ExpectedRunVersion: version, StageName: "package", AttemptID: attempt.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: meteringID("finish", "package-local-call"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	binding := domain.VerifiedPackageBinding{PackageID: domain.SumBytes([]byte("package identity")), ManifestDigest: domain.SumBytes([]byte("manifest")), QualityDigest: digest, Archive: pending.Blob}
	finish := domain.FinishStageCommand{RunID: r.RunID, ExpectedRunVersion: version, StageName: "package", AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &binding.Archive.Digest, NextStage: "package", NextInputDigest: &binding.Archive.Digest, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: meteringID("finish", "verified-package"), At: testNow}
	return s, domain.FinalizeVerifiedPackageCommand{Finish: finish, Package: binding}
}

func TestVerifiedPackageAtomicRollbackReplayAndImmutability(t *testing.T) {
	s, command := packageCommitFixture(t)
	ctx := context.Background()
	ordinary := command.Finish
	ordinary.RunState = domain.RunReady
	if _, err := s.FinishStage(ctx, ordinary); err == nil {
		t.Fatal("ordinary FinishStage authorized READY")
	}
	if _, err := s.db.Exec(`UPDATE runs SET state='READY' WHERE run_id=?`, command.Finish.RunID); err == nil {
		t.Fatal("SQL authorized READY without a package")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_package_commit BEFORE INSERT ON verified_packages BEGIN SELECT RAISE(ABORT,'injected package transaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeVerifiedPackage(ctx, command); err == nil {
		t.Fatal("injected commit did not fail")
	}
	run, err := s.GetRun(ctx, command.Finish.RunID)
	if err != nil || run.State != domain.RunRunning || run.Version != command.Finish.ExpectedRunVersion || run.FinalPackageOccurrenceID != nil {
		t.Fatalf("partial run commit: %+v %v", run, err)
	}
	var occurrences, packages int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM artifact_occurrences),(SELECT count(*) FROM verified_packages)`).Scan(&occurrences, &packages); err != nil || occurrences != 0 || packages != 0 {
		t.Fatalf("partial package attachment: %d %d %v", occurrences, packages, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_package_commit`); err != nil {
		t.Fatal(err)
	}
	ready, err := s.FinalizeVerifiedPackage(ctx, command)
	if err != nil || ready.State != domain.RunReady || ready.FinalPackageOccurrenceID == nil {
		t.Fatalf("READY completion: %+v %v", ready, err)
	}
	again, err := s.FinalizeVerifiedPackage(ctx, command)
	if err != nil || again.Version != ready.Version || again.FinalPackageOccurrenceID == nil || *again.FinalPackageOccurrenceID != *ready.FinalPackageOccurrenceID {
		t.Fatalf("completion replay changed identity: %+v %v", again, err)
	}
	record, err := s.ReadVerifiedPackage(ctx, ready.RunID)
	if err != nil || record.Binding != command.Package || record.OccurrenceID != *ready.FinalPackageOccurrenceID {
		t.Fatalf("package record: %+v %v", record, err)
	}
	command.Package.ManifestDigest = domain.SumBytes([]byte("changed manifest"))
	if _, err := s.FinalizeVerifiedPackage(ctx, command); err == nil {
		t.Fatal("idempotency key accepted changed package")
	}
	for _, query := range []string{"UPDATE verified_packages SET status='VERIFIED'", "DELETE FROM verified_packages", "UPDATE runs SET state='RUNNING',final_package_occurrence_id=NULL"} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("immutable package changed: %s", query)
		}
	}
}

func TestVerifiedPackageRejectsChangedQualityAndPendingCancel(t *testing.T) {
	for _, mode := range []string{"quality", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, command := packageCommitFixture(t)
			ctx := context.Background()
			if mode == "quality" {
				if _, err := s.db.Exec(`UPDATE stage_records SET output_digest=? WHERE stage_name='quality'`, domain.SumBytes([]byte("another quality"))); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := s.RequestCancel(ctx, domain.CancelRequest{ID: domain.ControlRequestID(meteringID("control", "cancel-package")), RunID: command.Finish.RunID, ExpectedRunVersion: command.Finish.ExpectedRunVersion, Reason: "stop", IdempotencyKey: meteringID("cancel", "package"), At: testNow})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.FinalizeVerifiedPackage(ctx, command); err == nil {
				t.Fatal("invalid run acquired READY package")
			}
			if _, err := s.ReadVerifiedPackage(ctx, command.Finish.RunID); err == nil {
				t.Fatal("rejected package became readable")
			}
		})
	}
}
