package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	"cpgen/internal/domain"
)

// finishAttachedLLMResponseCalls completes local response publication in the
// same transaction that attaches its occurrences and settles physical bytes.
// It never creates a provider grant or runs filesystem I/O.
func finishAttachedLLMResponseCalls(ctx context.Context, tx *immediateTx, command domain.FinishStageCommand) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT call.call_record_id FROM call_records call
		JOIN artifact_occurrences occurrence ON occurrence.current_call_record_id=call.call_record_id
		WHERE occurrence.run_id=? AND occurrence.stage_name=? AND occurrence.attempt_id=?
		AND occurrence.kind='NEW_WRITE' AND call.provider='private-blob'
		AND ((call.call_kind='LLM_GENERATE' AND (call.logical_operation_id LIKE 'llm-response:%' OR call.logical_operation_id LIKE 'idea-batch-output:%'))
		OR (call.call_kind='SIMILARITY_SEARCH' AND call.logical_operation_id LIKE 'similarity-response:%'))`, command.RunID, command.StageName, command.AttemptID)
	if err != nil {
		return err
	}
	var ids []domain.CallRecordID
	for rows.Next() {
		var id domain.CallRecordID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		call, err := readCallRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		parentID, ok := privateResponseParent(call)
		if !ok {
			return errors.New("invalid private response parent")
		}
		parent, err := readCallRecord(ctx, tx, parentID)
		if err != nil {
			return err
		}
		if parent.RunID != call.RunID || parent.StageName != call.StageName || parent.AttemptID != call.AttemptID || parent.State != domain.CallRecordTerminal || parent.Kind != call.Kind || parent.Provider == "private-blob" {
			return wrap(ErrConsistency, "attached response requires its terminal provider call", nil)
		}
		prepared, err := readPreparedCalls(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, physical := range prepared.PhysicalCalls {
			if physical.Kind != domain.PhysicalLocalArtifactWrite {
				return errors.New("response completion contains a non-artifact call")
			}
			if physical.State == domain.PhysicalAbortedNoDispatch {
				continue
			}
			var ref domain.BlobRef
			var token domain.ArtifactWriterTokenID
			var reservation domain.ReservationID
			var physicalBytes, settled int64
			err := tx.QueryRowContext(ctx, `SELECT occurrence.digest,occurrence.size,token.writer_token_id,reservation.reservation_id,pin.physical_new_bytes,reservation.settled_value
				FROM artifact_occurrences occurrence JOIN artifact_declarations decl ON decl.declaration_id=occurrence.declaration_id
				JOIN artifact_writer_tokens token ON token.writer_token_id=occurrence.writer_token_id
				JOIN blob_pins pin ON pin.pin_id=occurrence.pin_id
				JOIN budget_reservations reservation ON reservation.reservation_id=occurrence.reservation_id
				WHERE occurrence.current_call_record_id=? AND decl.attempt_call_id=? AND occurrence.run_id=? AND occurrence.stage_name=? AND occurrence.attempt_id=?
				AND token.state='FINALIZED' AND reservation.state='SETTLED' AND reservation.dimension='ARTIFACT_PHYSICAL_NEW_BYTES'`, id, physical.ID, command.RunID, command.StageName, command.AttemptID).Scan(&ref.Digest, &ref.Size, &token, &reservation, &physicalBytes, &settled)
			if err != nil {
				return wrap(ErrConsistency, "response physical call lacks its attached settled artifact", err)
			}
			if physicalBytes != settled {
				return wrap(ErrConsistency, "response physical byte settlement differs", nil)
			}
			if physical.State == domain.PhysicalCompleted {
				if physical.Outcome == nil || *physical.Outcome != domain.PhysicalOutcomeSuccess || physical.ResponseDigest == nil || *physical.ResponseDigest != ref.Digest {
					return wrap(ErrConsistency, "completed artifact receipt differs", nil)
				}
				continue
			}
			if physical.State != domain.PhysicalSent {
				return wrap(ErrInvalidTransition, "response publication has no confirmed local boundary", nil)
			}
			completion := domain.CompletePhysicalRequest{RunID: command.RunID, ExpectedRunVersion: command.ExpectedRunVersion, StageName: command.StageName, AttemptID: command.AttemptID, CallRecordID: id, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "local-artifact:" + string(token), ResponseDigest: &ref.Digest, Usage: []domain.ReservationUsage{{ReservationID: reservation, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: "response", Value: settled, Verified: true}}, IdempotencyKey: llmArtifactMutationID("complete", string(physical.ID)), At: command.At}
			if err := writeLLMArtifactCompletion(ctx, tx, completion); err != nil {
				return err
			}
		}
		finished, err := finishSettledLLMArtifactCall(ctx, tx, call, command.ExpectedRunVersion, command.At)
		if err != nil {
			return err
		}
		if !finished {
			return wrap(ErrConsistency, "attached response has unfinished local slots", nil)
		}
	}
	return nil
}

func writeLLMArtifactCompletion(ctx context.Context, tx *immediateTx, completion domain.CompletePhysicalRequest) error {
	if err := completion.Validate(); err != nil {
		return err
	}
	digest, _, err := digestJSON(completion)
	if err != nil {
		return err
	}
	code, class, payload, err := failureColumns(completion.Failure)
	if err != nil {
		return err
	}
	var provider, response any
	if completion.ProviderRequestID != "" {
		provider = completion.ProviderRequestID
	}
	if completion.ResponseDigest != nil {
		response = string(*completion.ResponseDigest)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE physical_calls SET state=?,outcome_kind=?,failure_code=?,failure_class=?,failure_json=?,provider_request_id=?,response_digest=?,completed_at=?,complete_idempotency_key=?,complete_command_digest=? WHERE attempt_call_id=?`, completion.State, completion.Outcome, code, class, payload, provider, response, formatTime(completion.At), completion.IdempotencyKey, digest, completion.AttemptCallID); err != nil {
		return err
	}
	stored, err := readPhysicalCall(ctx, tx, completion.AttemptCallID)
	if err != nil {
		return err
	}
	return stored.Validate()
}

