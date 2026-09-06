package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"cpgen/internal/domain"
)

func insertInitialBudgetAccounts(ctx context.Context, tx *immediateTx, runID domain.RunID, requestDigest domain.Digest, limits domain.BudgetLimits) error {
	values := []struct {
		dimension domain.BudgetDimension
		limit     int64
	}{
		{domain.BudgetLLMCalls, limits.MaxLLMCalls},
		{domain.BudgetLLMInputTokens, limits.MaxLLMInputTokens},
		{domain.BudgetLLMOutputTokens, limits.MaxLLMOutputTokens},
		{domain.BudgetExternalCostMicroUSD, limits.MaxLLMCostMicroUSD},
		{domain.BudgetSimilarityCalls, limits.MaxSimilarityCalls},
		{domain.BudgetSimilarityCostMicroUSD, limits.MaxSimilarityCostMicroUSD},
		{domain.BudgetDockerContainerCreates, limits.MaxSandboxCreates},
		{domain.BudgetArtifactPhysicalNewBytes, limits.MaxArtifactBytes},
		{domain.BudgetActiveTimeNS, limits.MaxActiveTimeMilliseconds * int64(time.Millisecond)},
	}
	for _, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO budget_accounts(
			run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version
		) VALUES (?, ?, ?, ?, 0, 0, 1)`, runID, requestDigest, value.dimension, value.limit); err != nil {
			return fmt.Errorf("insert initial %s budget account: %w", value.dimension, err)
		}
	}
	return nil
}

func (s *Store) OpenCall(ctx context.Context, request domain.OpenCallRequest) (domain.CallRecord, error) {
	if err := request.Validate(); err != nil {
		return domain.CallRecord{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.CallRecord{}, err
	}
	var result domain.CallRecord
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var storedID string
		var storedDigest string
		err := tx.QueryRowContext(ctx, `SELECT call_record_id, open_command_digest FROM call_records
			WHERE run_id = ? AND open_idempotency_key = ?`, request.RunID, request.IdempotencyKey).Scan(&storedID, &storedDigest)
		if err == nil {
			if storedDigest != string(commandDigest) {
				return wrap(ErrConsistency, "logical call idempotency key was reused with different content", nil)
			}
			result, err = readCallRecord(ctx, tx, domain.CallRecordID(storedID))
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := validateMeteringContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		if err := ensureCallIdentityAvailable(ctx, tx, request); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO call_records(
				call_record_id, run_id, stage_name, attempt_id, logical_operation_id, call_kind, provider,
				request_digest, policy_digest, retry_max_attempts, retry_initial_backoff_ns, retry_max_backoff_ns,
				retry_jitter_seed_digest, state, opened_at, open_idempotency_key, open_command_digest
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'OPEN', ?, ?, ?)`,
			request.ID, request.RunID, request.StageName, request.AttemptID, request.LogicalOperationID,
			request.Kind, request.Provider, request.RequestDigest, request.PolicyDigest,
			request.RetryPolicy.MaxAttempts, int64(request.RetryPolicy.InitialBackoff), int64(request.RetryPolicy.MaxBackoff),
			request.RetryPolicy.JitterSeedDigest, formatTime(request.At), request.IdempotencyKey, commandDigest,
		)
		if err != nil {
			return err
		}
		result, err = readCallRecord(ctx, tx, request.ID)
		return err
	})
	return result, err
}

func (s *Store) LoadCall(ctx context.Context, recordID domain.CallRecordID) (domain.PreparedCalls, error) {
	if err := recordID.Validate(); err != nil {
		return domain.PreparedCalls{}, err
	}
	var result domain.PreparedCalls
	err := s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		result, err = readPreparedCalls(ctx, tx, recordID)
		return err
	})
	return result, err
}

