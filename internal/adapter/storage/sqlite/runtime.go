package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
)

func (s *Store) CreateRun(ctx context.Context, request domain.CreateRunRequest) (domain.RunSnapshot, error) {
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	var result domain.RunSnapshot
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var storedDigest string
		var storedResult []byte
		err := tx.QueryRowContext(ctx,
			"SELECT create_command_digest, create_result_json FROM runs WHERE create_idempotency_key = ?",
			request.IdempotencyKey,
		).Scan(&storedDigest, &storedResult)
		if err == nil {
			if storedDigest != string(commandDigest) {
				return wrap(ErrConsistency, "create idempotency key was reused with different content", nil)
			}
			return json.Unmarshal(storedResult, &result)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check create replay: %w", err)
		}
		var existing int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE run_id = ?", string(request.RunID)).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return wrap(ErrConsistency, "run id already exists under another create identity", nil)
		}
		workflowRevision := string(request.WorkflowDigest)
		schemaVersion := requestSchemaVersion(request.SubmittedRequestJSON)
		result = domain.RunSnapshot{
			RunID: request.RunID, State: domain.RunCreated, Version: 1,
			WorkflowRevision: workflowRevision, SchemaVersion: schemaVersion,
			RequestDigest:  request.SubmittedRequestDigest,
			ConfigDigest:   request.RedactedEffectiveConfigDigest,
			WorkflowDigest: request.WorkflowDigest,
			CurrentStage:   request.StageSequence[0], CurrentStageOrdinal: 1,
			CreatedAt: request.CreatedAt, UpdatedAt: request.CreatedAt,
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		limits := request.BudgetLimits
		_, err = tx.ExecContext(ctx, `
			INSERT INTO runs(
				run_id, submitted_request_json, submitted_request_digest, effective_seed,
				redacted_effective_config_json, redacted_effective_config_digest, workflow_digest,
				workflow_revision, schema_version,
				max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
				max_llm_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
				max_mutations_per_stage, max_active_time_ns,
				state, current_stage, current_stage_ordinal, version,
				create_idempotency_key, create_command_digest, create_result_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
			string(request.RunID), request.SubmittedRequestJSON, string(request.SubmittedRequestDigest), request.EffectiveSeed,
			request.RedactedEffectiveConfigJSON, string(request.RedactedEffectiveConfigDigest), string(request.WorkflowDigest),
			workflowRevision, schemaVersion,
			limits.MaxLLMCalls, limits.MaxSimilarityCalls, limits.MaxLLMInputTokens, limits.MaxLLMOutputTokens,
			limits.MaxLLMCostMicroUSD, limits.MaxSandboxCreates, limits.MaxArtifactBytes, limits.MaxPackageBytes,
			limits.MaxMutationsPerStage, limits.MaxActiveTimeMilliseconds*int64(time.Millisecond),
			string(domain.RunCreated), string(request.StageSequence[0]), 1,
			request.IdempotencyKey, string(commandDigest), resultJSON, formatTime(request.CreatedAt), formatTime(request.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		for index, stage := range request.StageSequence {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO stage_records(
					run_id, stage_name, ordinal, state, version, input_digest, attempt_count,
					logical_idempotency_key, created_at, updated_at
				) VALUES (?, ?, ?, 'PENDING', 1, ?, 0, ?, ?, ?)`,
				string(request.RunID), string(stage), index+1, string(request.SubmittedRequestDigest),
				request.IdempotencyKey+":"+string(stage), formatTime(request.CreatedAt), formatTime(request.CreatedAt),
			)
			if err != nil {
				return fmt.Errorf("insert stage %q: %w", stage, err)
			}
		}
		return insertEvent(ctx, tx, result.RunID, 1, domain.EventRunCreated, "", request.IdempotencyKey, commandDigest, resultJSON, request.CreatedAt)
	})
	return result, err
}

func (s *Store) GetRun(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	if err := runID.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer connection.Close()
	return readRun(ctx, connection, runID)
}

