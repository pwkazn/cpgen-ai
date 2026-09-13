package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/domain"
)

func (s *Store) CreateReview(ctx context.Context, request domain.CreateReviewRequest) (domain.ReviewDecision, error) {
	if err := request.Validate(); err != nil {
		return domain.ReviewDecision{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	var result domain.ReviewDecision
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if replayed, err := replayEvent(ctx, tx, request.RunID, request.IdempotencyKey, commandDigest, &result); err != nil || replayed {
			return err
		}
		run, err := readRun(ctx, tx, request.RunID)
		if err != nil {
			return err
		}
		if run.Version != request.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "review creation expected version does not match", nil)
		}
		if run.State != domain.RunNeedsReview || run.CurrentStage != request.StageName {
			return wrap(ErrInvalidTransition, "review requires the current NEEDS_REVIEW stage", nil)
		}
		if run.WorkflowRevision != request.WorkflowRevision {
			return wrap(ErrConsistency, "review workflow revision does not match the run snapshot", nil)
		}
		if err := ValidateNoPendingCancel(ctx, tx, request.RunID); err != nil {
			return err
		}
		if pending, err := pendingReviewTx(ctx, tx, request.RunID); err != nil {
			return err
		} else if pending != nil {
			return wrap(ErrReviewPending, "run already has a pending review", nil)
		}
		var stageState, inputDigest, evidenceDigest, policyDigest string
		var stageWaivable int
		if err := tx.QueryRowContext(ctx, `
			SELECT state, input_digest, review_evidence_digest, review_policy_digest, review_waivable
			FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(request.RunID), string(request.StageName),
		).Scan(&stageState, &inputDigest, &evidenceDigest, &policyDigest, &stageWaivable); err != nil {
			return err
		}
		if stageState != string(domain.StageNeedsReview) || inputDigest != string(request.StageInputDigest) ||
			evidenceDigest != string(request.EvidenceDigest) || policyDigest != string(request.PolicyDigest) {
			return wrap(ErrConsistency, "review binding does not match the current stage snapshot", nil)
		}
		waivable := request.Kind == domain.ReviewWaive
		if waivable && stageWaivable != 1 {
			return wrap(ErrInvalidTransition, "persisted review gate is not waivable", nil)
		}
		newVersion := run.Version + 1
		result = domain.ReviewDecision{
			ID: request.ID, RunID: request.RunID, Kind: request.Kind, State: domain.ReviewPending,
			ExpectedRunVersion: request.ExpectedRunVersion, RunVersion: newVersion,
			WorkflowRevision: request.WorkflowRevision, StageName: request.StageName,
			StageInputDigest: request.StageInputDigest, EvidenceDigest: request.EvidenceDigest, PolicyDigest: request.PolicyDigest,
			RequestedEditsDigest: request.RequestedEditsDigest, WaiverScopeDigest: request.WaiverScopeDigest,
			ExternalConditionDigest: request.ExternalConditionDigest, BudgetIncrease: request.BudgetIncrease,
			WaivableGate: waivable, Reviewer: request.Reviewer, Reason: request.Reason,
			CreatedAt: request.At,
		}
		if err := result.Validate(); err != nil {
			return wrap(ErrConsistency, "derived review decision is invalid", err)
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		var edits, waiver, condition, budgetJSON any
		if request.RequestedEditsDigest != nil {
			edits = string(*request.RequestedEditsDigest)
		}
		if request.WaiverScopeDigest != nil {
			waiver = string(*request.WaiverScopeDigest)
		}
		if request.ExternalConditionDigest != nil {
			condition = string(*request.ExternalConditionDigest)
		}
		if request.BudgetIncrease != (domain.BudgetLimits{}) {
			encoded, err := json.Marshal(request.BudgetIncrease)
			if err != nil {
				return err
			}
			budgetJSON = encoded
		}
		waivableValue := 0
		if waivable {
			waivableValue = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO review_decisions(
				review_id, run_id, kind, state, expected_run_version, run_version, workflow_revision,
				stage_name, stage_input_digest, evidence_digest, policy_digest,
				requested_edits_digest, waiver_scope_digest, external_condition_digest,
				budget_increase_json, waivable_gate, reviewer, reason,
				idempotency_key, command_digest, created_at
			) VALUES (?, ?, ?, 'PENDING', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(request.ID), string(request.RunID), string(request.Kind), request.ExpectedRunVersion, newVersion,
			request.WorkflowRevision, string(request.StageName), string(request.StageInputDigest),
			string(request.EvidenceDigest), string(request.PolicyDigest), edits, waiver, condition,
			budgetJSON, waivableValue, request.Reviewer, request.Reason,
			request.IdempotencyKey, string(commandDigest), formatTime(request.At),
		); err != nil {
			return fmt.Errorf("insert review decision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET version = ?, updated_at = ? WHERE run_id = ?`,
			newVersion, formatTime(request.At), string(request.RunID)); err != nil {
			return err
		}
		return insertEvent(ctx, tx, request.RunID, newVersion, domain.EventReviewCreated, request.StageName,
			request.IdempotencyKey, commandDigest, resultJSON, request.At)
	})
	return result, err
}