func (s *Store) PrepareCalls(ctx context.Context, request domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	if err := request.Validate(); err != nil {
		return domain.PreparedCalls{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.PreparedCalls{}, err
	}
	var result domain.PreparedCalls
	err = s.immediate(ctx, func(tx *immediateTx) error {
		call, err := readCallRecord(ctx, tx, request.CallRecordID)
		if err != nil {
			return err
		}
		var storedKey, storedDigest sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT prepare_idempotency_key, prepare_command_digest
			FROM call_records WHERE call_record_id = ?`, request.CallRecordID).Scan(&storedKey, &storedDigest); err != nil {
			return err
		}
		if storedKey.Valid {
			if storedKey.String != request.IdempotencyKey || storedDigest.String != string(commandDigest) {
				return wrap(ErrConsistency, "physical plan idempotency was reused with different content", nil)
			}
			result, err = readPreparedCalls(ctx, tx, request.CallRecordID)
			return err
		}
		if err := validateMeteringContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		if call.RunID != request.RunID || call.StageName != request.StageName || call.AttemptID != request.AttemptID {
			return wrap(ErrConsistency, "physical plan does not match its logical call", nil)
		}
		if call.State != domain.CallRecordOpen {
			return wrap(ErrInvalidTransition, "only an OPEN logical call can prepare physical calls", nil)
		}
		if int64(len(request.Calls)) > call.RetryPolicy.MaxAttempts {
			return wrap(ErrConsistency, "physical plan exceeds persisted retry policy", nil)
		}
		reservedByDimension, err := aggregateReservations(request.Calls)
		if err != nil {
			return err
		}
		exhausted, err := budgetWouldExhaust(ctx, tx, request.RunID, reservedByDimension)
		if err != nil {
			return err
		}
		if exhausted {
			failure := domain.PortFailure{Code: domain.FailureBudgetExhausted, Class: domain.FailureRejected}
			failureJSON, err := json.Marshal(failure)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE call_records SET
				state = 'TERMINAL', dispatch_kind = 'NO_DISPATCH', failure_code = ?, failure_class = ?, failure_json = ?,
				completed_at = ?, prepare_idempotency_key = ?, prepare_command_digest = ?,
				finish_idempotency_key = ?, finish_command_digest = ?
				WHERE call_record_id = ? AND state = 'OPEN'`,
				failure.Code, failure.Class, failureJSON, formatTime(request.At), request.IdempotencyKey, commandDigest,
				request.IdempotencyKey, commandDigest, request.CallRecordID,
			); err != nil {
				return err
			}
			call, err = readCallRecord(ctx, tx, request.CallRecordID)
			if err != nil {
				return err
			}
			trace := domain.CallTrace{LogicalOperationID: call.LogicalOperationID, DispatchKind: domain.DispatchNone}
			result = domain.PreparedCalls{Call: call, Failure: &failure, CallTrace: &trace}
			return result.Validate()
		}
		if err := reserveBudget(ctx, tx, request.RunID, reservedByDimension); err != nil {
			return err
		}
		for _, planned := range request.Calls {
			if _, err := tx.ExecContext(ctx, `INSERT INTO physical_calls(
				attempt_call_id, call_record_id, run_id, stage_name, attempt_id, ordinal, retry_group, retry_ordinal,
				physical_kind, provider, request_digest, idempotency_key, state, prepared_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PREPARED', ?)`,
				planned.ID, request.CallRecordID, request.RunID, request.StageName, request.AttemptID,
				planned.Ordinal, planned.RetryGroup, planned.RetryOrdinal, planned.Kind, planned.Provider,
				planned.RequestDigest, planned.IdempotencyKey, formatTime(request.At),
			); err != nil {
				return err
			}
			for _, reservation := range planned.Reservations {
				if _, err := tx.ExecContext(ctx, `INSERT INTO budget_reservations(
					reservation_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id, physical_kind,
					dimension, subkey, upper_bound, state, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'RESERVED', ?)`,
					reservation.ID, request.RunID, request.StageName, request.AttemptID, request.CallRecordID,
					planned.ID, planned.Kind, reservation.Dimension, reservation.Subkey, reservation.UpperBound, formatTime(request.At),
				); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE call_records SET state = 'PREPARED', prepared_at = ?,
			prepare_idempotency_key = ?, prepare_command_digest = ? WHERE call_record_id = ? AND state = 'OPEN'`,
			formatTime(request.At), request.IdempotencyKey, commandDigest, request.CallRecordID,
		); err != nil {
			return err
		}
		result, err = readPreparedCalls(ctx, tx, request.CallRecordID)
		return err
	})
	return result, err
}

func (s *Store) BeginDispatch(ctx context.Context, request domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	if err := request.Validate(); err != nil {
		return domain.DispatchGrant{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.DispatchGrant{}, err
	}
	var result domain.DispatchGrant
	err = s.immediate(ctx, func(tx *immediateTx) error {
		physical, err := readPhysicalCall(ctx, tx, request.AttemptCallID)
		if err != nil {
			return err
		}
		var storedKey, storedDigest sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT begin_idempotency_key, begin_command_digest FROM physical_calls
			WHERE attempt_call_id = ?`, request.AttemptCallID).Scan(&storedKey, &storedDigest); err != nil {
			return err
		}
		if storedKey.Valid {
			if storedKey.String != request.IdempotencyKey || storedDigest.String != string(commandDigest) {
				return wrap(ErrConsistency, "dispatch idempotency was reused with different content", nil)
			}
			result, err = readDispatchGrant(ctx, tx, request.ExpectedRunVersion, request.AttemptCallID)
			return err
		}
		if err := validateMeteringContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		if physical.CallRecordID != request.CallRecordID || physical.RunID != request.RunID || physical.StageName != request.StageName || physical.AttemptID != request.AttemptID {
			return wrap(ErrConsistency, "dispatch request does not match physical call", nil)
		}
		if physical.State != domain.PhysicalPrepared {
			return wrap(ErrInvalidTransition, "only a PREPARED physical call can begin dispatch", nil)
		}
		var callState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM call_records WHERE call_record_id = ?`, request.CallRecordID).Scan(&callState); err != nil {
			return err
		}
		if callState != string(domain.CallRecordPrepared) {
			return wrap(ErrInvalidTransition, "logical call is not prepared", nil)
		}
		var unknownCount int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM physical_calls
			WHERE call_record_id = ? AND state = 'UNKNOWN'`, physical.CallRecordID).Scan(&unknownCount); err != nil {
			return err
		}
		if unknownCount != 0 {
			return wrap(ErrInvalidTransition, "an UNKNOWN physical boundary blocks every new key for the logical call", nil)
		}
		if physical.Ordinal > 1 {
			var previousState, previousOutcome string
			err := tx.QueryRowContext(ctx, `SELECT state, outcome_kind FROM physical_calls
				WHERE call_record_id = ? AND ordinal = ?`,
				physical.CallRecordID, physical.Ordinal-1,
			).Scan(&previousState, &previousOutcome)
			if err != nil {
				return wrap(ErrConsistency, "retry predecessor does not exist", err)
			}
			if !(previousState == string(domain.PhysicalAbortedNoDispatch) ||
				(previousState == string(domain.PhysicalCompleted) && previousOutcome == string(domain.PhysicalOutcomeRetryableFailure))) {
				return wrap(ErrInvalidTransition, "retry predecessor does not authorize a new physical key", nil)
			}
		}
		result = dispatchGrantFromPhysical(request.ExpectedRunVersion, request.IdempotencyKey, request.At, physical)
		grantDigest, err := calculateGrantDigest(result)
		if err != nil {
			return err
		}
		result.GrantDigest = grantDigest
		if _, err := tx.ExecContext(ctx, `UPDATE physical_calls SET state = 'DISPATCHING', dispatch_started_at = ?,
			begin_idempotency_key = ?, begin_command_digest = ?, grant_digest = ?
			WHERE attempt_call_id = ? AND state = 'PREPARED'`,
			formatTime(request.At), request.IdempotencyKey, commandDigest, grantDigest, request.AttemptCallID,
		); err != nil {
			return err
		}
		return result.Validate()
	})
	return result, err
}