// finishDiscardedLLMResponses closes local writes when a stage rejects its
// output. Published canonical bytes are still charged even though their pins
// are released for GC. Unpublished staging slots release their reservation.
// The release preflight requires terminal provider parents before entering here.
func finishDiscardedLLMResponses(ctx context.Context, tx *immediateTx, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT call_record_id FROM call_records WHERE run_id=? AND stage_name=? AND attempt_id=? AND provider='private-blob' AND state IN ('OPEN','PREPARED')
		AND ((call_kind='LLM_GENERATE' AND (logical_operation_id LIKE 'llm-response:%' OR logical_operation_id LIKE 'idea-batch-output:%')) OR (call_kind='SIMILARITY_SEARCH' AND logical_operation_id LIKE 'similarity-response:%'))`, runID, stage, attempt)
	if err != nil {
		return err
	}
	var ids []domain.CallRecordID
	for rows.Next() {
		var id domain.CallRecordID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	run, err := readRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		call, err := readCallRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		parentID, ok := privateResponseParent(call)
		if !ok {
			return errors.New("invalid private response parent")
		}
		parent, err := readCallRecord(ctx, tx, parentID)
		if err != nil {
			return err
		}
		if parent.RunID != runID || parent.StageName != stage || parent.AttemptID != attempt || parent.Kind != call.Kind || parent.Provider == "private-blob" {
			return errors.New("discarded response has a mismatched provider parent")
		}
		if parent.State != domain.CallRecordTerminal {
			return wrap(ErrInvalidTransition, "discarded private response parent is not reconciled", nil)
		}
		if call.State == domain.CallRecordOpen {
			if _, err := finishSettledLLMArtifactCall(ctx, tx, call, run.Version, at); err != nil {
				return err
			}
			continue
		}
		prepared, err := readPreparedCalls(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, physical := range prepared.PhysicalCalls {
			if physical.Kind != domain.PhysicalLocalArtifactWrite {
				return errors.New("discarded response contains a provider call")
			}
			if physical.State == domain.PhysicalCompleted || physical.State == domain.PhysicalAbortedNoDispatch {
				continue
			}
			if physical.State != domain.PhysicalPrepared && physical.State != domain.PhysicalDispatching && physical.State != domain.PhysicalSent {
				return errors.New("discarded response has an unexpected boundary")
			}
			var reservation *domain.BudgetReservation
			for i := range prepared.Reservations {
				if prepared.Reservations[i].AttemptCallID == physical.ID {
					if reservation != nil {
						return errors.New("response has multiple byte reservations")
					}
					reservation = &prepared.Reservations[i]
				}
			}
			if reservation == nil || reservation.Dimension != domain.BudgetArtifactPhysicalNewBytes || reservation.State != domain.ReservationReserved {
				return errors.New("discarded response lacks its reserved byte account")
			}
			completion := domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: run.Version, StageName: stage, AttemptID: attempt, CallRecordID: id, AttemptCallID: physical.ID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: llmArtifactMutationID("discard", string(physical.ID)), At: at}
			if physical.State == domain.PhysicalSent {
				var ref domain.BlobRef
				var token domain.ArtifactWriterTokenID
				var physicalBytes int64
				if err := tx.QueryRowContext(ctx, `SELECT token.final_digest,token.final_size,token.writer_token_id,pin.physical_new_bytes FROM artifact_declarations decl JOIN artifact_writer_tokens token ON token.declaration_id=decl.declaration_id JOIN blob_pins pin ON pin.pin_id=token.pin_id WHERE decl.attempt_call_id=? AND token.state='RELEASED' AND pin.state='RELEASED'`, physical.ID).Scan(&ref.Digest, &ref.Size, &token, &physicalBytes); err != nil {
					return err
				}
				if err := settleReservation(ctx, tx, *reservation, physicalBytes, domain.ReservationSettled, at); err != nil {
					return err
				}
				completion.State, completion.Outcome, completion.Failure = domain.PhysicalCompleted, domain.PhysicalOutcomeSuccess, nil
				completion.ProviderRequestID, completion.ResponseDigest = "local-artifact:"+string(token), &ref.Digest
				completion.Usage = []domain.ReservationUsage{{ReservationID: reservation.ID, Dimension: reservation.Dimension, Subkey: reservation.Subkey, Value: physicalBytes, Verified: true}}
			} else {
				var active int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifact_writer_tokens token JOIN artifact_declarations decl ON decl.declaration_id=token.declaration_id WHERE decl.attempt_call_id=? AND token.state<>'RELEASED'`, physical.ID).Scan(&active); err != nil {
					return err
				}
				if active != 0 {
					return errors.New("unpublished response still has an active writer")
				}
				if err := settleReservation(ctx, tx, *reservation, 0, domain.ReservationReleased, at); err != nil {
					return err
				}
			}
			if err := writeLLMArtifactCompletion(ctx, tx, completion); err != nil {
				return err
			}
		}
		if _, err := finishSettledLLMArtifactCall(ctx, tx, prepared.Call, run.Version, at); err != nil {
			return err
		}
	}
	return nil
}

