package sqlite

import (
	"bytes"
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
		legacyReplay := false
		err := tx.QueryRowContext(ctx,
			"SELECT create_command_digest, create_result_json FROM runs WHERE create_idempotency_key = ?",
			request.IdempotencyKey,
		).Scan(&storedDigest, &storedResult)
		if err == nil {
			if storedDigest != string(commandDigest) {
				legacyDigest, err := legacyCreateRunDigestV691b611(request)
				if err != nil {
					return err
				}
				if storedDigest != string(legacyDigest) {
					return wrap(ErrConsistency, "create idempotency key was reused with different content", nil)
				}
				if err := validateLegacyCreateRunReplay(ctx, tx, request, legacyDigest, storedResult); err != nil {
					return err
				}
				legacyReplay = true
			}
			if legacyReplay {
				result, err = unmarshalLegacyCreateRunResultV691b611(storedResult, request)
				return err
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
		result = domain.RunSnapshot{
			RunID: request.RunID, State: domain.RunCreated, Version: 1,
			WorkflowRevision: request.WorkflowRevision, SchemaVersion: request.SchemaVersion,
			RequestDigest:  request.SubmittedRequestDigest,
			ConfigDigest:   request.RedactedEffectiveConfigDigest,
			WorkflowDigest: request.WorkflowDigest,
			CurrentStage:   request.StageSequence[0], CurrentStageOrdinal: 1,
			CreatedAt: request.CreatedAt, UpdatedAt: request.CreatedAt,
		}
		if err := result.Validate(); err != nil {
			return wrap(ErrConsistency, "new run projection is invalid", err)
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
				max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
				max_mutations_per_stage, max_active_time_ns,
				state, current_stage, current_stage_ordinal, version,
				create_idempotency_key, create_command_digest, create_result_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
			string(request.RunID), request.SubmittedRequestJSON, string(request.SubmittedRequestDigest), request.EffectiveSeed,
			request.RedactedEffectiveConfigJSON, string(request.RedactedEffectiveConfigDigest), string(request.WorkflowDigest),
			request.WorkflowRevision, string(request.SchemaVersion),
			limits.MaxLLMCalls, limits.MaxSimilarityCalls, limits.MaxLLMInputTokens, limits.MaxLLMOutputTokens,
			limits.MaxLLMCostMicroUSD, limits.MaxSimilarityCostMicroUSD, limits.MaxSandboxCreates, limits.MaxArtifactBytes, limits.MaxPackageBytes,
			limits.MaxMutationsPerStage, limits.MaxActiveTimeMilliseconds*int64(time.Millisecond),
			string(domain.RunCreated), string(request.StageSequence[0]), 1,
			request.IdempotencyKey, string(commandDigest), resultJSON, formatTime(request.CreatedAt), formatTime(request.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		if err := insertInitialBudgetAccounts(ctx, tx, request.RunID, request.SubmittedRequestDigest, limits); err != nil {
			return err
		}
		for index, stage := range request.StageSequence {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO stage_records(
					run_id, stage_name, ordinal, workflow_revision, schema_version,
					state, version, input_digest, attempt_count,
					logical_idempotency_key, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, 'PENDING', 1, ?, 0, ?, ?, ?)`,
				string(request.RunID), string(stage), index+1, request.WorkflowRevision, string(request.SchemaVersion),
				string(request.SubmittedRequestDigest),
				request.IdempotencyKey+":"+string(stage), formatTime(request.CreatedAt), formatTime(request.CreatedAt),
			)
			if err != nil {
				return fmt.Errorf("insert stage %q: %w", stage, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_config_revisions(
				run_id, revision, redacted_effective_config_json, redacted_effective_config_digest, created_at
			) VALUES (?, 1, ?, ?, ?)`,
			string(request.RunID), request.RedactedEffectiveConfigJSON,
			string(request.RedactedEffectiveConfigDigest), formatTime(request.CreatedAt),
		); err != nil {
			return fmt.Errorf("insert initial config revision: %w", err)
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
		if err := result.Validate(); err != nil {
			return wrap(ErrConsistency, "new attempt violates persisted domain invariants", err)
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
		var stageUpdatedRaw string
		if err := tx.QueryRowContext(ctx, `SELECT state, current_attempt_id, ordinal, updated_at FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(command.RunID), string(command.StageName)).Scan(&stageState, &currentAttempt, &stageOrdinal, &stageUpdatedRaw); err != nil {
			return err
		}
		if stageState != string(domain.StageRunning) || !currentAttempt.Valid || currentAttempt.String != string(command.AttemptID) {
			return wrap(ErrConsistency, "finish attempt is not the active stage attempt", nil)
		}
		newStageState := stageStateForAttempt(command.AttemptState)
		if err := domain.ValidateStageTransition(domain.StageRunning, newStageState); err != nil {
			return err
		}
		var attemptState, attemptInput, attemptStartedRaw string
		if err := tx.QueryRowContext(ctx, `
			SELECT state, input_digest, started_at FROM stage_attempts
			WHERE attempt_id = ? AND run_id = ? AND stage_name = ?`,
			string(command.AttemptID), string(command.RunID), string(command.StageName),
		).Scan(&attemptState, &attemptInput, &attemptStartedRaw); err != nil {
			return err
		}
		if attemptState != string(domain.StageAttemptRunning) {
			return wrap(ErrInvalidTransition, "finish requires a RUNNING persisted attempt", nil)
		}
		attemptStarted, err := parseTime(attemptStartedRaw)
		if err != nil {
			return err
		}
		stageUpdated, err := parseTime(stageUpdatedRaw)
		if err != nil {
			return err
		}
		currentUpdated := run.UpdatedAt
		if stageUpdated.After(currentUpdated) {
			currentUpdated = stageUpdated
		}
		if err := command.ValidateAgainst(attemptStarted, currentUpdated); err != nil {
			return err
		}
		finishedAt := command.At
		persistedAttempt := domain.StageAttempt{
			AttemptID: command.AttemptID, RunID: command.RunID, StageName: command.StageName,
			Ordinal: stageOrdinal, State: command.AttemptState, InputDigest: domain.Digest(attemptInput),
			OutputDigest: command.OutputDigest, Cause: command.Cause, BlockedBinding: command.BlockedBinding,
			StartedAt: attemptStarted, FinishedAt: &finishedAt,
		}
		var attemptOrdinal int
		if err := tx.QueryRowContext(ctx, `SELECT ordinal FROM stage_attempts WHERE attempt_id = ?`, string(command.AttemptID)).Scan(&attemptOrdinal); err != nil {
			return err
		}
		persistedAttempt.Ordinal = attemptOrdinal
		if err := persistedAttempt.Validate(); err != nil {
			return wrap(ErrConsistency, "finished attempt would violate persisted domain invariants", err)
		}
		if command.AttemptState == domain.StageAttemptSucceeded {
			if err := attachPendingOccurrences(ctx, tx, command); err != nil {
				return err
			}
		} else if err := releasePendingArtifactTokens(ctx, tx, command.RunID, command.StageName, command.AttemptID, command.At); err != nil {
			return err
		}
		var output, cause, reviewEvidence, reviewPolicy, blockedBinding any
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
		if command.BlockedBinding != nil {
			encoded, marshalErr := json.Marshal(command.BlockedBinding)
			if marshalErr != nil {
				return fmt.Errorf("encode blocked checkpoint: %w", marshalErr)
			}
			blockedBinding = encoded
		}
		var reviewWaivable any
		if command.AttemptState == domain.StageAttemptNeedsReview {
			reviewWaivable = 0
			if command.ReviewGateWaivable {
				reviewWaivable = 1
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_attempts SET state = ?, output_digest = ?, cause = ?, blocked_binding_json = ?, finished_at = ?
			WHERE attempt_id = ? AND state = 'RUNNING'`,
			string(command.AttemptState), output, cause, blockedBinding, formatTime(command.At), string(command.AttemptID),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = ?, version = version + 1, output_digest = ?, current_attempt_id = NULL,
				review_evidence_digest = ?, review_policy_digest = ?, review_waivable = ?, updated_at = ?
			WHERE run_id = ? AND stage_name = ?`,
			string(newStageState), output, reviewEvidence, reviewPolicy, reviewWaivable, formatTime(command.At),
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
		if err := domain.ValidateRunTransition(run.State, domain.RunCreated); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		if err := domain.ValidateStageTransition(domain.StageRunning, domain.StagePending); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		var state, inputDigest, startedRaw string
		var ordinal int
		if err := tx.QueryRowContext(ctx, `
			SELECT state, input_digest, ordinal, started_at FROM stage_attempts
			WHERE attempt_id = ? AND run_id = ? AND stage_name = ?`,
			string(command.AttemptID), string(command.RunID), string(command.StageName),
		).Scan(&state, &inputDigest, &ordinal, &startedRaw); err != nil {
			return err
		}
		if state != string(domain.StageAttemptRunning) {
			return wrap(ErrInvalidTransition, "only a RUNNING attempt can be interrupted", nil)
		}
		startedAt, err := parseTime(startedRaw)
		if err != nil {
			return err
		}
		if command.At.Before(startedAt) || command.At.Before(run.UpdatedAt) {
			return wrap(ErrConsistency, "interrupt time precedes the persisted attempt or projection", nil)
		}
		finishedAt := command.At
		persistedAttempt := domain.StageAttempt{
			AttemptID: command.AttemptID, RunID: command.RunID, StageName: command.StageName,
			Ordinal: ordinal, State: domain.StageAttemptInterrupted, InputDigest: domain.Digest(inputDigest),
			Cause: &command.Cause, StartedAt: startedAt, FinishedAt: &finishedAt,
		}
		if err := persistedAttempt.Validate(); err != nil {
			return wrap(ErrConsistency, "interrupted attempt would violate persisted domain invariants", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stage_attempts SET state = 'INTERRUPTED', cause = ?, finished_at = ? WHERE attempt_id = ?`,
			string(command.Cause), formatTime(command.At), string(command.AttemptID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = 'PENDING', version = version + 1, current_attempt_id = NULL,
				output_digest = NULL, review_evidence_digest = NULL, review_policy_digest = NULL, review_waivable = NULL, updated_at = ?
			WHERE run_id = ? AND stage_name = ?`, formatTime(command.At), string(command.RunID), string(command.StageName)); err != nil {
			return err
		}
		// A force-killed owner may have durably sealed or finalized an artifact
		// before the stage interruption was recorded. Release that exact writer
		// token and pin in the same short transaction; otherwise Resume would
		// leave a writer/pin leak even though the interrupted attempt is gone.
		if err := releasePendingArtifactTokens(ctx, tx, command.RunID, command.StageName, command.AttemptID, command.At); err != nil {
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

// FinalizeCancel commits the terminal projection only after the caller has
// crossed its later exact-resource reconciliation boundary.
func (s *Store) FinalizeCancel(ctx context.Context, command domain.FinalizeCancelCommand) (domain.RunSnapshot, error) {
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
			return wrap(ErrVersionConflict, "finalize cancel expected version does not match", nil)
		}
		if run.ActiveStartedAt != nil {
			return wrap(ErrInvalidTransition, "finalize cancel requires closed active-time accounting", nil)
		}
		if run.State != domain.RunCreated && run.State != domain.RunBlocked && run.State != domain.RunNeedsReview {
			return wrap(ErrInvalidTransition, "finalize cancel requires a paused or interrupted run", nil)
		}
		if err := domain.ValidateRunTransition(run.State, domain.RunCancelled); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		pending, err := pendingCancelTx(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if pending == nil || pending.ID != command.ControlRequestID {
			return wrap(ErrConsistency, "finalize cancel does not match the active control request", nil)
		}
		var stageState string
		if err := tx.QueryRowContext(ctx, `
			SELECT state FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(command.RunID), string(run.CurrentStage),
		).Scan(&stageState); err != nil {
			return err
		}
		if err := domain.ValidateStageTransition(domain.StageState(stageState), domain.StageCancelled); err != nil {
			return wrap(ErrInvalidTransition, err.Error(), err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stage_records SET state = 'CANCELLED', version = version + 1,
				output_digest = NULL, current_attempt_id = NULL,
				review_evidence_digest = NULL, review_policy_digest = NULL, review_waivable = NULL,
				updated_at = ?
			WHERE run_id = ? AND stage_name = ?`,
			formatTime(command.At), string(command.RunID), string(run.CurrentStage),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE control_requests SET state = 'APPLIED', applied_at = ?
			WHERE control_id = ? AND run_id = ? AND state = 'PENDING'`,
			formatTime(command.At), string(command.ControlRequestID), string(command.RunID),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE review_decisions SET state = 'STALE', applied_at = ?
			WHERE run_id = ? AND state = 'PENDING'`,
			formatTime(command.At), string(command.RunID),
		); err != nil {
			return err
		}
		newVersion := run.Version + 1
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET state = 'CANCELLED', version = ?, cancel_summary = ?, updated_at = ?
			WHERE run_id = ?`,
			newVersion, pending.Reason, formatTime(command.At), string(command.RunID),
		); err != nil {
			return err
		}
		result, err = readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if err := result.Validate(); err != nil {
			return wrap(ErrConsistency, "finalized cancellation projection is invalid", err)
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventCancelFinalized, run.CurrentStage,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
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
		var maxNS, accountLimit, accountReserved, accountConsumed int64
		if err := tx.QueryRowContext(ctx, `
			SELECT run.max_active_time_ns, account.limit_value, account.reserved_value, account.consumed_value
			FROM runs run JOIN budget_accounts account ON account.run_id = run.run_id
			WHERE run.run_id = ? AND account.dimension = ?`,
			string(command.RunID), domain.BudgetActiveTimeNS,
		).Scan(&maxNS, &accountLimit, &accountReserved, &accountConsumed); err != nil {
			return err
		}
		if accountLimit != maxNS || accountReserved != 0 || int64(run.ActiveElapsed) != accountConsumed {
			return wrap(ErrConsistency, "active-time account and run projection disagree", nil)
		}
		elapsed := time.Duration(accountConsumed)
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
		if int64(elapsed) != accountConsumed {
			accountResult, err := tx.ExecContext(ctx, `
				UPDATE budget_accounts SET consumed_value = ?, account_version = account_version + 1
				WHERE run_id = ? AND dimension = ? AND consumed_value = ? AND reserved_value = 0
				  AND ? <= limit_value`,
				int64(elapsed), string(command.RunID), domain.BudgetActiveTimeNS, accountConsumed, int64(elapsed),
			)
			if err != nil {
				return err
			}
			updated, err := accountResult.RowsAffected()
			if err != nil {
				return err
			}
			if updated != 1 {
				return wrap(ErrConsistency, "active-time account debit lost its conditional update", nil)
			}
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
	if err := result.Validate(); err != nil {
		return domain.RunSnapshot{}, wrap(ErrConsistency, "stored run projection is invalid", err)
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

// legacyCreateRunDigestV691b611 reproduces the CreateRun command format used
// before workflow revision and schema version became explicit request fields.
// It exists only to recognize an exact, persisted 691b611 idempotent replay.
func legacyCreateRunDigestV691b611(request domain.CreateRunRequest) (domain.Digest, error) {
	type budgetLimitsV691b611 struct {
		MaxLLMCalls               int64 `json:"max_llm_calls"`
		MaxSimilarityCalls        int64 `json:"max_similarity_calls"`
		MaxLLMInputTokens         int64 `json:"max_llm_input_tokens"`
		MaxLLMOutputTokens        int64 `json:"max_llm_output_tokens"`
		MaxLLMCostMicroUSD        int64 `json:"max_llm_cost_micro_usd"`
		MaxSandboxCreates         int64 `json:"max_sandbox_creates"`
		MaxArtifactBytes          int64 `json:"max_artifact_bytes"`
		MaxPackageBytes           int64 `json:"max_package_bytes"`
		MaxMutationsPerStage      int64 `json:"max_mutations_per_stage"`
		MaxActiveTimeMilliseconds int64 `json:"max_active_time_milliseconds"`
	}
	type createRunRequestV691b611 struct {
		RunID                         domain.RunID         `json:"run_id"`
		SubmittedRequestJSON          []byte               `json:"submitted_request_json"`
		SubmittedRequestDigest        domain.Digest        `json:"submitted_request_digest"`
		EffectiveSeed                 int64                `json:"effective_seed"`
		RedactedEffectiveConfigJSON   []byte               `json:"redacted_effective_config_json"`
		RedactedEffectiveConfigDigest domain.Digest        `json:"redacted_effective_config_digest"`
		WorkflowDigest                domain.Digest        `json:"workflow_digest"`
		BudgetLimits                  budgetLimitsV691b611 `json:"budget_limits"`
		StageSequence                 []domain.StageName   `json:"stage_sequence"`
		CreatedAt                     time.Time            `json:"created_at"`
		IdempotencyKey                string               `json:"idempotency_key"`
	}
	encoded, err := json.Marshal(createRunRequestV691b611{
		RunID: request.RunID, SubmittedRequestJSON: request.SubmittedRequestJSON, SubmittedRequestDigest: request.SubmittedRequestDigest,
		EffectiveSeed: request.EffectiveSeed, RedactedEffectiveConfigJSON: request.RedactedEffectiveConfigJSON,
		RedactedEffectiveConfigDigest: request.RedactedEffectiveConfigDigest, WorkflowDigest: request.WorkflowDigest,
		BudgetLimits: budgetLimitsV691b611{
			MaxLLMCalls: request.BudgetLimits.MaxLLMCalls, MaxSimilarityCalls: request.BudgetLimits.MaxSimilarityCalls,
			MaxLLMInputTokens: request.BudgetLimits.MaxLLMInputTokens, MaxLLMOutputTokens: request.BudgetLimits.MaxLLMOutputTokens,
			MaxLLMCostMicroUSD: request.BudgetLimits.MaxLLMCostMicroUSD, MaxSandboxCreates: request.BudgetLimits.MaxSandboxCreates,
			MaxArtifactBytes: request.BudgetLimits.MaxArtifactBytes, MaxPackageBytes: request.BudgetLimits.MaxPackageBytes,
			MaxMutationsPerStage:      request.BudgetLimits.MaxMutationsPerStage,
			MaxActiveTimeMilliseconds: request.BudgetLimits.MaxActiveTimeMilliseconds,
		}, StageSequence: request.StageSequence, CreatedAt: request.CreatedAt,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return "", fmt.Errorf("encode 691b611 CreateRun command: %w", err)
	}
	return domain.SumBytes(encoded), nil
}

// validateLegacyCreateRunReplay makes the versioned digest fallback safe: a
// matching pre-versioned digest is insufficient unless every immutable run,
// stage, and creation-event binding still matches the submitted command.
func validateLegacyCreateRunReplay(ctx context.Context, tx *immediateTx, request domain.CreateRunRequest, legacyDigest domain.Digest, storedResult []byte) error {
	var (
		storedRunID, storedRequestDigest, storedConfigDigest, storedWorkflowDigest string
		storedWorkflowRevision, storedSchemaVersion, storedIdempotencyKey          string
		storedRequestJSON, storedConfigJSON                                        []byte
		storedSeed                                                                 int64
		storedLimits                                                               domain.BudgetLimits
		storedCreatedAt                                                            string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT run_id, submitted_request_json, submitted_request_digest, effective_seed,
			redacted_effective_config_json, redacted_effective_config_digest, workflow_digest,
			workflow_revision, schema_version,
			max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
			max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
			max_mutations_per_stage, max_active_time_ns, create_idempotency_key, created_at
		FROM runs WHERE create_idempotency_key = ?`, request.IdempotencyKey,
	).Scan(
		&storedRunID, &storedRequestJSON, &storedRequestDigest, &storedSeed,
		&storedConfigJSON, &storedConfigDigest, &storedWorkflowDigest,
		&storedWorkflowRevision, &storedSchemaVersion,
		&storedLimits.MaxLLMCalls, &storedLimits.MaxSimilarityCalls, &storedLimits.MaxLLMInputTokens, &storedLimits.MaxLLMOutputTokens,
		&storedLimits.MaxLLMCostMicroUSD, &storedLimits.MaxSimilarityCostMicroUSD, &storedLimits.MaxSandboxCreates, &storedLimits.MaxArtifactBytes, &storedLimits.MaxPackageBytes,
		&storedLimits.MaxMutationsPerStage, &storedLimits.MaxActiveTimeMilliseconds, &storedIdempotencyKey, &storedCreatedAt,
	)
	if err != nil {
		return fmt.Errorf("read legacy create replay bindings: %w", err)
	}
	if storedLimits.MaxActiveTimeMilliseconds%int64(time.Millisecond) != 0 {
		return wrap(ErrConsistency, "legacy create active-time binding is invalid", nil)
	}
	storedLimits.MaxActiveTimeMilliseconds /= int64(time.Millisecond)
	createdAt, err := parseTime(storedCreatedAt)
	if err != nil {
		return wrap(ErrConsistency, "legacy create timestamp is invalid", err)
	}
	if storedRunID != string(request.RunID) ||
		!bytes.Equal(storedRequestJSON, request.SubmittedRequestJSON) || storedRequestDigest != string(request.SubmittedRequestDigest) ||
		storedSeed != request.EffectiveSeed ||
		!bytes.Equal(storedConfigJSON, request.RedactedEffectiveConfigJSON) || storedConfigDigest != string(request.RedactedEffectiveConfigDigest) ||
		storedWorkflowDigest != string(request.WorkflowDigest) ||
		storedWorkflowRevision != string(request.WorkflowDigest) || storedWorkflowRevision != request.WorkflowRevision ||
		storedSchemaVersion != string(request.SchemaVersion) ||
		storedLimits != request.BudgetLimits || storedIdempotencyKey != request.IdempotencyKey ||
		!createdAt.Equal(request.CreatedAt) {
		return wrap(ErrConsistency, "legacy create idempotency content does not match persisted bindings", nil)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT stage_name, ordinal, workflow_revision, schema_version, logical_idempotency_key
		FROM stage_records WHERE run_id = ? ORDER BY ordinal`, string(request.RunID))
	if err != nil {
		return fmt.Errorf("read legacy create stage bindings: %w", err)
	}
	defer rows.Close()
	for index, stage := range request.StageSequence {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate legacy create stage bindings: %w", err)
			}
			return wrap(ErrConsistency, "legacy create stage sequence does not match persisted bindings", nil)
		}
		var storedStage, storedRevision, storedSchema, storedKey string
		var storedOrdinal int
		if err := rows.Scan(&storedStage, &storedOrdinal, &storedRevision, &storedSchema, &storedKey); err != nil {
			return fmt.Errorf("scan legacy create stage binding: %w", err)
		}
		if storedStage != string(stage) || storedOrdinal != index+1 ||
			storedRevision != request.WorkflowRevision || storedSchema != string(request.SchemaVersion) ||
			storedKey != request.IdempotencyKey+":"+string(stage) {
			return wrap(ErrConsistency, "legacy create stage sequence does not match persisted bindings", nil)
		}
	}
	if rows.Next() {
		return wrap(ErrConsistency, "legacy create stage sequence does not match persisted bindings", nil)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy create stage bindings: %w", err)
	}

	var eventType, eventKey, eventDigest, eventOccurred string
	var eventResult []byte
	err = tx.QueryRowContext(ctx, `
		SELECT event_type, idempotency_key, command_digest, result_json, occurred_at
		FROM run_events WHERE run_id = ? AND version = 1`, string(request.RunID),
	).Scan(&eventType, &eventKey, &eventDigest, &eventResult, &eventOccurred)
	if err != nil {
		return fmt.Errorf("read legacy create event binding: %w", err)
	}
	occurredAt, err := parseTime(eventOccurred)
	if err != nil {
		return wrap(ErrConsistency, "legacy create event timestamp is invalid", err)
	}
	if eventType != string(domain.EventRunCreated) || eventKey != request.IdempotencyKey || eventDigest != string(legacyDigest) ||
		!bytes.Equal(eventResult, storedResult) || !occurredAt.Equal(request.CreatedAt) {
		return wrap(ErrConsistency, "legacy create event does not match persisted bindings", nil)
	}
	return nil
}

// unmarshalLegacyCreateRunResultV691b611 preserves the historical idempotent
// result while adapting its formerly derived schema field to the current type.
func unmarshalLegacyCreateRunResultV691b611(storedResult []byte, request domain.CreateRunRequest) (domain.RunSnapshot, error) {
	type runSnapshotV691b611 struct {
		RunID                     domain.RunID     `json:"run_id"`
		State                     domain.RunState  `json:"state"`
		Version                   int64            `json:"version"`
		WorkflowRevision          string           `json:"workflow_revision"`
		SchemaVersion             string           `json:"schema_version"`
		RequestDigest             domain.Digest    `json:"request_digest"`
		ConfigDigest              domain.Digest    `json:"config_digest"`
		WorkflowDigest            domain.Digest    `json:"workflow_digest"`
		CurrentStage              domain.StageName `json:"current_stage"`
		CurrentStageOrdinal       int              `json:"current_stage_ordinal"`
		CreatedAt                 time.Time        `json:"created_at"`
		UpdatedAt                 time.Time        `json:"updated_at"`
		ActiveElapsed             time.Duration    `json:"active_elapsed"`
		ActiveStartedAt           *time.Time       `json:"active_started_at,omitempty"`
		LastAccountingHeartbeatAt *time.Time       `json:"last_accounting_heartbeat_at,omitempty"`
		CancelSummary             string           `json:"cancel_summary,omitempty"`
	}
	var legacy runSnapshotV691b611
	if err := json.Unmarshal(storedResult, &legacy); err != nil {
		return domain.RunSnapshot{}, wrap(ErrConsistency, "stored legacy create result is invalid", err)
	}
	legacySchemaVersion, err := legacyCreateRunSchemaVersionV691b611(request.SubmittedRequestJSON)
	if err != nil {
		return domain.RunSnapshot{}, wrap(ErrConsistency, "decode legacy create schema version", err)
	}
	if legacy.RunID != request.RunID || legacy.WorkflowRevision != string(request.WorkflowDigest) ||
		legacy.SchemaVersion != legacySchemaVersion || legacy.RequestDigest != request.SubmittedRequestDigest ||
		legacy.ConfigDigest != request.RedactedEffectiveConfigDigest || legacy.WorkflowDigest != request.WorkflowDigest {
		return domain.RunSnapshot{}, wrap(ErrConsistency, "stored legacy create result does not match persisted bindings", nil)
	}
	result := domain.RunSnapshot{
		RunID: legacy.RunID, State: legacy.State, Version: legacy.Version,
		WorkflowRevision: legacy.WorkflowRevision, SchemaVersion: request.SchemaVersion,
		RequestDigest: legacy.RequestDigest, ConfigDigest: legacy.ConfigDigest, WorkflowDigest: legacy.WorkflowDigest,
		CurrentStage: legacy.CurrentStage, CurrentStageOrdinal: legacy.CurrentStageOrdinal,
		CreatedAt: legacy.CreatedAt, UpdatedAt: legacy.UpdatedAt,
		ActiveElapsed: legacy.ActiveElapsed, ActiveStartedAt: legacy.ActiveStartedAt,
		LastAccountingHeartbeatAt: legacy.LastAccountingHeartbeatAt, CancelSummary: legacy.CancelSummary,
	}
	if err := result.Validate(); err != nil {
		return domain.RunSnapshot{}, wrap(ErrConsistency, "stored legacy create result is invalid", err)
	}
	return result, nil
}

// legacyCreateRunSchemaVersionV691b611 retains the schema lookup used by the
// old CreateRun result before Migration 2 supplied the explicit binding.
func legacyCreateRunSchemaVersionV691b611(submittedRequestJSON []byte) (string, error) {
	var value map[string]any
	if err := json.Unmarshal(submittedRequestJSON, &value); err != nil {
		return "", err
	}
	schema, _ := value["schema"].(string)
	return schema, nil
}

func marshalResult(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode idempotent result: %w", err)
	}
	return encoded, nil
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