func (s *Store) ResumeDispatch(ctx context.Context, expectedRunVersion int64, attemptCallID domain.AttemptCallID) (domain.DispatchGrant, error) {
	if expectedRunVersion <= 0 {
		return domain.DispatchGrant{}, errors.New("expected run version must be positive")
	}
	if err := attemptCallID.Validate(); err != nil {
		return domain.DispatchGrant{}, err
	}
	var result domain.DispatchGrant
	err := s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		result, err = readDispatchGrant(ctx, tx, expectedRunVersion, attemptCallID)
		return err
	})
	return result, err
}

func (s *Store) MarkSent(ctx context.Context, grant domain.DispatchGrant, sentAt time.Time) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	if sentAt.IsZero() || sentAt.Location() != time.UTC {
		return errors.New("sent at must be a canonical UTC timestamp")
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		physical, err := readPhysicalCall(ctx, tx, grant.AttemptCallID)
		if err != nil {
			return err
		}
		storedGrant, err := readDispatchGrant(ctx, tx, grant.ExpectedRunVersion, grant.AttemptCallID)
		if err != nil {
			return err
		}
		if !dispatchGrantsEqual(storedGrant, grant) {
			return wrap(ErrConsistency, "dispatch grant does not match persisted authorization", nil)
		}
		if physical.State == domain.PhysicalSent || physical.State == domain.PhysicalCompleted {
			if physical.SentAt == nil || !physical.SentAt.Equal(sentAt) {
				return wrap(ErrConsistency, "sent boundary replay changed its timestamp", nil)
			}
			return nil
		}
		if err := validateMeteringContext(ctx, tx, grant.RunID, grant.ExpectedRunVersion, grant.StageName, grant.AttemptID); err != nil {
			return err
		}
		if physical.State != domain.PhysicalDispatching || sentAt.Before(grant.DispatchStartedAt) {
			return wrap(ErrInvalidTransition, "physical call is not at a confirmable send boundary", nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE physical_calls SET state = 'SENT', sent_at = ?
			WHERE attempt_call_id = ? AND state = 'DISPATCHING'`, formatTime(sentAt), grant.AttemptCallID)
		return err
	})
}

func (s *Store) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		physical, err := readPhysicalCall(ctx, tx, request.AttemptCallID)
		if err != nil {
			return err
		}
		var storedKey, storedDigest sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT complete_idempotency_key, complete_command_digest
			FROM physical_calls WHERE attempt_call_id = ?`, request.AttemptCallID).Scan(&storedKey, &storedDigest); err != nil {
			return err
		}
		if storedKey.Valid {
			if storedKey.String != request.IdempotencyKey || storedDigest.String != string(commandDigest) {
				return wrap(ErrConsistency, "physical completion idempotency was reused with different content", nil)
			}
			return nil
		}
		// A call may have crossed the external boundary immediately before a
		// cancellation request. Terminal settlement is cleanup work and must
		// remain possible while the run is still in its current attempt; new
		// dispatches continue to use the cancel-rejecting guard above.
		if err := validateMeteringSettlementContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		if physical.CallRecordID != request.CallRecordID || physical.RunID != request.RunID || physical.StageName != request.StageName || physical.AttemptID != request.AttemptID {
			return wrap(ErrConsistency, "physical completion does not match its persisted identity", nil)
		}
		switch request.State {
		case domain.PhysicalCompleted:
			if physical.State != domain.PhysicalSent {
				return wrap(ErrInvalidTransition, "COMPLETED requires a SENT physical call", nil)
			}
		case domain.PhysicalAbortedNoDispatch:
			if physical.State != domain.PhysicalPrepared && physical.State != domain.PhysicalDispatching {
				return wrap(ErrInvalidTransition, "ABORTED_NO_DISPATCH requires PREPARED or DISPATCHING", nil)
			}
		case domain.PhysicalUnknown:
			if physical.State != domain.PhysicalDispatching && physical.State != domain.PhysicalSent {
				return wrap(ErrInvalidTransition, "UNKNOWN requires DISPATCHING or SENT", nil)
			}
		}
		if request.At.Before(physical.PreparedAt) {
			return wrap(ErrConsistency, "physical completion precedes preparation", nil)
		}
		if err := settlePhysicalReservations(ctx, tx, physical, request); err != nil {
			return err
		}
		failureCode, failureClass, failureJSON, err := failureColumns(request.Failure)
		if err != nil {
			return err
		}
		var providerID any
		if request.ProviderRequestID != "" {
			providerID = request.ProviderRequestID
		}
		var responseDigest any
		if request.ResponseDigest != nil {
			responseDigest = string(*request.ResponseDigest)
		}
		_, err = tx.ExecContext(ctx, `UPDATE physical_calls SET state = ?, outcome_kind = ?, failure_code = ?, failure_class = ?, failure_json = ?,
			provider_request_id = ?, response_digest = ?, completed_at = ?, complete_idempotency_key = ?, complete_command_digest = ?
			WHERE attempt_call_id = ?`,
			request.State, request.Outcome, failureCode, failureClass, failureJSON, providerID, responseDigest,
			formatTime(request.At), request.IdempotencyKey, commandDigest, request.AttemptCallID,
		)
		if err != nil {
			return err
		}
		stored, err := readPhysicalCall(ctx, tx, request.AttemptCallID)
		if err != nil {
			return err
		}
		return stored.Validate()
	})
}