func (s *Store) ListRuns(ctx context.Context, filter domain.RunFilter) ([]domain.RunSummary, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	query := "SELECT run_id, state, version, current_stage, created_at, updated_at FROM runs"
	var args []any
	if filter.State != nil {
		query += " WHERE state = ?"
		args = append(args, string(*filter.State))
	}
	query += " ORDER BY created_at, run_id"
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	query += " LIMIT ?"
	args = append(args, limit)
	rows, err := connection.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []domain.RunSummary
	for rows.Next() {
		var item domain.RunSummary
		var runIDRaw, stateRaw, stageRaw, createdRaw, updatedRaw string
		if err := rows.Scan(&runIDRaw, &stateRaw, &item.Version, &stageRaw, &createdRaw, &updatedRaw); err != nil {
			return nil, err
		}
		item.RunID, item.State, item.CurrentStage = domain.RunID(runIDRaw), domain.RunState(stateRaw), domain.StageName(stageRaw)
		if item.CreatedAt, err = parseTime(createdRaw); err != nil {
			return nil, err
		}
		if item.UpdatedAt, err = parseTime(updatedRaw); err != nil {
			return nil, err
		}
		summaries = append(summaries, item)
	}
	return summaries, rows.Err()
}

func (s *Store) Events(ctx context.Context, runID domain.RunID, afterVersion int64) ([]domain.RunEvent, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	if afterVersion < 0 {
		return nil, errors.New("after version must not be negative")
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `
		SELECT version, event_type, COALESCE(stage_name, ''), idempotency_key, command_digest, occurred_at
		FROM run_events WHERE run_id = ? AND version > ? ORDER BY version`, string(runID), afterVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.RunEvent
	for rows.Next() {
		var item domain.RunEvent
		var eventType, stage, digest, occurred string
		item.RunID = runID
		if err := rows.Scan(&item.Version, &eventType, &stage, &item.IdempotencyKey, &digest, &occurred); err != nil {
			return nil, err
		}
		item.Type, item.StageName, item.CommandDigest = domain.RunEventType(eventType), domain.StageName(stage), domain.Digest(digest)
		if item.OccurredAt, err = parseTime(occurred); err != nil {
			return nil, err
		}
		events = append(events, item)
	}
	return events, rows.Err()
}

func (s *Store) BeginStage(ctx context.Context, command domain.BeginStageCommand) (domain.StageAttempt, error) {
	if err := command.Validate(); err != nil {
		return domain.StageAttempt{}, err
	}
	commandDigest, _, err := digestJSON(command)
	if err != nil {
		return domain.StageAttempt{}, err
	}
	var result domain.StageAttempt
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, command.RunID, command.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if run.Version != command.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "begin stage expected version does not match", nil)
		}
		if err := ValidateNoPendingCancel(ctx, tx, command.RunID); err != nil {
			return err
		}
		if err := domain.ValidateRunTransition(run.State, domain.RunRunning); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		if run.CurrentStage != command.StageName {
			return wrap(ErrConsistency, "begin stage is not the current stage", nil)
		}
		var stageState string
		var ordinal, attemptCount int
		var storedInput string
		if err := tx.QueryRowContext(ctx, `
			SELECT state, ordinal, attempt_count, input_digest FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(command.RunID), string(command.StageName),
		).Scan(&stageState, &ordinal, &attemptCount, &storedInput); err != nil {
			return err
		}
		if err := domain.ValidateStageTransition(domain.StageState(stageState), domain.StageRunning); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		if domain.StageState(stageState) == domain.StageBlocked && storedInput != string(command.InputDigest) {
			return wrap(ErrConsistency, "blocked-stage retry changed its input digest", nil)
		}
		result = domain.StageAttempt{
			AttemptID: command.AttemptID, RunID: command.RunID, StageName: command.StageName,
			Ordinal: attemptCount + 1, State: domain.StageAttemptRunning,
			InputDigest: command.InputDigest, StartedAt: command.At,
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stage_attempts(attempt_id, run_id, stage_name, ordinal, state, input_digest, started_at)
			VALUES (?, ?, ?, ?, 'RUNNING', ?, ?)`,
			string(command.AttemptID), string(command.RunID), string(command.StageName), result.Ordinal,
			string(command.InputDigest), formatTime(command.At),
		); err != nil {
			return fmt.Errorf("insert stage attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = 'RUNNING', version = version + 1, input_digest = ?,
				attempt_count = ?, current_attempt_id = ?, updated_at = ?
			WHERE run_id = ? AND stage_name = ?`,
			string(command.InputDigest), result.Ordinal, string(command.AttemptID), formatTime(command.At),
			string(command.RunID), string(command.StageName),
		); err != nil {
			return err
		}
		newVersion := run.Version + 1
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state = 'RUNNING', version = ?, updated_at = ? WHERE run_id = ?`,
			newVersion, formatTime(command.At), string(command.RunID)); err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventStageBegan, command.StageName,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
}