func (s *Store) PendingReview(ctx context.Context, runID domain.RunID) (*domain.ReviewDecision, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	return pendingReviewTx(ctx, connection, runID)
}

func (s *Store) ApplyReview(ctx context.Context, command domain.ApplyReviewCommand) (domain.RunSnapshot, error) {
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
			return wrap(ErrVersionConflict, "review application expected version does not match", nil)
		}
		if run.State != domain.RunNeedsReview || run.CurrentStage != command.StageName {
			return wrap(ErrInvalidTransition, "review application requires the current NEEDS_REVIEW stage", nil)
		}
		if err := ValidateNoPendingCancel(ctx, tx, command.RunID); err != nil {
			return err
		}
		decision, err := reviewByIDTx(ctx, tx, command.RunID, command.ReviewDecisionID)
		if err != nil {
			return err
		}
		if decision.State != domain.ReviewPending {
			return wrap(ErrInvalidTransition, "review decision is not pending", nil)
		}
		if decision.StageName != command.StageName || decision.StageInputDigest != command.StageInputDigest ||
			decision.EvidenceDigest != command.EvidenceDigest || decision.PolicyDigest != command.PolicyDigest {
			return wrap(ErrConsistency, "review application binding changed", nil)
		}
		var stageState, stageInput, stageEvidence, stagePolicy string
		var stageWaivable int
		if err := tx.QueryRowContext(ctx, `
			SELECT state, input_digest, review_evidence_digest, review_policy_digest, review_waivable
			FROM stage_records WHERE run_id = ? AND stage_name = ?`,
			string(command.RunID), string(command.StageName),
		).Scan(&stageState, &stageInput, &stageEvidence, &stagePolicy, &stageWaivable); err != nil {
			return err
		}
		if stageState != string(domain.StageNeedsReview) || stageInput != string(decision.StageInputDigest) ||
			stageEvidence != string(decision.EvidenceDigest) || stagePolicy != string(decision.PolicyDigest) ||
			(decision.Kind == domain.ReviewWaive && stageWaivable != 1) {
			return wrap(ErrConsistency, "persisted stage snapshot no longer matches the review decision", nil)
		}
		if decision.Kind == domain.ReviewRevise {
			if command.NewInputDigest == nil || command.NewConfigDigest == nil || len(command.NewConfigJSON) == 0 || len(command.InvalidatedStages) == 0 {
				return wrap(ErrConsistency, "REVISE application lacks revised digests or invalidation", nil)
			}
		} else if command.NewInputDigest != nil || command.NewConfigDigest != nil || len(command.NewConfigJSON) != 0 || len(command.InvalidatedStages) != 0 {
			return wrap(ErrConsistency, "non-REVISE application carries revision payload", nil)
		}
		newRunState := domain.RunCreated
		newReviewState := domain.ReviewApplied
		newStageState := domain.StagePending
		if decision.Kind == domain.ReviewReject {
			newRunState, newReviewState, newStageState = domain.RunFailed, domain.ReviewRejected, domain.StageFailed
		}
		if err := domain.ValidateRunTransition(domain.RunNeedsReview, newRunState); err != nil {
			return err
		}
		if err := domain.ValidateStageTransition(domain.StageNeedsReview, newStageState); err != nil {
			return err
		}
		restartStage, restartOrdinal := run.CurrentStage, run.CurrentStageOrdinal
		if decision.Kind == domain.ReviewRevise {
			rows, err := tx.QueryContext(ctx, `
				SELECT stage_name, ordinal FROM stage_records WHERE run_id = ? ORDER BY ordinal`, string(command.RunID))
			if err != nil {
				return err
			}
			type stagePosition struct {
				name    domain.StageName
				ordinal int
			}
			var pipeline []stagePosition
			for rows.Next() {
				var raw string
				var ordinal int
				if err := rows.Scan(&raw, &ordinal); err != nil {
					_ = rows.Close()
					return err
				}
				pipeline = append(pipeline, stagePosition{name: domain.StageName(raw), ordinal: ordinal})
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			start := -1
			for index := range pipeline {
				if pipeline[index].name == command.InvalidatedStages[0] {
					start = index
					break
				}
			}
			if start < 0 || len(command.InvalidatedStages) != len(pipeline)-start {
				return wrap(ErrConsistency, "REVISE invalidation is not the exact downstream suffix", nil)
			}
			for offset, stage := range command.InvalidatedStages {
				if pipeline[start+offset].name != stage {
					return wrap(ErrConsistency, "REVISE invalidation has a gap or is out of order", nil)
				}
			}
			restartStage, restartOrdinal = pipeline[start].name, pipeline[start].ordinal
			for index, stage := range command.InvalidatedStages {
				input := any(nil)
				if index == 0 {
					input = string(*command.NewInputDigest)
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE stage_records SET state = 'PENDING', version = version + 1,
						input_digest = COALESCE(?, input_digest), output_digest = NULL, current_attempt_id = NULL,
						review_evidence_digest = NULL, review_policy_digest = NULL, review_waivable = NULL, updated_at = ?
					WHERE run_id = ? AND stage_name = ?`, input, formatTime(command.At), string(command.RunID), string(stage)); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE stage_records SET state = ?, version = version + 1, output_digest = NULL,
					current_attempt_id = NULL, review_evidence_digest = NULL, review_policy_digest = NULL, review_waivable = NULL, updated_at = ?
				WHERE run_id = ? AND stage_name = ?`,
				string(newStageState), formatTime(command.At), string(command.RunID), string(command.StageName)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_decisions SET state = ?, applied_at = ? WHERE review_id = ? AND state = 'PENDING'`,
			string(newReviewState), formatTime(command.At), string(command.ReviewDecisionID)); err != nil {
			return err
		}
		newVersion := run.Version + 1
		if decision.Kind == domain.ReviewRevise {
			var nextConfigRevision int
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(MAX(revision), 0) + 1 FROM run_config_revisions WHERE run_id = ?`,
				string(command.RunID),
			).Scan(&nextConfigRevision); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_config_revisions(
					run_id, revision, redacted_effective_config_json, redacted_effective_config_digest,
					source_review_id, created_at
				) VALUES (?, ?, ?, ?, ?, ?)`,
				string(command.RunID), nextConfigRevision, command.NewConfigJSON, string(*command.NewConfigDigest),
				string(command.ReviewDecisionID), formatTime(command.At),
			); err != nil {
				return fmt.Errorf("insert revised config binding: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE runs SET state = ?, current_stage = ?, current_stage_ordinal = ?,
					redacted_effective_config_json = ?, redacted_effective_config_digest = ?,
					version = ?, updated_at = ? WHERE run_id = ?`,
				string(newRunState), string(restartStage), restartOrdinal,
				command.NewConfigJSON, string(*command.NewConfigDigest), newVersion, formatTime(command.At), string(command.RunID),
			); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET state = ?, version = ?, updated_at = ? WHERE run_id = ?`,
			string(newRunState), newVersion, formatTime(command.At), string(command.RunID)); err != nil {
			return err
		}
		result, err = readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if err := result.Validate(); err != nil {
			return wrap(ErrConsistency, "review application produced an invalid run projection", err)
		}
		resultJSON, err := marshalResult(result)
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, command.RunID, newVersion, domain.EventReviewApplied, command.StageName,
			command.IdempotencyKey, commandDigest, resultJSON, command.At)
	})
	return result, err
}

func pendingReviewTx(ctx context.Context, queryer rowQuerier, runID domain.RunID) (*domain.ReviewDecision, error) {
	return scanReview(queryer.QueryRowContext(ctx, reviewSelect+" WHERE run_id = ? AND state = 'PENDING'", string(runID)))
}

func reviewByIDTx(ctx context.Context, queryer rowQuerier, runID domain.RunID, id domain.ReviewDecisionID) (domain.ReviewDecision, error) {
	result, err := scanReview(queryer.QueryRowContext(ctx, reviewSelect+" WHERE run_id = ? AND review_id = ?", string(runID), string(id)))
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	if result == nil {
		return domain.ReviewDecision{}, wrap(ErrNotFound, "review decision does not exist", nil)
	}
	return *result, nil
}

const reviewSelect = `
	SELECT review_id, run_id, kind, state, expected_run_version, run_version, workflow_revision,
		stage_name, stage_input_digest, evidence_digest, policy_digest,
		requested_edits_digest, waiver_scope_digest, external_condition_digest,
		budget_increase_json, waivable_gate, reviewer, reason, created_at, applied_at
	FROM review_decisions`

func scanReview(row *sql.Row) (*domain.ReviewDecision, error) {
	var result domain.ReviewDecision
	var id, runID, kind, state, stage, input, evidence, policy, created string
	var edits, waiver, condition, applied sql.NullString
	var budgetJSON []byte
	var waivable int
	err := row.Scan(
		&id, &runID, &kind, &state, &result.ExpectedRunVersion, &result.RunVersion, &result.WorkflowRevision,
		&stage, &input, &evidence, &policy, &edits, &waiver, &condition,
		&budgetJSON, &waivable, &result.Reviewer, &result.Reason, &created, &applied,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.ID, result.RunID = domain.ReviewDecisionID(id), domain.RunID(runID)
	result.Kind, result.State, result.StageName = domain.ReviewDecisionKind(kind), domain.ReviewDecisionState(state), domain.StageName(stage)
	result.StageInputDigest, result.EvidenceDigest, result.PolicyDigest = domain.Digest(input), domain.Digest(evidence), domain.Digest(policy)
	if edits.Valid {
		value := domain.Digest(edits.String)
		result.RequestedEditsDigest = &value
	}
	if waiver.Valid {
		value := domain.Digest(waiver.String)
		result.WaiverScopeDigest = &value
	}
	if condition.Valid {
		value := domain.Digest(condition.String)
		result.ExternalConditionDigest = &value
	}
	if len(budgetJSON) != 0 {
		if err := json.Unmarshal(budgetJSON, &result.BudgetIncrease); err != nil {
			return nil, wrap(ErrConsistency, "stored review budget increase is invalid", err)
		}
	}
	result.WaivableGate = waivable == 1
	var errTime error
	if result.CreatedAt, errTime = parseTime(created); errTime != nil {
		return nil, errTime
	}
	if applied.Valid {
		value, err := parseTime(applied.String)
		if err != nil {
			return nil, err
		}
		result.AppliedAt = &value
	}
	if err := result.Validate(); err != nil {
		return nil, wrap(ErrConsistency, "stored review decision is invalid", err)
	}
	return &result, nil
}

func positiveTime(value time.Time) bool { return !value.IsZero() }