func (s *Store) FinishCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	if err := request.Validate(); err != nil {
		return domain.CallTrace{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.CallTrace{}, err
	}
	var trace domain.CallTrace
	err = s.immediate(ctx, func(tx *immediateTx) error {
		call, err := readCallRecord(ctx, tx, request.CallRecordID)
		if err != nil {
			return err
		}
		var storedKey, storedDigest sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT finish_idempotency_key, finish_command_digest
			FROM call_records WHERE call_record_id = ?`, request.CallRecordID).Scan(&storedKey, &storedDigest); err != nil {
			return err
		}
		if storedKey.Valid {
			if storedKey.String != request.IdempotencyKey || storedDigest.String != string(commandDigest) {
				return wrap(ErrConsistency, "logical completion idempotency was reused with different content", nil)
			}
			trace, err = callTraceForRecord(ctx, tx, request.CallRecordID)
			return err
		}
		if err := validateMeteringContext(ctx, tx, request.RunID, request.ExpectedRunVersion, request.StageName, request.AttemptID); err != nil {
			return err
		}
		if call.RunID != request.RunID || call.StageName != request.StageName || call.AttemptID != request.AttemptID || call.State == domain.CallRecordTerminal {
			return wrap(ErrConsistency, "logical completion does not match an active call", nil)
		}
		var inflight int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM physical_calls
			WHERE call_record_id = ? AND state IN ('DISPATCHING','SENT')`, request.CallRecordID).Scan(&inflight); err != nil {
			return err
		}
		if inflight != 0 {
			return wrap(ErrInvalidTransition, "logical call has an in-flight physical boundary", nil)
		}
		if err := validateFinishProjection(ctx, tx, request); err != nil {
			return err
		}
		if err := releaseUnusedReservations(ctx, tx, request); err != nil {
			return err
		}
		failureCode, failureClass, failureJSON, err := failureColumns(request.Failure)
		if err != nil {
			return err
		}
		var resultID, sourceID, hitID any
		if request.ResultAttemptCallID != nil {
			resultID = string(*request.ResultAttemptCallID)
		}
		if request.CacheSourceCallRecordID != nil {
			sourceID = string(*request.CacheSourceCallRecordID)
		}
		if request.CacheHitCallRecordID != nil {
			hitID = string(*request.CacheHitCallRecordID)
		}
		_, err = tx.ExecContext(ctx, `UPDATE call_records SET state = 'TERMINAL', dispatch_kind = ?,
			result_attempt_call_id = ?, cache_source_call_record_id = ?, cache_hit_call_record_id = ?,
			failure_code = ?, failure_class = ?, failure_json = ?, completed_at = ?,
			finish_idempotency_key = ?, finish_command_digest = ? WHERE call_record_id = ?`,
			request.DispatchKind, resultID, sourceID, hitID, failureCode, failureClass, failureJSON,
			formatTime(request.At), request.IdempotencyKey, commandDigest, request.CallRecordID,
		)
		if err != nil {
			return err
		}
		call, err = readCallRecord(ctx, tx, request.CallRecordID)
		if err != nil {
			return err
		}
		if err := call.Validate(); err != nil {
			return wrap(ErrConsistency, "terminal logical call projection is invalid", err)
		}
		trace, err = callTraceForRecord(ctx, tx, request.CallRecordID)
		return err
	})
	return trace, err
}

func validateMeteringContext(ctx context.Context, tx *immediateTx, runID domain.RunID, expectedVersion int64, stage domain.StageName, attemptID domain.AttemptID) error {
	return validateMeteringContextMode(ctx, tx, runID, expectedVersion, stage, attemptID, true)
}

func validateMeteringSettlementContext(ctx context.Context, tx *immediateTx, runID domain.RunID, expectedVersion int64, stage domain.StageName, attemptID domain.AttemptID) error {
	return validateMeteringContextMode(ctx, tx, runID, expectedVersion, stage, attemptID, false)
}

func validateMeteringContextMode(ctx context.Context, tx *immediateTx, runID domain.RunID, expectedVersion int64, stage domain.StageName, attemptID domain.AttemptID, rejectCancel bool) error {
	run, err := readRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if run.Version != expectedVersion {
		return wrap(ErrVersionConflict, "metering expected run version does not match", nil)
	}
	if run.State != domain.RunRunning || run.CurrentStage != stage {
		return wrap(ErrInvalidTransition, "metering requires the current RUNNING stage", nil)
	}
	var stageState, currentAttempt, attemptState string
	err = tx.QueryRowContext(ctx, `SELECT stage.state, stage.current_attempt_id, attempt.state
		FROM stage_records stage JOIN stage_attempts attempt
		  ON attempt.run_id = stage.run_id AND attempt.stage_name = stage.stage_name AND attempt.attempt_id = stage.current_attempt_id
		WHERE stage.run_id = ? AND stage.stage_name = ?`, runID, stage).Scan(&stageState, &currentAttempt, &attemptState)
	if err != nil {
		return wrap(ErrConsistency, "current stage attempt is missing", err)
	}
	if stageState != string(domain.StageRunning) || currentAttempt != string(attemptID) || attemptState != string(domain.StageAttemptRunning) {
		return wrap(ErrInvalidTransition, "metering requires the current RUNNING stage attempt", nil)
	}
	if rejectCancel {
		return ValidateNoPendingCancel(ctx, tx, runID)
	}
	return nil
}