func (s *Store) FinishStage(ctx context.Context, command domain.FinishStageCommand) (domain.RunSnapshot, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	commandDigest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	var result domain.RunSnapshot
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, command.RunID, command.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if run.Version != command.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "finish stage expected version does not match", nil)
		}
		if run.State != domain.RunRunning || run.CurrentStage != command.StageName {
			return wrap(ErrInvalidTransition, "finish requires the current RUNNING stage", nil)
		}
		if run.ActiveStartedAt != nil {
			return wrap(ErrConsistency, "active-time interval must be closed before finishing a stage", nil)
		}
		pending, err := pendingCancelTx(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if pending != nil && command.AttemptState != domain.StageAttemptCancelled {
			return wrap(ErrCancelPending, "a pending cancel prevents non-cancel completion", nil)
		}
		if pending == nil && command.AttemptState == domain.StageAttemptCancelled {
			return wrap(ErrInvalidTransition, "cancelled finish requires an active cancel request", nil)
		}
		if err := domain.ValidateRunTransition(run.State, command.RunState); err != nil {
			return err
		}
		var stageState string
		var currentAttempt sql.NullString
		var stageOrdinal int
		if err := tx.QueryRowContext(ctx, `SELECT state, current_attempt_id, ordinal FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(command.RunID), string(command.StageName)).Scan(&stageState, &currentAttempt, &stageOrdinal); err != nil {
			return err
		}
		if stageState != string(domain.StageRunning) || !currentAttempt.Valid || currentAttempt.String != string(command.AttemptID) {
			return wrap(ErrConsistency, "finish attempt is not the active stage attempt", nil)
		}
		newStageState := stageStateForAttempt(command.AttemptState)
		if err := domain.ValidateStageTransition(domain.StageRunning, newStageState); err != nil {
			return err
		}
		var output, cause, reviewEvidence, reviewPolicy any
		if command.OutputDigest != nil {
			output = string(*command.OutputDigest)
		}
		if command.Cause != nil {
			cause = string(*command.Cause)
		}
		if command.ReviewEvidenceDigest != nil {
			reviewEvidence = string(*command.ReviewEvidenceDigest)
		}
		if command.ReviewPolicyDigest != nil {
			reviewPolicy = string(*command.ReviewPolicyDigest)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_attempts SET state = ?, output_digest = ?, cause = ?, finished_at = ?
			WHERE attempt_id = ? AND state = 'RUNNING'`,
			string(command.AttemptState), output, cause, formatTime(command.At), string(command.AttemptID),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = ?, version = version + 1, output_digest = ?, current_attempt_id = NULL,
				review_evidence_digest = ?, review_policy_digest = ?, updated_at = ?
			WHERE run_id = ? AND stage_name = ?`,
			string(newStageState), output, reviewEvidence, reviewPolicy, formatTime(command.At),
			string(command.RunID), string(command.StageName),
		); err != nil {
			return err
		}
		currentStage, currentOrdinal := command.StageName, stageOrdinal
		configDigest := run.ConfigDigest
		cancelSummary := any(nil)
		if command.AttemptState == domain.StageAttemptSucceeded {
			var nextOrdinal int
			var nextState string
			if err := tx.QueryRowContext(ctx, `SELECT ordinal, state FROM stage_records WHERE run_id = ? AND stage_name = ?`,
				string(command.RunID), string(command.NextStage)).Scan(&nextOrdinal, &nextState); err != nil {
				return wrap(ErrConsistency, "next stage does not exist", err)
			}
			if nextOrdinal != stageOrdinal+1 || nextState != string(domain.StagePending) {
				return wrap(ErrConsistency, "next stage is not the fixed pending successor", nil)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE stage_records SET input_digest = ?, version = version + 1, updated_at = ? WHERE run_id = ? AND stage_name = ?`,
				string(*command.NextInputDigest), formatTime(command.At), string(command.RunID), string(command.NextStage)); err != nil {
				return err
			}
			currentStage, currentOrdinal = command.NextStage, nextOrdinal
		}
		if command.AttemptState == domain.StageAttemptCancelled {
			cancelSummary = pending.Reason
			if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = 'APPLIED', applied_at = ? WHERE control_id = ? AND state = 'PENDING'`,
				formatTime(command.At), string(pending.ID)); err != nil {
				return err
			}
		}
		newVersion := run.Version + 1
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET state = ?, current_stage = ?, current_stage_ordinal = ?, version = ?,
				redacted_effective_config_digest = ?, cancel_summary = ?, updated_at = ? WHERE run_id = ?`,
			string(command.RunState), string(currentStage), currentOrdinal, newVersion,
			string(configDigest), cancelSummary, formatTime(command.At), string(command.RunID),
		); err != nil {
			return err
		}
		result, err = readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventStageFinished, command.StageName,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
}

