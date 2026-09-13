package sqlite

import (
	"context"
	"errors"

	"cpgen/internal/domain"
)

// ReleaseUnwrittenArtifactReservations releases unwritten response slots only
// after their parent provider call is terminal. SEALED/FINALIZED receipts retain
// byte reservations until occurrence attachment. The executor holds its run lock.
func (s *Store) ReleaseUnwrittenArtifactReservations(ctx context.Context, request domain.OpenCallRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		call, err := readCallRecord(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		if call.RunID != request.RunID || call.StageName != request.StageName || call.AttemptID != request.AttemptID || call.RequestDigest != request.RequestDigest || call.PolicyDigest != request.PolicyDigest {
			return wrap(ErrConsistency, "artifact reservation scope differs", nil)
		}
		if err := validateMeteringSettlementContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		parentID, ok := privateResponseParent(call)
		if !ok {
			return wrap(ErrConsistency, "artifact release requires a bound private response operation", nil)
		}
		parent, err := readCallRecord(ctx, tx, parentID)
		if err != nil {
			return err
		}
		if parent.RunID != call.RunID || parent.StageName != call.StageName || parent.AttemptID != call.AttemptID || parent.Kind != call.Kind || parent.Provider == "private-blob" || parent.State != domain.CallRecordTerminal {
			return wrap(ErrInvalidTransition, "response slots require a terminal provider call in the same attempt", nil)
		}
		if call.State == domain.CallRecordOpen {
			_, err := finishSettledLLMArtifactCall(ctx, tx, call, request.ExpectedRunVersion, s.clock.Now())
			return err
		}
		prepared, err := readPreparedCalls(ctx, tx, call.ID)
		if err != nil {
			return err
		}
		for _, physical := range prepared.PhysicalCalls {
			if physical.Kind != domain.PhysicalLocalArtifactWrite {
				return errors.New("artifact release requires only local artifact calls")
			}
		}
		now := s.clock.Now()
		for _, reservation := range prepared.Reservations {
			if reservation.State != domain.ReservationReserved {
				continue
			}
			// A terminal foreground operation cannot produce more bytes. An
			// unsealed writer left by process death has no recoverable receipt.
			if _, err := tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state='RELEASED', released_at=?
				WHERE declaration_id IN (SELECT declaration_id FROM artifact_declarations WHERE reservation_id=?) AND state IN ('PREPARED','OPEN')`, formatTime(now), reservation.ID); err != nil {
				return err
			}
			var active int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifact_writer_tokens token JOIN artifact_declarations decl ON decl.declaration_id = token.declaration_id
				WHERE decl.reservation_id = ? AND token.state <> 'RELEASED'`, reservation.ID).Scan(&active); err != nil {
				return err
			}
			if active != 0 {
				continue
			}
			if err := settleReservation(ctx, tx, reservation, 0, domain.ReservationReleased, now); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE physical_calls SET state = 'ABORTED_NO_DISPATCH', outcome_kind = 'NO_SEND',
				failure_code = 'policy_rejected', failure_class = 'REJECTED', failure_json = ?, completed_at = ?,
				complete_idempotency_key = ?, complete_command_digest = ? WHERE attempt_call_id = ? AND state IN ('PREPARED','DISPATCHING')`,
				[]byte(`{"code":"policy_rejected","class":"REJECTED"}`), formatTime(now), "unused-artifact:"+string(reservation.AttemptCallID), domain.SumBytes([]byte("unused-artifact:"+string(reservation.AttemptCallID))), reservation.AttemptCallID)
			if err != nil {
				return err
			}
		}
		_, err = finishSettledLLMArtifactCall(ctx, tx, call, request.ExpectedRunVersion, now)
		return err
	})
}