func ensureCallIdentityAvailable(ctx context.Context, tx *immediateTx, request domain.OpenCallRequest) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM call_records
		WHERE call_record_id = ? OR (run_id = ? AND logical_operation_id = ?)`,
		request.ID, request.RunID, request.LogicalOperationID,
	).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return wrap(ErrConsistency, "logical call identity already belongs to different content", nil)
	}
	return nil
}

func aggregateReservations(calls []domain.PhysicalCallPlan) (map[domain.BudgetDimension]int64, error) {
	result := make(map[domain.BudgetDimension]int64)
	seenReservationIDs := make(map[domain.ReservationID]struct{})
	seenPhysicalKeys := make(map[string]struct{})
	for _, call := range calls {
		if _, exists := seenPhysicalKeys[call.IdempotencyKey]; exists {
			return nil, wrap(ErrConsistency, "physical idempotency key is duplicated in the fixed plan", nil)
		}
		seenPhysicalKeys[call.IdempotencyKey] = struct{}{}
		for _, reservation := range call.Reservations {
			if _, exists := seenReservationIDs[reservation.ID]; exists {
				return nil, wrap(ErrConsistency, "reservation ID is duplicated in the fixed plan", nil)
			}
			seenReservationIDs[reservation.ID] = struct{}{}
			current := result[reservation.Dimension]
			if current > int64(^uint64(0)>>1)-reservation.UpperBound {
				return nil, wrap(ErrConsistency, "reservation bundle overflows an integer account", nil)
			}
			result[reservation.Dimension] = current + reservation.UpperBound
		}
	}
	return result, nil
}

func sortedDimensions(values map[domain.BudgetDimension]int64) []domain.BudgetDimension {
	result := make([]domain.BudgetDimension, 0, len(values))
	for dimension := range values {
		result = append(result, dimension)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func budgetWouldExhaust(ctx context.Context, tx *immediateTx, runID domain.RunID, requested map[domain.BudgetDimension]int64) (bool, error) {
	for _, dimension := range sortedDimensions(requested) {
		var limit, reserved, consumed int64
		if err := tx.QueryRowContext(ctx, `SELECT limit_value, reserved_value, consumed_value
			FROM budget_accounts WHERE run_id = ? AND dimension = ?`, runID, dimension).Scan(&limit, &reserved, &consumed); err != nil {
			return false, wrap(ErrConsistency, "budget account is missing", err)
		}
		if consumed > limit || reserved > limit-consumed || requested[dimension] > limit-consumed-reserved {
			return true, nil
		}
	}
	return false, nil
}

func reserveBudget(ctx context.Context, tx *immediateTx, runID domain.RunID, requested map[domain.BudgetDimension]int64) error {
	for _, dimension := range sortedDimensions(requested) {
		amount := requested[dimension]
		result, err := tx.ExecContext(ctx, `UPDATE budget_accounts SET
			reserved_value = reserved_value + ?, account_version = account_version + 1
			WHERE run_id = ? AND dimension = ? AND ? <= limit_value - consumed_value - reserved_value`,
			amount, runID, dimension, amount,
		)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return wrap(ErrConsistency, "budget changed after preflight in one transaction", nil)
		}
	}
	return nil
}

func dispatchGrantFromPhysical(expectedVersion int64, key string, at time.Time, physical domain.PhysicalCall) domain.DispatchGrant {
	return domain.DispatchGrant{
		RunID: physical.RunID, ExpectedRunVersion: expectedVersion, StageName: physical.StageName,
		AttemptID: physical.AttemptID, CallRecordID: physical.CallRecordID, AttemptCallID: physical.ID,
		Ordinal: physical.Ordinal, RetryGroup: physical.RetryGroup, RetryOrdinal: physical.RetryOrdinal,
		Kind: physical.Kind, Provider: physical.Provider, RequestDigest: physical.RequestDigest,
		IdempotencyKey: key, DispatchStartedAt: at,
	}
}

func calculateGrantDigest(grant domain.DispatchGrant) (domain.Digest, error) {
	grant.GrantDigest = ""
	digest, _, err := digestJSON(grant)
	return digest, err
}

func readDispatchGrant(ctx context.Context, queryer rowQuerier, expectedVersion int64, id domain.AttemptCallID) (domain.DispatchGrant, error) {
	physical, err := readPhysicalCall(ctx, queryer, id)
	if err != nil {
		return domain.DispatchGrant{}, err
	}
	var key, started, digest string
	if err := queryer.QueryRowContext(ctx, `SELECT begin_idempotency_key, dispatch_started_at, grant_digest
		FROM physical_calls WHERE attempt_call_id = ?`, id).Scan(&key, &started, &digest); err != nil {
		return domain.DispatchGrant{}, err
	}
	parsed, err := parseTime(started)
	if err != nil {
		return domain.DispatchGrant{}, err
	}
	grant := dispatchGrantFromPhysical(expectedVersion, key, parsed, physical)
	grant.GrantDigest = domain.Digest(digest)
	if err := grant.Validate(); err != nil {
		return domain.DispatchGrant{}, wrap(ErrConsistency, "stored dispatch grant is invalid", err)
	}
	return grant, nil
}

func dispatchGrantsEqual(left, right domain.DispatchGrant) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	return err == nil && string(leftJSON) == string(rightJSON)
}

func settlePhysicalReservations(ctx context.Context, tx *immediateTx, physical domain.PhysicalCall, request domain.CompletePhysicalRequest) error {
	reservations, err := readReservationsForPhysical(ctx, tx, physical.ID)
	if err != nil {
		return err
	}
	usageByID := make(map[domain.ReservationID]domain.ReservationUsage, len(request.Usage))
	for _, usage := range request.Usage {
		usageByID[usage.ReservationID] = usage
	}
	for _, reservation := range reservations {
		if reservation.State != domain.ReservationReserved {
			return wrap(ErrConsistency, "physical call has a non-reserved unsettled reservation", nil)
		}
		settled := reservation.UpperBound
		state := domain.ReservationSettled
		if request.State == domain.PhysicalAbortedNoDispatch {
			settled, state = 0, domain.ReservationReleased
		} else if request.State == domain.PhysicalCompleted {
			if usage, exists := usageByID[reservation.ID]; exists && usage.Verified && usage.Dimension == reservation.Dimension && usage.Subkey == reservation.Subkey && usage.Value >= 0 && usage.Value <= reservation.UpperBound && !(fixedCountBudgetDimension(reservation.Dimension) && usage.Value == 0) {
				settled = usage.Value
			}
		}
		delete(usageByID, reservation.ID)
		if err := settleReservation(ctx, tx, reservation, settled, state, request.At); err != nil {
			return err
		}
	}
	if len(usageByID) != 0 {
		return wrap(ErrConsistency, "physical completion reports usage for an unrelated reservation", nil)
	}
	return nil
}

func fixedCountBudgetDimension(dimension domain.BudgetDimension) bool {
	return dimension == domain.BudgetLLMCalls || dimension == domain.BudgetSimilarityCalls || dimension == domain.BudgetDockerContainerCreates
}

func settleReservation(ctx context.Context, tx *immediateTx, reservation domain.BudgetReservation, settled int64, state domain.ReservationState, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE budget_accounts SET
		reserved_value = reserved_value - ?, consumed_value = consumed_value + ?, account_version = account_version + 1
		WHERE run_id = ? AND dimension = ? AND reserved_value >= ? AND ? <= limit_value - consumed_value`,
		reservation.UpperBound, settled, reservation.RunID, reservation.Dimension, reservation.UpperBound, settled,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return wrap(ErrConsistency, "budget reservation cannot settle against its account", nil)
	}
	_, err = tx.ExecContext(ctx, `UPDATE budget_reservations SET state = ?, settled_value = ?, settled_at = ?
		WHERE reservation_id = ? AND state = 'RESERVED'`, state, settled, formatTime(at), reservation.ID)
	return err
}