func (s *Store) InterruptStage(ctx context.Context, command domain.InterruptStageCommand) (domain.RunSnapshot, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	commandDigest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	var result domain.RunSnapshot
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, command.RunID, command.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if run.Version != command.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "interrupt expected version does not match", nil)
		}
		if run.State != domain.RunRunning || run.CurrentStage != command.StageName || run.ActiveStartedAt != nil {
			return wrap(ErrInvalidTransition, "interrupt requires a RUNNING stage with closed accounting", nil)
		}
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM stage_attempts WHERE attempt_id = ? AND run_id = ? AND stage_name = ?",
			string(command.AttemptID), string(command.RunID), string(command.StageName)).Scan(&state); err != nil {
			return err
		}
		if state != string(domain.StageAttemptRunning) {
			return wrap(ErrInvalidTransition, "only a RUNNING attempt can be interrupted", nil)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stage_attempts SET state = 'INTERRUPTED', cause = ?, finished_at = ? WHERE attempt_id = ?`,
			string(command.Cause), formatTime(command.At), string(command.AttemptID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = 'PENDING', version = version + 1, current_attempt_id = NULL,
				output_digest = NULL, review_evidence_digest = NULL, review_policy_digest = NULL, updated_at = ?
			WHERE run_id = ? AND stage_name = ?`, formatTime(command.At), string(command.RunID), string(command.StageName)); err != nil {
			return err
		}
		newVersion := run.Version + 1
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state = 'CREATED', version = ?, updated_at = ? WHERE run_id = ?`,
			newVersion, formatTime(command.At), string(command.RunID)); err != nil {
			return err
		}
		result, err = readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventStageInterrupted, command.StageName,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
}

func (s *Store) RequestCancel(ctx context.Context, request domain.CancelRequest) (domain.ControlRequest, error) {
	if err := request.Validate(); err != nil {
		return domain.ControlRequest{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.ControlRequest{}, err
	}
	var result domain.ControlRequest
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, request.RunID, request.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, request.RunID)
		if err != nil {
			return err
		}
		if run.Version != request.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "cancel expected version does not match", nil)
		}
		if run.State == domain.RunReady || run.State == domain.RunFailed || run.State == domain.RunCancelled {
			return wrap(ErrInvalidTransition, "terminal run cannot be cancelled", nil)
		}
		if pending, err := pendingCancelTx(ctx, tx, request.RunID); err != nil {
			return err
		} else if pending != nil {
			return wrap(ErrCancelPending, "run already has an active cancel request", nil)
		}
		newVersion := run.Version + 1
		result = domain.ControlRequest{
			ID: request.ID, RunID: request.RunID, Reason: request.Reason,
			Active: true, RunVersion: newVersion, CreatedAt: request.At,
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO control_requests(control_id, run_id, kind, state, reason, expected_run_version,
				idempotency_key, command_digest, created_at)
			VALUES (?, ?, 'CANCEL', 'PENDING', ?, ?, ?, ?, ?)`,
			string(request.ID), string(request.RunID), request.Reason, request.ExpectedRunVersion,
			request.IdempotencyKey, string(commandDigest), formatTime(request.At),
		); err != nil {
			return fmt.Errorf("insert cancel request: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET version = ?, updated_at = ? WHERE run_id = ?`,
			newVersion, formatTime(request.At), string(request.RunID)); err != nil {
			return err
		}
		return insertEvent(ctx, tx, request.RunID, newVersion, domain.EventCancelRequested, run.CurrentStage,
			request.IdempotencyKey, commandDigest, resultJSON, request.At)
	})
	return result, err
}

func (s *Store) PendingCancel(ctx context.Context, runID domain.RunID) (*domain.ControlRequest, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	return pendingCancelTx(ctx, connection, runID)
}

