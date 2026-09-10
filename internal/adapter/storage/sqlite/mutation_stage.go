package sqlite

import (
	"context"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var _ port.MutationStageStore = (*Store)(nil)

func (s *Store) FinishMutationStage(ctx context.Context, command domain.FinishMutationStageCommand) (domain.RunSnapshot, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.finishStage(ctx, command.Finish, digest, &command.Mutation, nil)
}

func finishMutationRecordTx(ctx context.Context, tx *immediateTx, finish domain.FinishStageCommand, mutation domain.MutationStageRecord) error {
	// A successful mutation finalization accounts for the complete attempt,
	// including failed transport/format operations. Existing generic records
	// retain their historical semantics; this stronger command cannot omit
	// unsettled operations or a settled reservation from its evidence set.
	var calls, reservations int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM call_records WHERE run_id=? AND stage_name=? AND attempt_id=?`,
		finish.RunID, finish.StageName, finish.AttemptID).Scan(&calls); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM budget_reservations WHERE run_id=? AND stage_name=? AND attempt_id=?`,
		finish.RunID, finish.StageName, finish.AttemptID).Scan(&reservations); err != nil {
		return err
	}
	if calls != len(mutation.Operations) || reservations != len(mutation.Reservations) {
		return wrap(ErrConsistency, "mutation finalization omits attempt operations or reservations", nil)
	}
	var occurrenceID domain.ArtifactOccurrenceID
	if err := tx.QueryRowContext(ctx, `SELECT occurrence.occurrence_id FROM artifact_occurrences occurrence
		JOIN call_records call ON call.call_record_id=occurrence.current_call_record_id
		JOIN artifact_declarations declaration ON declaration.declaration_id=occurrence.declaration_id
		JOIN physical_calls physical ON physical.attempt_call_id=declaration.attempt_call_id
		WHERE occurrence.run_id=? AND occurrence.stage_name=? AND occurrence.attempt_id=?
		AND occurrence.writer_token_id=? AND occurrence.role='OUTPUT' AND occurrence.digest=?
		AND call.state='TERMINAL' AND call.failure_code IS NULL
		AND physical.state='COMPLETED' AND physical.outcome_kind='SUCCESS'
		AND physical.response_digest=occurrence.digest`,
		finish.RunID, finish.StageName, finish.AttemptID, mutation.OutputWriterTokenID, mutation.OutputBlobDigest).Scan(&occurrenceID); err != nil {
		return wrap(ErrConsistency, "mutation finalization cannot resolve its attached output", err)
	}
	return recordMutationTx(ctx, tx, domain.MutationRecordRequest{
		Grant: mutation.Grant, RecordID: mutation.RecordID, Operations: mutation.Operations,
		Reservations: mutation.Reservations, OutputOccurrenceID: occurrenceID, At: finish.At,
	})
}