func validateFinishProjection(ctx context.Context, tx *immediateTx, request domain.FinishCallRequest) error {
	var dispatched int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM physical_calls
		WHERE call_record_id = ? AND (sent_at IS NOT NULL OR state = 'UNKNOWN')`, request.CallRecordID).Scan(&dispatched); err != nil {
		return err
	}
	switch request.DispatchKind {
	case domain.DispatchDispatched:
		if dispatched == 0 {
			return wrap(ErrInvalidTransition, "DISPATCHED call has no physical send boundary", nil)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM physical_calls
			WHERE attempt_call_id = ? AND call_record_id = ?`, *request.ResultAttemptCallID, request.CallRecordID).Scan(&state); err != nil {
			return wrap(ErrConsistency, "result physical call is outside its logical record", err)
		}
		if state != string(domain.PhysicalCompleted) && state != string(domain.PhysicalUnknown) {
			return wrap(ErrInvalidTransition, "result physical call is not terminal", nil)
		}
	case domain.DispatchCacheHit:
		var sourceState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM call_records WHERE call_record_id = ?`, *request.CacheSourceCallRecordID).Scan(&sourceState); err != nil {
			return wrap(ErrConsistency, "cache source logical call does not exist", err)
		}
		var physicalCount int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM physical_calls
			WHERE call_record_id = ?`, request.CallRecordID).Scan(&physicalCount); err != nil {
			return err
		}
		if sourceState != string(domain.CallRecordTerminal) {
			return wrap(ErrConsistency, "cache source logical call is not terminal", nil)
		}
		if physicalCount != 0 {
			return wrap(ErrInvalidTransition, "CACHE_HIT logical call must have zero physical rows", nil)
		}
	case domain.DispatchNone:
		if dispatched != 0 {
			return wrap(ErrInvalidTransition, "NO_DISPATCH call has a physical send boundary", nil)
		}
	}
	return nil
}