func (s *Store) AccountActiveTime(ctx context.Context, command domain.ActiveTimeCommand) (domain.ActiveTimeResult, error) {
	if err := command.Validate(); err != nil {
		return domain.ActiveTimeResult{}, err
	}
	commandDigest, _, err := digestJSON(command)
	if err != nil {
		return domain.ActiveTimeResult{}, err
	}
	var result domain.ActiveTimeResult
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, command.RunID, command.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if run.Version != command.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "active-time expected version does not match", nil)
		}
		if run.State != domain.RunRunning {
			return wrap(ErrInvalidTransition, "active time can advance only while RUNNING", nil)
		}
		var maxNS int64
		if err := tx.QueryRowContext(ctx, "SELECT max_active_time_ns FROM runs WHERE run_id = ?", string(command.RunID)).Scan(&maxNS); err != nil {
			return err
		}
		elapsed := run.ActiveElapsed
		active := run.ActiveStartedAt != nil
		started, heartbeat := run.ActiveStartedAt, run.LastAccountingHeartbeatAt
		switch command.Action {
		case domain.ActiveTimeStart:
			if active {
				return wrap(ErrInvalidTransition, "active-time interval is already open", nil)
			}
			startedAt := command.At
			started, heartbeat, active = &startedAt, &startedAt, true
		case domain.ActiveTimeHeartbeat, domain.ActiveTimeStop, domain.ActiveTimeRecover:
			if !active {
				return wrap(ErrInvalidTransition, "active-time interval is not open", nil)
			}
			end := command.At
			if command.Action == domain.ActiveTimeRecover {
				bound := heartbeat.Add(command.HeartbeatInterval)
				if end.After(bound) {
					end = bound
				}
			}
			if end.Before(*heartbeat) {
				return wrap(ErrConsistency, "active-time command precedes last heartbeat", nil)
			}
			delta := end.Sub(*heartbeat)
			remainingBefore := time.Duration(maxNS) - elapsed
			if remainingBefore < 0 {
				return wrap(ErrConsistency, "stored active time exceeds immutable limit", nil)
			}
			if delta > remainingBefore {
				delta = remainingBefore
			}
			elapsed += delta
			if command.Action == domain.ActiveTimeHeartbeat && elapsed < time.Duration(maxNS) {
				heartbeatAt := command.At
				heartbeat = &heartbeatAt
			} else {
				started, heartbeat, active = nil, nil, false
			}
		}
		remaining := time.Duration(maxNS) - elapsed
		if remaining < 0 {
			remaining = 0
		}
		newVersion := run.Version + 1
		deadline := command.At.Add(remaining)
		result = domain.ActiveTimeResult{
			RunID: command.RunID, RunVersion: newVersion, ActiveElapsed: elapsed,
			Remaining: remaining, Deadline: deadline, Active: active, Exhausted: remaining == 0,
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		var startedValue, heartbeatValue any
		if started != nil {
			startedValue = formatTime(*started)
			heartbeatValue = formatTime(*heartbeat)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET active_elapsed_ns = ?, active_started_at = ?, last_accounting_heartbeat_at = ?,
				version = ?, updated_at = ? WHERE run_id = ?`,
			int64(elapsed), startedValue, heartbeatValue, newVersion, formatTime(command.At), string(command.RunID),
		); err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventActiveTimeAccounted, run.CurrentStage,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRun(ctx context.Context, queryer rowQuerier, runID domain.RunID) (domain.RunSnapshot, error) {
	var result domain.RunSnapshot
	var runIDRaw, state, requestDigest, configDigest, workflowDigest, currentStage string
	var activeStarted, heartbeat, cancel sql.NullString
	var createdAt, updatedAt string
	var activeElapsed int64
	err := queryer.QueryRowContext(ctx, `
		SELECT run_id, state, version, workflow_revision, schema_version,
			submitted_request_digest, redacted_effective_config_digest, workflow_digest,
			current_stage, current_stage_ordinal, created_at, updated_at,
			active_elapsed_ns, active_started_at, last_accounting_heartbeat_at, cancel_summary
		FROM runs WHERE run_id = ?`, string(runID)).Scan(
		&runIDRaw, &state, &result.Version, &result.WorkflowRevision, &result.SchemaVersion,
		&requestDigest, &configDigest, &workflowDigest,
		&currentStage, &result.CurrentStageOrdinal, &createdAt, &updatedAt,
		&activeElapsed, &activeStarted, &heartbeat, &cancel,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RunSnapshot{}, wrap(ErrNotFound, "run does not exist", err)
	}
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	result.RunID, result.State = domain.RunID(runIDRaw), domain.RunState(state)
	result.RequestDigest, result.ConfigDigest, result.WorkflowDigest = domain.Digest(requestDigest), domain.Digest(configDigest), domain.Digest(workflowDigest)
	result.CurrentStage, result.ActiveElapsed = domain.StageName(currentStage), time.Duration(activeElapsed)
	if result.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.RunSnapshot{}, err
	}
	if result.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.RunSnapshot{}, err
	}
	if activeStarted.Valid {
		value, err := parseTime(activeStarted.String)
		if err != nil {
			return domain.RunSnapshot{}, err
		}
		result.ActiveStartedAt = &value
	}
	if heartbeat.Valid {
		value, err := parseTime(heartbeat.String)
		if err != nil {
			return domain.RunSnapshot{}, err
		}
		result.LastAccountingHeartbeatAt = &value
	}
	if cancel.Valid {
		result.CancelSummary = cancel.String
	}
	return result, nil
}

func insertEvent(ctx context.Context, tx *immediateTx, runID domain.RunID, version int64, eventType domain.RunEventType,
	stage domain.StageName, idempotencyKey string, commandDigest domain.Digest, resultJSON []byte, at time.Time,
) error {
	var stageValue any
	if stage != "" {
		stageValue = string(stage)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_events(run_id, version, event_type, stage_name, idempotency_key, command_digest, result_json, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(runID), version, string(eventType), stageValue, idempotencyKey, string(commandDigest), resultJSON, formatTime(at),
	)
	return err
}

func replayEvent(ctx context.Context, tx *immediateTx, runID domain.RunID, key string, digest domain.Digest, destination any) (bool, error) {
	var storedDigest string
	var resultJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT command_digest, result_json FROM run_events WHERE run_id = ? AND idempotency_key = ?`,
		string(runID), key).Scan(&storedDigest, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedDigest != string(digest) {
		return true, wrap(ErrConsistency, "idempotency key was reused with different content", nil)
	}
	if err := json.Unmarshal(resultJSON, destination); err != nil {
		return true, wrap(ErrConsistency, "stored idempotent result is invalid", err)
	}
	return true, nil
}

func digestJSON(value any) (domain.Digest, []byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", nil, fmt.Errorf("encode command: %w", err)
	}
	return domain.SumBytes(encoded), encoded, nil
}