func llmArtifactMutationID(prefix, identity string) string {
	digest := strings.TrimPrefix(string(domain.SumBytes([]byte("cpgen.llm-artifact-completion/v1\n"+prefix+"\n"+identity))), "sha256:")
	return prefix + "_" + digest[:32]
}

// finishSettledLLMArtifactCall leaves any live publication or byte reservation
// untouched. All unused slots can close before stage commit; retained writes
// can close only after atomic occurrence attachment has settled their bytes.
func finishSettledLLMArtifactCall(ctx context.Context, tx *immediateTx, call domain.CallRecord, version int64, at time.Time) (bool, error) {
	if call.State == domain.CallRecordTerminal {
		return true, nil
	}
	var prepared domain.PreparedCalls
	if call.State == domain.CallRecordOpen {
		// The parent may have stopped before local reservation preparation.
		// Keep LoadCall's prepared-bundle contract strict and prove this empty
		// child has no physical rows or reservations before no-send completion.
		var effects int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM physical_calls WHERE call_record_id=?) + (SELECT count(*) FROM budget_reservations WHERE call_record_id=?)`, call.ID, call.ID).Scan(&effects); err != nil {
			return false, err
		}
		if effects != 0 {
			return false, errors.New("open private response has prepared effects")
		}
	} else {
		var err error
		prepared, err = readPreparedCalls(ctx, tx, call.ID)
		if err != nil {
			return false, err
		}
	}
	for _, reservation := range prepared.Reservations {
		if reservation.State == domain.ReservationReserved {
			return false, nil
		}
	}
	var resultID *domain.AttemptCallID
	for _, physical := range prepared.PhysicalCalls {
		if physical.Kind != domain.PhysicalLocalArtifactWrite {
			return false, errors.New("local response completion contains a provider call")
		}
		switch physical.State {
		case domain.PhysicalCompleted:
			if physical.Outcome == nil || *physical.Outcome != domain.PhysicalOutcomeSuccess {
				return false, errors.New("local response publication did not succeed")
			}
			id := physical.ID
			resultID = &id
		case domain.PhysicalAbortedNoDispatch:
		default:
			return false, nil
		}
	}
	finish := domain.FinishCallRequest{RunID: call.RunID, ExpectedRunVersion: version, StageName: call.StageName, AttemptID: call.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: resultID, IdempotencyKey: llmArtifactMutationID("finish", string(call.ID)), At: at}
	if resultID == nil {
		finish.DispatchKind = domain.DispatchNone
		finish.Failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	}
	if err := finish.Validate(); err != nil {
		return false, err
	}
	if err := validateFinishProjection(ctx, tx, finish); err != nil {
		return false, err
	}
	digest, _, err := digestJSON(finish)
	if err != nil {
		return false, err
	}
	code, class, payload, err := failureColumns(finish.Failure)
	if err != nil {
		return false, err
	}
	var result any
	if resultID != nil {
		result = string(*resultID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE call_records SET state='TERMINAL',dispatch_kind=?,result_attempt_call_id=?,failure_code=?,failure_class=?,failure_json=?,completed_at=?,finish_idempotency_key=?,finish_command_digest=? WHERE call_record_id=?`, finish.DispatchKind, result, code, class, payload, formatTime(at), finish.IdempotencyKey, digest, call.ID); err != nil {
		return false, err
	}
	stored, err := readCallRecord(ctx, tx, call.ID)
	if err != nil {
		return false, err
	}
	if err := stored.Validate(); err != nil {
		return false, err
	}
	_, err = callTraceForRecord(ctx, tx, call.ID)
	return err == nil, err
}
