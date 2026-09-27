package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const workbenchEventLimit = 50

// ReadWorkbenchRun returns the complete durable workbench projection from a
// single SQLite read transaction. Budget is deliberately read alongside the
// run rather than through BudgetSnapshot, which would permit mixed snapshots.
func (s *Store) ReadWorkbenchRun(ctx context.Context, runID domain.RunID) (port.WorkbenchReadSnapshot, error) {
	var out port.WorkbenchReadSnapshot
	if err := runID.Validate(); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out.Run, err = readRun(ctx, tx, runID)
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT submitted_request_json, redacted_effective_config_json FROM runs WHERE run_id=?`, string(runID)).Scan(&out.RequestJSON, &out.ConfigJSON); err != nil {
		return out, err
	}
	out.RequestJSON = append([]byte(nil), out.RequestJSON...)
	out.ConfigJSON = append([]byte(nil), out.ConfigJSON...)
	out.Budget.Remaining = make(map[domain.BudgetDimension]int64)
	out.BudgetUsed = make(map[domain.BudgetDimension]int64)
	out.BudgetReserved = make(map[domain.BudgetDimension]int64)
	rows, err := tx.QueryContext(ctx, `SELECT request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version FROM budget_accounts WHERE run_id=? ORDER BY dimension`, string(runID))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var reqDigest, dimRaw string
		var limit, reserved, used, version int64
		if err := rows.Scan(&reqDigest, &dimRaw, &limit, &reserved, &used, &version); err != nil {
			rows.Close()
			return out, err
		}
		if reqDigest != string(out.Run.RequestDigest) {
			rows.Close()
			return out, wrap(ErrConsistency, "budget request binding differs from run", nil)
		}
		dim := domain.BudgetDimension(dimRaw)
		account := domain.BudgetAccount{RunID: runID, RequestSnapshotDigest: domain.Digest(reqDigest), Dimension: dim, Limit: limit, Reserved: reserved, Consumed: used, Version: version}
		if err := account.Validate(); err != nil {
			rows.Close()
			return out, wrap(ErrConsistency, "budget account invalid", err)
		}
		out.Budget.Limits = setWorkbenchLimit(out.Budget.Limits, dim, limit)
		out.Budget.Remaining[dim], out.BudgetUsed[dim], out.BudgetReserved[dim] = account.Remaining(), used, reserved
		if version > out.Budget.Version {
			out.Budget.Version = version
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	var extra struct{ MaxPackageBytes, MaxMutationsPerStage int64 }
	if err := tx.QueryRowContext(ctx, `SELECT max_package_bytes,max_mutations_per_stage FROM runs WHERE run_id=?`, string(runID)).Scan(&extra.MaxPackageBytes, &extra.MaxMutationsPerStage); err != nil {
		return out, err
	}
	out.Budget.Limits.MaxPackageBytes, out.Budget.Limits.MaxMutationsPerStage = extra.MaxPackageBytes, extra.MaxMutationsPerStage
	if err := out.Budget.Validate(); err != nil {
		return out, wrap(ErrConsistency, "budget snapshot invalid", err)
	}
	rows, err = tx.QueryContext(ctx, `SELECT stage_name,ordinal,state FROM stage_records WHERE run_id=? ORDER BY ordinal`, string(runID))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var stage port.WorkbenchStageSnapshot
		var state string
		if err := rows.Scan(&stage.Name, &stage.Ordinal, &state); err != nil {
			rows.Close()
			return out, err
		}
		stage.State = domain.StageState(state)
		if err := stage.Name.Validate(); err != nil {
			rows.Close()
			return out, err
		}
		if stage.Ordinal != len(out.Stages)+1 || !stage.State.Valid() {
			rows.Close()
			return out, wrap(ErrConsistency, "stage sequence invalid", nil)
		}
		attempts, err := readWorkbenchAttempts(ctx, tx, runID, stage.Name)
		if err != nil {
			rows.Close()
			return out, err
		}
		stage.Attempts = attempts
		out.Stages = append(out.Stages, stage)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	out.PendingReview, err = readPendingWorkbenchReview(ctx, tx, runID)
	if err != nil {
		return out, err
	}
	out.PendingCancel, err = pendingCancelTx(ctx, tx, runID)
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT occurrence.occurrence_id,occurrence.digest,occurrence.size,occurrence.role,occurrence.logical_path,occurrence.stage_name,occurrence.media_type FROM artifact_occurrences occurrence JOIN blobs blob ON blob.digest=occurrence.digest AND blob.size=occurrence.size WHERE occurrence.run_id=? AND blob.state='READY' AND EXISTS (SELECT 1 FROM stage_records stage JOIN stage_attempts attempt ON attempt.run_id=stage.run_id AND attempt.stage_name=stage.stage_name WHERE stage.run_id=occurrence.run_id AND stage.stage_name=occurrence.stage_name AND stage.state='SUCCEEDED' AND attempt.attempt_id=occurrence.attempt_id AND attempt.state='SUCCEEDED' AND attempt.ordinal=stage.attempt_count) ORDER BY occurrence.created_at,occurrence.occurrence_id`, string(runID))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item port.WorkbenchArtifactSnapshot
		var id, digest, role, path string
		if err := rows.Scan(&id, &digest, &item.Blob.Size, &role, &path, &item.StageName, &item.MediaType); err != nil {
			rows.Close()
			return out, err
		}
		item.OccurrenceID, item.Blob.Digest, item.Role, item.LogicalPath = domain.ArtifactOccurrenceID(id), domain.Digest(digest), domain.ArtifactRole(role), domain.SafeRelPath(path)
		if err := item.CommittedArtifactRef.Validate(); err != nil {
			rows.Close()
			return out, wrap(ErrConsistency, "artifact occurrence invalid", err)
		}
		out.Artifacts = append(out.Artifacts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT version,event_type,stage_name,idempotency_key,command_digest,occurred_at FROM run_events WHERE run_id=? ORDER BY version DESC LIMIT ?`, string(runID), workbenchEventLimit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var event domain.RunEvent
		var kind string
		var stage sql.NullString
		var at string
		if err := rows.Scan(&event.Version, &kind, &stage, &event.IdempotencyKey, &event.CommandDigest, &at); err != nil {
			rows.Close()
			return out, err
		}
		event.RunID, event.Type = runID, domain.RunEventType(kind)
		if stage.Valid {
			event.StageName = domain.StageName(stage.String)
		}
		if event.OccurredAt, err = parseTime(at); err != nil {
			rows.Close()
			return out, err
		}
		if err := event.Validate(); err != nil {
			rows.Close()
			return out, wrap(ErrConsistency, "run event invalid", err)
		}
		out.RecentEvents = append(out.RecentEvents, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for i, j := 0, len(out.RecentEvents)-1; i < j; i, j = i+1, j-1 {
		out.RecentEvents[i], out.RecentEvents[j] = out.RecentEvents[j], out.RecentEvents[i]
	}
	if len(out.RecentEvents) > 0 {
		out.BeforeVersion = out.RecentEvents[0].Version
	}
	if err := tx.Commit(); err != nil {
		return port.WorkbenchReadSnapshot{}, err
	}
	return out, nil
}

func setWorkbenchLimit(l domain.BudgetLimits, d domain.BudgetDimension, v int64) domain.BudgetLimits {
	switch d {
	case domain.BudgetLLMCalls:
		l.MaxLLMCalls = v
	case domain.BudgetSimilarityCalls:
		l.MaxSimilarityCalls = v
	case domain.BudgetLLMInputTokens:
		l.MaxLLMInputTokens = v
	case domain.BudgetLLMOutputTokens:
		l.MaxLLMOutputTokens = v
	case domain.BudgetExternalCostMicroUSD:
		l.MaxLLMCostMicroUSD = v
	case domain.BudgetSimilarityCostMicroUSD:
		l.MaxSimilarityCostMicroUSD = v
	case domain.BudgetDockerContainerCreates:
		l.MaxSandboxCreates = v
	case domain.BudgetArtifactPhysicalNewBytes:
		l.MaxArtifactBytes = v
	case domain.BudgetActiveTimeNS:
		l.MaxActiveTimeMilliseconds = v / 1e6
	}
	return l
}

func readWorkbenchAttempts(ctx context.Context, q *sql.Tx, runID domain.RunID, stage domain.StageName) ([]domain.StageAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT attempt_id,ordinal,state,input_digest,output_digest,cause,started_at,finished_at FROM stage_attempts WHERE run_id=? AND stage_name=? ORDER BY ordinal`, string(runID), string(stage))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.StageAttempt
	for rows.Next() {
		var a domain.StageAttempt
		var state, input string
		var output, cause, finished sql.NullString
		var started string
		if err := rows.Scan(&a.AttemptID, &a.Ordinal, &state, &input, &output, &cause, &started, &finished); err != nil {
			return nil, err
		}
		a.RunID, a.StageName, a.State, a.InputDigest = runID, stage, domain.StageAttemptState(state), domain.Digest(input)
		if output.Valid {
			d := domain.Digest(output.String)
			a.OutputDigest = &d
		}
		if cause.Valid {
			c := domain.ExecutionCause(cause.String)
			a.Cause = &c
		}
		if a.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if finished.Valid {
			t, e := parseTime(finished.String)
			if e != nil {
				return nil, e
			}
			a.FinishedAt = &t
		}
		if err := a.Validate(); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func readPendingWorkbenchReview(ctx context.Context, q rowQuerier, runID domain.RunID) (*domain.ReviewDecision, error) {
	var v domain.ReviewDecision
	var kind, state, stage, input, evidence, policy string
	var edits, waiver, external, budget, applied sql.NullString
	var gate bool
	var created string
	err := q.QueryRowContext(ctx, `SELECT review_id,kind,state,expected_run_version,run_version,workflow_revision,stage_name,stage_input_digest,evidence_digest,policy_digest,requested_edits_digest,waiver_scope_digest,external_condition_digest,budget_increase_json,waivable_gate,reviewer,reason,created_at,applied_at FROM review_decisions WHERE run_id=? AND state='PENDING'`, string(runID)).Scan(&v.ID, &kind, &state, &v.ExpectedRunVersion, &v.RunVersion, &v.WorkflowRevision, &stage, &input, &evidence, &policy, &edits, &waiver, &external, &budget, &gate, &v.Reviewer, &v.Reason, &created, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.RunID, v.Kind, v.State, v.StageName, v.StageInputDigest, v.EvidenceDigest, v.PolicyDigest = runID, domain.ReviewDecisionKind(kind), domain.ReviewDecisionState(state), domain.StageName(stage), domain.Digest(input), domain.Digest(evidence), domain.Digest(policy)
	for src, dst := range map[*sql.NullString]**domain.Digest{&edits: &v.RequestedEditsDigest, &waiver: &v.WaiverScopeDigest, &external: &v.ExternalConditionDigest} {
		if src.Valid {
			d := domain.Digest(src.String)
			*dst = &d
		}
	}
	if budget.Valid && budget.String != "" {
		if err := json.Unmarshal([]byte(budget.String), &v.BudgetIncrease); err != nil {
			return nil, err
		}
	}
	v.WaivableGate = gate
	if v.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if applied.Valid {
		t, e := parseTime(applied.String)
		if e != nil {
			return nil, e
		}
		v.AppliedAt = &t
	}
	if err := v.Validate(); err != nil {
		return nil, fmt.Errorf("pending review invalid: %w", err)
	}
	return &v, nil
}

// ReadWorkbenchArtifact validates occurrence scope and publication state in
// SQLite. The application then verifies bytes against this immutable digest.
func (s *Store) ReadWorkbenchArtifact(ctx context.Context, runID domain.RunID, id domain.ArtifactOccurrenceID) (port.WorkbenchArtifactSnapshot, error) {
	var a port.WorkbenchArtifactSnapshot
	if err := runID.Validate(); err != nil {
		return a, err
	}
	if err := id.Validate(); err != nil {
		return a, err
	}
	var occ, digest, role, path string
	err := s.db.QueryRowContext(ctx, `SELECT occurrence.occurrence_id,occurrence.digest,occurrence.size,occurrence.role,occurrence.logical_path,occurrence.stage_name,occurrence.media_type FROM artifact_occurrences occurrence JOIN blobs blob ON blob.digest=occurrence.digest AND blob.size=occurrence.size WHERE occurrence.run_id=? AND occurrence.occurrence_id=? AND blob.state='READY' AND EXISTS (SELECT 1 FROM stage_records stage JOIN stage_attempts attempt ON attempt.run_id=stage.run_id AND attempt.stage_name=stage.stage_name WHERE stage.run_id=occurrence.run_id AND stage.stage_name=occurrence.stage_name AND stage.state='SUCCEEDED' AND attempt.attempt_id=occurrence.attempt_id AND attempt.state='SUCCEEDED' AND attempt.ordinal=stage.attempt_count)`, string(runID), string(id)).Scan(&occ, &digest, &a.Blob.Size, &role, &path, &a.StageName, &a.MediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return a, wrap(ErrNotFound, "committed artifact occurrence not found", err)
	}
	if err != nil {
		return a, err
	}
	a.OccurrenceID, a.Blob.Digest, a.Role, a.LogicalPath = domain.ArtifactOccurrenceID(occ), domain.Digest(digest), domain.ArtifactRole(role), domain.SafeRelPath(path)
	if err := a.CommittedArtifactRef.Validate(); err != nil {
		return a, wrap(ErrConsistency, "artifact occurrence invalid", err)
	}
	return a, nil
}