func marshalResult(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode idempotent result: %w", err)
	}
	return encoded, nil
}

func requestSchemaVersion(encoded []byte) string {
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return ""
	}
	if schema, ok := value["schema"].(string); ok {
		return schema
	}
	return ""
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted UTC time: %w", err)
	}
	if parsed.Location() != time.UTC {
		return time.Time{}, errors.New("persisted timestamp is not UTC")
	}
	return parsed, nil
}

func stageStateForAttempt(state domain.StageAttemptState) domain.StageState {
	switch state {
	case domain.StageAttemptSucceeded:
		return domain.StageSucceeded
	case domain.StageAttemptBlocked:
		return domain.StageBlocked
	case domain.StageAttemptNeedsReview:
		return domain.StageNeedsReview
	case domain.StageAttemptFailed:
		return domain.StageFailed
	case domain.StageAttemptCancelled:
		return domain.StageCancelled
	default:
		return ""
	}
}

func ValidateNoPendingCancel(ctx context.Context, queryer rowQuerier, runID domain.RunID) error {
	var count int
	if err := queryer.QueryRowContext(ctx, `SELECT count(*) FROM control_requests WHERE run_id = ? AND state = 'PENDING'`, string(runID)).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return wrap(ErrCancelPending, "run has an active cancel request", nil)
	}
	return nil
}

func pendingCancelTx(ctx context.Context, queryer rowQuerier, runID domain.RunID) (*domain.ControlRequest, error) {
	var result domain.ControlRequest
	var id, runIDRaw, reason, created string
	var expectedVersion int64
	err := queryer.QueryRowContext(ctx, `
		SELECT control_id, run_id, reason, expected_run_version, created_at
		FROM control_requests WHERE run_id = ? AND state = 'PENDING'`, string(runID)).Scan(
		&id, &runIDRaw, &reason, &expectedVersion, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.ID, result.RunID, result.Reason = domain.ControlRequestID(id), domain.RunID(runIDRaw), reason
	result.Active, result.RunVersion = true, expectedVersion+1
	if result.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &result, nil
}

func trimReason(value string) string { return strings.TrimSpace(value) }