func releaseUnusedReservations(ctx context.Context, tx *immediateTx, request domain.FinishCallRequest) error {
	rows, err := tx.QueryContext(ctx, `SELECT reservation_id, run_id, stage_name, attempt_id, call_record_id,
		attempt_call_id, dimension, subkey, upper_bound, settled_value, state, created_at, settled_at
		FROM budget_reservations WHERE call_record_id = ? AND state = 'RESERVED' ORDER BY reservation_id`, request.CallRecordID)
	if err != nil {
		return err
	}
	var reservations []domain.BudgetReservation
	for rows.Next() {
		reservation, err := scanBudgetReservation(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, reservation := range reservations {
		if err := settleReservation(ctx, tx, reservation, 0, domain.ReservationReleased, request.At); err != nil {
			return err
		}
	}
	unusedFailure := request.Failure
	if unusedFailure == nil {
		unusedFailure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	}
	failureCode, failureClass, failureJSON, err := failureColumns(unusedFailure)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE physical_calls SET state = 'ABORTED_NO_DISPATCH', outcome_kind = 'NO_SEND',
		failure_code = ?, failure_class = ?, failure_json = ?, completed_at = ?,
		complete_idempotency_key = ? || ':' || ordinal, complete_command_digest = ?
		WHERE call_record_id = ? AND state = 'PREPARED'`,
		failureCode, failureClass, failureJSON, formatTime(request.At), request.IdempotencyKey, domain.SumBytes([]byte("unused:"+string(request.CallRecordID))), request.CallRecordID,
	)
	return err
}

func failureColumns(failure *domain.PortFailure) (any, any, any, error) {
	if failure == nil {
		return nil, nil, nil, nil
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		return nil, nil, nil, err
	}
	return string(failure.Code), string(failure.Class), encoded, nil
}

func callTraceForRecord(ctx context.Context, queryer rowQuerier, recordID domain.CallRecordID) (domain.CallTrace, error) {
	call, err := readCallRecord(ctx, queryer, recordID)
	if err != nil {
		return domain.CallTrace{}, err
	}
	if call.State != domain.CallRecordTerminal || call.DispatchKind == nil {
		return domain.CallTrace{}, wrap(ErrInvalidTransition, "logical call is not terminal", nil)
	}
	trace := domain.CallTrace{
		LogicalOperationID: call.LogicalOperationID, DispatchKind: *call.DispatchKind,
		ResultAttemptCallID: call.ResultAttemptCallID, CacheSourceCallRecordID: call.CacheSourceCallRecordID,
		CacheHitCallRecordID: call.CacheHitCallRecordID,
	}
	rows, err := queryer.(*immediateTx).QueryContext(ctx, `SELECT attempt_call_id FROM physical_calls
		WHERE call_record_id = ? AND (sent_at IS NOT NULL OR state = 'UNKNOWN') ORDER BY ordinal`, recordID)
	if err != nil {
		return domain.CallTrace{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id domain.AttemptCallID
		if err := rows.Scan(&id); err != nil {
			return domain.CallTrace{}, err
		}
		trace.PhysicalAttemptCallIDs = append(trace.PhysicalAttemptCallIDs, id)
	}
	if err := rows.Err(); err != nil {
		return domain.CallTrace{}, err
	}
	if err := trace.Validate(); err != nil {
		return domain.CallTrace{}, wrap(ErrConsistency, "stored call trace is invalid", err)
	}
	return trace, nil
}

func readPreparedCalls(ctx context.Context, tx *immediateTx, recordID domain.CallRecordID) (domain.PreparedCalls, error) {
	call, err := readCallRecord(ctx, tx, recordID)
	if err != nil {
		return domain.PreparedCalls{}, err
	}
	result := domain.PreparedCalls{Call: call}
	rows, err := tx.QueryContext(ctx, physicalCallSelect+` WHERE call_record_id = ? ORDER BY ordinal`, recordID)
	if err != nil {
		return domain.PreparedCalls{}, err
	}
	for rows.Next() {
		physical, err := scanPhysicalCall(rows)
		if err != nil {
			_ = rows.Close()
			return domain.PreparedCalls{}, err
		}
		result.PhysicalCalls = append(result.PhysicalCalls, physical)
	}
	if err := rows.Close(); err != nil {
		return domain.PreparedCalls{}, err
	}
	reservationRows, err := tx.QueryContext(ctx, `SELECT reservation_id, run_id, stage_name, attempt_id, call_record_id,
		attempt_call_id, dimension, subkey, upper_bound, settled_value, state, created_at, settled_at
		FROM budget_reservations WHERE call_record_id = ? ORDER BY attempt_call_id, dimension, subkey`, recordID)
	if err != nil {
		return domain.PreparedCalls{}, err
	}
	for reservationRows.Next() {
		reservation, err := scanBudgetReservation(reservationRows)
		if err != nil {
			_ = reservationRows.Close()
			return domain.PreparedCalls{}, err
		}
		result.Reservations = append(result.Reservations, reservation)
	}
	if err := reservationRows.Close(); err != nil {
		return domain.PreparedCalls{}, err
	}
	if call.State == domain.CallRecordTerminal {
		trace, err := callTraceForRecord(ctx, tx, recordID)
		if err != nil {
			return domain.PreparedCalls{}, err
		}
		result.Failure, result.CallTrace = call.Failure, &trace
	}
	return result, result.Validate()
}

const callRecordSelect = `SELECT call_record_id, run_id, stage_name, attempt_id, logical_operation_id,
	call_kind, provider, request_digest, policy_digest, retry_max_attempts, retry_initial_backoff_ns,
	retry_max_backoff_ns, retry_jitter_seed_digest, open_idempotency_key, state, dispatch_kind,
	result_attempt_call_id, cache_source_call_record_id, cache_hit_call_record_id, failure_json,
	opened_at, prepared_at, completed_at FROM call_records`

func readCallRecord(ctx context.Context, queryer rowQuerier, id domain.CallRecordID) (domain.CallRecord, error) {
	return scanCallRecord(queryer.QueryRowContext(ctx, callRecordSelect+` WHERE call_record_id = ?`, id))
}

type scanner interface{ Scan(...any) error }

func scanCallRecord(row scanner) (domain.CallRecord, error) {
	var result domain.CallRecord
	var kind, state string
	var requestDigest, policyDigest, jitterDigest string
	var initialNS, maxNS int64
	var dispatch, resultID, sourceID, hitID, failureJSON, preparedAt, completedAt sql.NullString
	var openedAt string
	err := row.Scan(&result.ID, &result.RunID, &result.StageName, &result.AttemptID, &result.LogicalOperationID,
		&kind, &result.Provider, &requestDigest, &policyDigest, &result.RetryPolicy.MaxAttempts, &initialNS,
		&maxNS, &jitterDigest, &result.IdempotencyKey, &state, &dispatch, &resultID, &sourceID, &hitID,
		&failureJSON, &openedAt, &preparedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CallRecord{}, wrap(ErrNotFound, "logical call does not exist", err)
	}
	if err != nil {
		return domain.CallRecord{}, err
	}
	result.Kind, result.State = domain.CallKind(kind), domain.CallRecordState(state)
	result.RequestDigest, result.PolicyDigest = domain.Digest(requestDigest), domain.Digest(policyDigest)
	result.RetryPolicy.InitialBackoff, result.RetryPolicy.MaxBackoff = time.Duration(initialNS), time.Duration(maxNS)
	result.RetryPolicy.JitterSeedDigest = domain.Digest(jitterDigest)
	if dispatch.Valid {
		value := domain.DispatchKind(dispatch.String)
		result.DispatchKind = &value
	}
	if resultID.Valid {
		value := domain.AttemptCallID(resultID.String)
		result.ResultAttemptCallID = &value
	}
	if sourceID.Valid {
		value := domain.CallRecordID(sourceID.String)
		result.CacheSourceCallRecordID = &value
	}
	if hitID.Valid {
		value := domain.CallRecordID(hitID.String)
		result.CacheHitCallRecordID = &value
	}
	if failureJSON.Valid {
		result.Failure = new(domain.PortFailure)
		if err := json.Unmarshal([]byte(failureJSON.String), result.Failure); err != nil {
			return domain.CallRecord{}, wrap(ErrConsistency, "stored logical failure is invalid", err)
		}
	}
	if result.OpenedAt, err = parseTime(openedAt); err != nil {
		return domain.CallRecord{}, err
	}
	if result.PreparedAt, err = parseOptionalTime(preparedAt); err != nil {
		return domain.CallRecord{}, err
	}
	if result.CompletedAt, err = parseOptionalTime(completedAt); err != nil {
		return domain.CallRecord{}, err
	}
	if err := result.Validate(); err != nil {
		return domain.CallRecord{}, wrap(ErrConsistency, "stored logical call is invalid", err)
	}
	return result, nil
}

const physicalCallSelect = `SELECT attempt_call_id, call_record_id, run_id, stage_name, attempt_id, ordinal,
	retry_group, retry_ordinal, physical_kind, provider, request_digest, idempotency_key, state, outcome_kind,
	failure_json, provider_request_id, response_digest, prepared_at, dispatch_started_at, sent_at, completed_at
	FROM physical_calls`

func readPhysicalCall(ctx context.Context, queryer rowQuerier, id domain.AttemptCallID) (domain.PhysicalCall, error) {
	return scanPhysicalCall(queryer.QueryRowContext(ctx, physicalCallSelect+` WHERE attempt_call_id = ?`, id))
}

func scanPhysicalCall(row scanner) (domain.PhysicalCall, error) {
	var result domain.PhysicalCall
	var kind, state, requestDigest, preparedAt string
	var outcome, failureJSON, providerID, responseDigest, dispatchAt, sentAt, completedAt sql.NullString
	err := row.Scan(&result.ID, &result.CallRecordID, &result.RunID, &result.StageName, &result.AttemptID,
		&result.Ordinal, &result.RetryGroup, &result.RetryOrdinal, &kind, &result.Provider, &requestDigest,
		&result.IdempotencyKey, &state, &outcome, &failureJSON, &providerID, &responseDigest,
		&preparedAt, &dispatchAt, &sentAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PhysicalCall{}, wrap(ErrNotFound, "physical call does not exist", err)
	}
	if err != nil {
		return domain.PhysicalCall{}, err
	}
	result.Kind, result.State, result.RequestDigest = domain.PhysicalCallKind(kind), domain.PhysicalCallState(state), domain.Digest(requestDigest)
	if outcome.Valid {
		value := domain.PhysicalOutcomeKind(outcome.String)
		result.Outcome = &value
	}
	if failureJSON.Valid {
		result.Failure = new(domain.PortFailure)
		if err := json.Unmarshal([]byte(failureJSON.String), result.Failure); err != nil {
			return domain.PhysicalCall{}, wrap(ErrConsistency, "stored physical failure is invalid", err)
		}
	}
	if providerID.Valid {
		result.ProviderRequestID = providerID.String
	}
	if responseDigest.Valid {
		value := domain.Digest(responseDigest.String)
		result.ResponseDigest = &value
	}
	if result.PreparedAt, err = parseTime(preparedAt); err != nil {
		return domain.PhysicalCall{}, err
	}
	if result.DispatchStartedAt, err = parseOptionalTime(dispatchAt); err != nil {
		return domain.PhysicalCall{}, err
	}
	if result.SentAt, err = parseOptionalTime(sentAt); err != nil {
		return domain.PhysicalCall{}, err
	}
	if result.CompletedAt, err = parseOptionalTime(completedAt); err != nil {
		return domain.PhysicalCall{}, err
	}
	if err := result.Validate(); err != nil {
		return domain.PhysicalCall{}, wrap(ErrConsistency, "stored physical call is invalid", err)
	}
	return result, nil
}

func readReservationsForPhysical(ctx context.Context, tx *immediateTx, id domain.AttemptCallID) ([]domain.BudgetReservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT reservation_id, run_id, stage_name, attempt_id, call_record_id,
		attempt_call_id, dimension, subkey, upper_bound, settled_value, state, created_at, settled_at
		FROM budget_reservations WHERE attempt_call_id = ? ORDER BY dimension, subkey`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.BudgetReservation
	for rows.Next() {
		reservation, err := scanBudgetReservation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, reservation)
	}
	return result, rows.Err()
}

func scanBudgetReservation(row scanner) (domain.BudgetReservation, error) {
	var result domain.BudgetReservation
	var dimension, state, createdAt string
	var settled sql.NullInt64
	var settledAt sql.NullString
	if err := row.Scan(&result.ID, &result.RunID, &result.StageName, &result.AttemptID, &result.CallRecordID,
		&result.AttemptCallID, &dimension, &result.Subkey, &result.UpperBound, &settled, &state, &createdAt, &settledAt); err != nil {
		return domain.BudgetReservation{}, err
	}
	result.Dimension, result.State = domain.BudgetDimension(dimension), domain.ReservationState(state)
	if settled.Valid {
		value := settled.Int64
		result.SettledValue = &value
	}
	var err error
	if result.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.BudgetReservation{}, err
	}
	if result.SettledAt, err = parseOptionalTime(settledAt); err != nil {
		return domain.BudgetReservation{}, err
	}
	if err := result.Validate(); err != nil {
		return domain.BudgetReservation{}, wrap(ErrConsistency, "stored reservation is invalid", err)
	}
	return result, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
