package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
)

// PrepareExecution seals the complete resource plan before any external
// Docker call. The execution and all resource rows commit atomically.
func (s *Store) PrepareExecution(ctx context.Context, request domain.PrepareExecutionRequest) (domain.SandboxExecution, error) {
	if err := request.Validate(); err != nil {
		return domain.SandboxExecution{}, err
	}
	digest, _, err := digestJSON(request)
	if err != nil {
		return domain.SandboxExecution{}, err
	}
	var result domain.SandboxExecution
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var storedDigest string
		err := tx.QueryRowContext(ctx, `SELECT command_digest FROM sandbox_executions WHERE sandbox_execution_id = ?`, string(request.ExecutionID)).Scan(&storedDigest)
		if err == nil {
			if storedDigest != string(digest) {
				return wrap(ErrConsistency, "sandbox execution idempotency content differs", nil)
			}
			result, err = readSandboxExecution(ctx, tx, request.ExecutionID)
			if err == nil {
				result.Resources, err = sandboxResourcesTx(ctx, tx, request.ExecutionID)
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var existingKey string
		err = tx.QueryRowContext(ctx, `SELECT command_digest FROM sandbox_executions WHERE run_id = ? AND idempotency_key = ?`, string(request.RunID), request.IdempotencyKey).Scan(&existingKey)
		if err == nil {
			if existingKey != string(digest) {
				return wrap(ErrConsistency, "sandbox execution idempotency key was reused", nil)
			}
			return readSandboxExecutionByKey(ctx, tx, request.RunID, request.IdempotencyKey, &result)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := sandboxRunGuard(ctx, tx, request.RunID, request.StageName, request.AttemptID, true); err != nil {
			return err
		}
		result = domain.SandboxExecution{
			ID: request.ExecutionID, RunID: request.RunID, AttemptID: request.AttemptID, StageName: request.StageName,
			LogicalOperationID: request.LogicalOperationID, ScopeDigest: request.ScopeDigest, PlanDigest: request.PlanDigest,
			EngineIdentityDigest: request.EngineIdentityDigest, WatchdogControlRef: request.WatchdogControlRef,
			WatchdogTokenDigest: request.WatchdogTokenDigest, State: domain.SandboxExecutionPlanned,
			LifecycleVersion: 1, SafetyDeadlineUTC: request.SafetyDeadlineUTC, CleanupDeadlineUTC: request.CleanupDeadlineUTC,
			CreatedAt: request.At, UpdatedAt: request.At,
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_executions(
			sandbox_execution_id, run_id, stage_name, attempt_id, logical_operation_id, scope_digest, plan_digest,
			engine_identity_digest, watchdog_control_ref, watchdog_token_digest, state, lifecycle_version, cleanup_version,
			safety_deadline_utc, cleanup_deadline_utc, idempotency_key, command_digest, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PLANNED', 1, 0, ?, ?, ?, ?, ?, ?)`,
			string(result.ID), string(result.RunID), string(result.StageName), string(result.AttemptID), result.LogicalOperationID,
			string(result.ScopeDigest), string(result.PlanDigest), string(result.EngineIdentityDigest), result.WatchdogControlRef,
			string(result.WatchdogTokenDigest), formatTime(result.SafetyDeadlineUTC), formatTime(result.CleanupDeadlineUTC), request.IdempotencyKey,
			string(digest), formatTime(result.CreatedAt), formatTime(result.UpdatedAt))
		if err != nil {
			return fmt.Errorf("insert sandbox execution: %w", err)
		}
		for index, resource := range request.Resources {
			if resource.ID == "" {
				return fmt.Errorf("resource %d has no stable id", index)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO sandbox_resources(
				resource_id, sandbox_execution_id, plan_ordinal, resource_kind, resource_role, physical_call_id,
				deterministic_name, expected_labels_digest, labels_digest, engine_resource_id, engine_identity_digest,
				cgroup_identity_digest, creation_nonce, phase, version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PLANNED', 1, ?, ?)`,
				string(resource.ID), string(result.ID), resource.PlanOrdinal, resource.Kind, resource.Role, nullableAttemptCall(resource.PhysicalCallID),
				resource.DeterministicName, string(resource.ExpectedLabelsDigest), nullableDigest(resource.LabelsDigest), nullableString(resource.EngineResourceID),
				string(resource.EngineIdentityDigest), nullableDigestPtr(resource.CgroupIdentityDigest), nullableString(resource.CreationNonce),
				formatTime(request.At), formatTime(request.At))
			if err != nil {
				return fmt.Errorf("insert sandbox resource %d: %w", index, err)
			}
		}
		result.Resources = request.Resources
		return nil
	})
	return result, err
}

func (s *Store) RecordWatchdogArmed(ctx context.Context, command domain.WatchdogArmed) error {
	if err := command.Validate(); err != nil {
		return err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var executionID string
		var state string
		var version int64
		var controlRef, token, armDigest sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT sandbox_execution_id, state, lifecycle_version, watchdog_control_ref, watchdog_token_digest, arm_command_digest FROM sandbox_executions WHERE sandbox_execution_id = ?`, string(command.ExecutionID)).Scan(&executionID, &state, &version, &controlRef, &token, &armDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "sandbox execution does not exist", err)
		}
		if err != nil {
			return err
		}
		if armDigest.Valid && armDigest.String == string(digest) {
			return nil
		}
		if version != command.ExpectedVersion {
			return wrap(ErrVersionConflict, "sandbox execution version changed", nil)
		}
		if state == string(domain.SandboxExecutionArmed) {
			return wrap(ErrConsistency, "watchdog arm command differs from persisted command", nil)
		}
		if state != string(domain.SandboxExecutionPlanned) {
			return wrap(ErrInvalidTransition, "sandbox execution is not PLANNED", nil)
		}
		if err := sandboxExecutionRunGuard(ctx, tx, command.ExecutionID, true); err != nil {
			return err
		}
		if !controlRef.Valid || !token.Valid || command.ControlRecordRef != controlRef.String || string(command.TokenDigest) != token.String {
			return wrap(ErrConsistency, "watchdog identity does not match execution", nil)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_watchdog_controls(control_id, sandbox_execution_id, process_record_ref, control_file_digest, token_digest, armed_at) VALUES (?, ?, ?, ?, ?, ?)`,
			command.ControlRecordRef, string(command.ExecutionID), command.ControlRecordRef, string(command.ControlFileDigest), string(command.TokenDigest), formatTime(command.At)); err != nil {
			return fmt.Errorf("persist watchdog control: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_executions SET state='ARMED', lifecycle_version=lifecycle_version+1, arm_idempotency_key=?, arm_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND lifecycle_version=?`,
			command.IdempotencyKey, string(digest), formatTime(command.At), string(command.ExecutionID), command.ExpectedVersion)
		return err
	})
}

func (s *Store) BeginResourceCreate(ctx context.Context, command domain.BeginResourceCreate) (domain.PreCreateRequest, error) {
	if err := command.Validate(); err != nil {
		return domain.PreCreateRequest{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.PreCreateRequest{}, err
	}
	var result domain.PreCreateRequest
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var resource domain.SandboxResource
		if err := readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &resource); err != nil {
			return err
		}
		if resource.Version != command.ExpectedVersion {
			var stored string
			if err := tx.QueryRowContext(ctx, `SELECT last_command_digest FROM sandbox_resources WHERE resource_id=?`, string(command.ResourceID)).Scan(&stored); err == nil && stored == string(digest) {
				result = domain.PreCreateRequest{ExecutionID: command.ExecutionID, ResourceID: command.ResourceID, Resource: resource, Version: resource.Version}
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox resource version changed", nil)
		}
		if resource.Phase != domain.SandboxResourcePlanned {
			return wrap(ErrInvalidTransition, "sandbox resource is not PLANNED", nil)
		}
		var execState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM sandbox_executions WHERE sandbox_execution_id=?`, string(command.ExecutionID)).Scan(&execState); err != nil {
			return err
		}
		if execState != string(domain.SandboxExecutionArmed) && execState != string(domain.SandboxExecutionRunning) {
			return wrap(ErrInvalidTransition, "sandbox execution is not armed", nil)
		}
		if err := sandboxExecutionRunGuard(ctx, tx, command.ExecutionID, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase='CREATING', version=version+1, last_idempotency_key=?, last_command_digest=?, updated_at=? WHERE resource_id=? AND version=? AND phase='PLANNED'`, command.IdempotencyKey, string(digest), formatTime(command.At), string(command.ResourceID), command.ExpectedVersion); err != nil {
			return err
		}
		if resource.Version == command.ExpectedVersion {
			resource.Version++
			resource.Phase = domain.SandboxResourceCreating
			resource.UpdatedAt = command.At
		}
		result = domain.PreCreateRequest{ExecutionID: command.ExecutionID, ResourceID: command.ResourceID, Resource: resource, Version: resource.Version}
		return nil
	})
	return result, err
}

func (s *Store) RecordPreCreateACK(ctx context.Context, ack domain.PreCreateACK) error {
	if err := ack.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		if err := sandboxExecutionRunGuard(ctx, tx, ack.ExecutionID, true); err != nil {
			return err
		}
		var executionRef string
		if err := tx.QueryRowContext(ctx, `SELECT watchdog_control_ref FROM sandbox_executions WHERE sandbox_execution_id=?`, string(ack.ExecutionID)).Scan(&executionRef); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return wrap(ErrNotFound, "sandbox execution does not exist", err)
			}
			return err
		}
		if ack.WatchdogRecordRef != executionRef {
			return wrap(ErrConsistency, "pre-create ACK watchdog reference differs from execution", nil)
		}
		var currentVersion int64
		var currentPhase string
		if err := tx.QueryRowContext(ctx, `SELECT version, phase FROM sandbox_resources WHERE sandbox_execution_id=? AND resource_id=?`, string(ack.ExecutionID), string(ack.ResourceID)).Scan(&currentVersion, &currentPhase); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return wrap(ErrNotFound, "sandbox resource does not exist", err)
			}
			return err
		}
		if currentVersion != ack.ResourceVersion || currentPhase != string(domain.SandboxResourceCreating) {
			return wrap(ErrInvalidTransition, "pre-create ACK does not match the CREATING resource version", nil)
		}
		var storedVersion int64
		var storedDigest, storedRef, storedKey, storedAt string
		err := tx.QueryRowContext(ctx, `SELECT resource_version, labels_digest, watchdog_record_ref, idempotency_key, acked_at FROM sandbox_precreate_acks WHERE sandbox_execution_id=? AND resource_id=?`, string(ack.ExecutionID), string(ack.ResourceID)).Scan(&storedVersion, &storedDigest, &storedRef, &storedKey, &storedAt)
		if err == nil {
			if storedVersion == ack.ResourceVersion && storedDigest == string(ack.LabelsDigest) && storedRef == ack.WatchdogRecordRef && storedKey == ack.IdempotencyKey && storedAt == formatTime(ack.At) {
				return nil
			}
			return wrap(ErrConsistency, "pre-create ACK content differs", nil)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_precreate_acks(sandbox_execution_id, resource_id, resource_version, labels_digest, watchdog_record_ref, idempotency_key, acked_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, string(ack.ExecutionID), string(ack.ResourceID), ack.ResourceVersion, string(ack.LabelsDigest), ack.WatchdogRecordRef, ack.IdempotencyKey, formatTime(ack.At))
		return err
	})
}

func (s *Store) AdvanceResource(ctx context.Context, command domain.AdvanceResourceRequest) (domain.SandboxResource, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxResource{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxResource{}, err
	}
	var result domain.SandboxResource
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		if err := readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result); err != nil {
			return err
		}
		if result.Version != command.ExpectedVersion {
			var lastDigest string
			_ = tx.QueryRowContext(ctx, `SELECT last_command_digest FROM sandbox_resources WHERE resource_id=?`, string(command.ResourceID)).Scan(&lastDigest)
			if resultPhaseMatchesCommand(result, command) && lastDigest == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox resource version changed", nil)
		}
		if !allowedSandboxResourceTransition(result.Phase, command.Phase) {
			return wrap(ErrInvalidTransition, fmt.Sprintf("sandbox resource cannot advance %s -> %s", result.Phase, command.Phase), nil)
		}
		if err := sandboxExecutionRunGuard(ctx, tx, command.ExecutionID, true); err != nil {
			return err
		}
		if command.Phase == domain.SandboxResourceDispatching {
			var ackCount int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_precreate_acks WHERE sandbox_execution_id=? AND resource_id=? AND resource_version=?`, string(command.ExecutionID), string(command.ResourceID), result.Version).Scan(&ackCount); err != nil {
				return err
			}
			if ackCount != 1 {
				return wrap(ErrInvalidTransition, "resource dispatch requires persisted watchdog pre-create ACK", nil)
			}
		}
		var executionDigest string
		if err := tx.QueryRowContext(ctx, `SELECT engine_identity_digest FROM sandbox_executions WHERE sandbox_execution_id=?`, string(command.ExecutionID)).Scan(&executionDigest); err != nil {
			return err
		}
		if command.EngineIdentityDigest != "" && string(command.EngineIdentityDigest) != executionDigest {
			return wrap(ErrConsistency, "engine identity changed", nil)
		}
		if result.EngineIdentityDigest != domain.Digest(executionDigest) {
			return wrap(ErrConsistency, "resource engine identity changed", nil)
		}
		if result.LabelsDigest != "" && command.LabelsDigest != "" && result.LabelsDigest != command.LabelsDigest {
			return wrap(ErrConsistency, "resource labels identity changed", nil)
		}
		if command.Phase == domain.SandboxResourceCompleted || command.Phase == domain.SandboxResourceStarted || command.Phase == domain.SandboxResourceStopped || command.Phase == domain.SandboxResourceCleaned {
			if strings.TrimSpace(command.EngineResourceID) == "" {
				return errors.New("settled sandbox resource requires exact engine ID")
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase=?, version=version+1, physical_call_id=COALESCE(?, physical_call_id), engine_resource_id=COALESCE(?, engine_resource_id), engine_identity_digest=?, labels_digest=COALESCE(?, labels_digest), last_idempotency_key=?, last_command_digest=?, updated_at=? WHERE resource_id=? AND sandbox_execution_id=? AND version=?`,
			string(command.Phase), nullableAttemptCall(command.PhysicalCallID), nullableString(command.EngineResourceID), executionDigest, nullableDigest(command.LabelsDigest), command.IdempotencyKey, string(digest), formatTime(command.At), string(command.ResourceID), string(command.ExecutionID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		return readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result)
	})
	return result, err
}

// RecordResourceStopProof is the sole persistence boundary for STOPPED. The
// caller must have already performed stop, kill, wait, and ownership inspect;
// only its immutable digest enters the transaction.
func (s *Store) RecordResourceStopProof(ctx context.Context, command domain.RecordResourceStopProofCommand) (domain.SandboxResource, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxResource{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxResource{}, err
	}
	var result domain.SandboxResource
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if err := readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result); err != nil {
			return err
		}
		if result.Version != command.ExpectedVersion {
			var last string
			_ = tx.QueryRowContext(ctx, `SELECT last_command_digest FROM sandbox_resources WHERE resource_id=?`, string(command.ResourceID)).Scan(&last)
			if result.Phase == domain.SandboxResourceStopped && last == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox resource version changed", nil)
		}
		if result.Phase != domain.SandboxResourceCleanupPending && result.Phase != domain.SandboxResourceStarted && result.Phase != domain.SandboxResourceCompleted && result.Phase != domain.SandboxResourceUnknown && result.Phase != domain.SandboxResourceStopped {
			return wrap(ErrInvalidTransition, "resource stop proof requires an externally created resource", nil)
		}
		if strings.TrimSpace(result.EngineResourceID) != "" && result.EngineResourceID != command.EngineResourceID {
			return wrap(ErrConsistency, "stop proof engine identity differs", nil)
		}
		if result.EngineIdentityDigest != command.EngineIdentityDigest {
			return wrap(ErrConsistency, "stop proof Engine identity differs", nil)
		}
		if result.LabelsDigest != "" && command.LabelsDigest != "" && result.LabelsDigest != command.LabelsDigest {
			return wrap(ErrConsistency, "stop proof labels identity differs", nil)
		}
		_, err := tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase='STOPPED', version=version+1, engine_resource_id=?, engine_identity_digest=?, labels_digest=COALESCE(?, labels_digest), stop_proof_digest=?, stop_proof_kind=?, stop_proof_at=?, last_idempotency_key=?, last_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND resource_id=? AND version=?`,
			command.EngineResourceID, string(command.EngineIdentityDigest), nullableDigest(command.LabelsDigest), string(command.ProofDigest), command.ProofKind, formatTime(command.At), "stop_"+string(command.ResourceID), string(digest), formatTime(command.At), string(command.ExecutionID), string(command.ResourceID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		return readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result)
	})
	return result, err
}

// RecordResourceCleaned persists the removal observation after a stop proof.
func (s *Store) RecordResourceCleaned(ctx context.Context, command domain.RecordResourceCleanedCommand) (domain.SandboxResource, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxResource{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxResource{}, err
	}
	var result domain.SandboxResource
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if err := readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result); err != nil {
			return err
		}
		if result.Version != command.ExpectedVersion {
			var last string
			_ = tx.QueryRowContext(ctx, `SELECT last_command_digest FROM sandbox_resources WHERE resource_id=?`, string(command.ResourceID)).Scan(&last)
			if result.Phase == domain.SandboxResourceCleaned && last == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox resource version changed", nil)
		}
		if result.Phase != domain.SandboxResourceStopped {
			return wrap(ErrInvalidTransition, "resource cleanup requires persisted stop proof", nil)
		}
		if result.StopProofDigest == "" || result.EngineResourceID != command.EngineResourceID || result.EngineIdentityDigest != command.EngineIdentityDigest {
			return wrap(ErrConsistency, "cleanup identity or stop proof differs", nil)
		}
		_, err := tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase='CLEANED', version=version+1, cleanup_evidence_digest=?, last_idempotency_key=?, last_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND resource_id=? AND version=?`,
			string(command.EvidenceDigest), "clean_"+string(command.ResourceID), string(digest), formatTime(command.At), string(command.ExecutionID), string(command.ResourceID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		return readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result)
	})
	return result, err
}

// RecordResourceInterrupted settles a planned resource for which no external
// create crossed the boundary. The reason is persisted as its stop proof.
func (s *Store) RecordResourceInterrupted(ctx context.Context, command domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxResource{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxResource{}, err
	}
	var result domain.SandboxResource
	err = s.immediate(ctx, func(tx *immediateTx) error {
		if err := readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result); err != nil {
			return err
		}
		if result.Version != command.ExpectedVersion {
			var last string
			_ = tx.QueryRowContext(ctx, `SELECT last_command_digest FROM sandbox_resources WHERE resource_id=?`, string(command.ResourceID)).Scan(&last)
			if result.Phase == domain.SandboxResourceInterrupted && last == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox resource version changed", nil)
		}
		if result.Phase != domain.SandboxResourcePlanned {
			return wrap(ErrInvalidTransition, "only an uncreated planned resource may be interrupted", nil)
		}
		_, err := tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase='INTERRUPTED', version=version+1, stop_proof_digest=?, stop_proof_kind='NO_CREATE', stop_proof_at=?, last_idempotency_key=?, last_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND resource_id=? AND version=?`,
			string(command.ReasonDigest), formatTime(command.At), "interrupt_"+string(command.ResourceID), string(digest), formatTime(command.At), string(command.ExecutionID), string(command.ResourceID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		return readSandboxResource(ctx, tx, command.ExecutionID, command.ResourceID, &result)
	})
	return result, err
}

func (s *Store) MarkCleanupPending(ctx context.Context, command domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxExecution{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxExecution{}, err
	}
	var result domain.SandboxExecution
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		stored, err := readSandboxExecution(ctx, tx, command.ExecutionID)
		if err != nil {
			return err
		}
		result = stored
		if result.LifecycleVersion != command.ExpectedVersion {
			var storedDigest string
			_ = tx.QueryRowContext(ctx, `SELECT last_cleanup_command_digest FROM sandbox_executions WHERE sandbox_execution_id=?`, string(command.ExecutionID)).Scan(&storedDigest)
			if result.State == domain.SandboxExecutionCleanupPending && storedDigest == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox execution version changed", nil)
		}
		if result.State == domain.SandboxExecutionCleaned {
			var storedDigest string
			_ = tx.QueryRowContext(ctx, `SELECT last_cleanup_command_digest FROM sandbox_executions WHERE sandbox_execution_id=?`, string(command.ExecutionID)).Scan(&storedDigest)
			if storedDigest == string(digest) {
				return nil
			}
			return wrap(ErrConsistency, "cleanup command differs from persisted command", nil)
		}
		if result.State == domain.SandboxExecutionInterrupted { /* interrupted cleanup is still settleable */
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_executions SET state='CLEANUP_PENDING', lifecycle_version=lifecycle_version+1, cleanup_version=cleanup_version+1, last_cleanup_reason=?, last_cleanup_idempotency_key=?, last_cleanup_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND lifecycle_version=?`, command.Reason, command.IdempotencyKey, string(digest), formatTime(command.At), string(command.ExecutionID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		stored, err = readSandboxExecution(ctx, tx, command.ExecutionID)
		if err == nil {
			result = stored
		}
		return err
	})
	return result, err
}

func (s *Store) FinishCleanup(ctx context.Context, command domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	if err := command.Validate(); err != nil {
		return domain.SandboxExecution{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.SandboxExecution{}, err
	}
	var result domain.SandboxExecution
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		stored, err := readSandboxExecution(ctx, tx, command.ExecutionID)
		if err != nil {
			return err
		}
		result = stored
		if result.LifecycleVersion != command.ExpectedVersion {
			var storedDigest string
			_ = tx.QueryRowContext(ctx, `SELECT last_cleanup_command_digest FROM sandbox_executions WHERE sandbox_execution_id=?`, string(command.ExecutionID)).Scan(&storedDigest)
			if result.State == domain.SandboxExecutionCleaned && storedDigest == string(digest) {
				return nil
			}
			return wrap(ErrVersionConflict, "sandbox execution version changed", nil)
		}
		if result.State != domain.SandboxExecutionCleanupPending && result.State != domain.SandboxExecutionInterrupted {
			return wrap(ErrInvalidTransition, "sandbox execution is not pending cleanup", nil)
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_resources WHERE sandbox_execution_id=? AND (phase NOT IN ('CLEANED','INTERRUPTED') OR (phase='CLEANED' AND (stop_proof_digest IS NULL OR cleanup_evidence_digest IS NULL)) OR (phase='INTERRUPTED' AND stop_proof_digest IS NULL))`, string(command.ExecutionID)).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return wrap(ErrInvalidTransition, "sandbox resources are not proven stopped", nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_executions SET state='CLEANED', lifecycle_version=lifecycle_version+1, cleanup_version=cleanup_version+1, reconciliation_evidence_digest=?, last_cleanup_idempotency_key=?, last_cleanup_command_digest=?, updated_at=? WHERE sandbox_execution_id=? AND lifecycle_version=?`, string(command.ReconciliationDigest), command.IdempotencyKey, string(digest), formatTime(command.At), string(command.ExecutionID), command.ExpectedVersion)
		if err != nil {
			return err
		}
		stored, err = readSandboxExecution(ctx, tx, command.ExecutionID)
		if err == nil {
			result = stored
		}
		return err
	})
	return result, err
}

// GetSandboxExecution returns the immutable execution identity and current
// lifecycle snapshot. It is intentionally read-only; all state changes go
// through the named lifecycle transactions above.
func (s *Store) GetSandboxExecution(ctx context.Context, executionID domain.SandboxExecutionID) (domain.SandboxExecution, error) {
	if err := executionID.Validate(); err != nil {
		return domain.SandboxExecution{}, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.SandboxExecution{}, err
	}
	defer connection.Close()
	execution, err := readSandboxExecution(ctx, connection, executionID)
	if err != nil {
		return domain.SandboxExecution{}, err
	}
	execution.Resources, err = sandboxResourcesTx(ctx, connection, executionID)
	return execution, err
}

// GetSandboxResources is a descriptive alias for SandboxResources used by
// callers that prefer an explicit read-method naming convention.
func (s *Store) GetSandboxResources(ctx context.Context, executionID domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	return s.SandboxResources(ctx, executionID)
}

// UnfinishedSandboxExecutions and SandboxResources are read-only inputs for
// the Docker reconciler; no external operation is performed by these methods.
func (s *Store) UnfinishedSandboxExecutions(ctx context.Context, runID domain.RunID) ([]domain.SandboxExecution, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `SELECT sandbox_execution_id FROM sandbox_executions WHERE run_id=? AND state NOT IN ('CLEANED','INTERRUPTED') ORDER BY created_at, sandbox_execution_id`, string(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.SandboxExecutionID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.SandboxExecutionID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.SandboxExecution, 0, len(ids))
	for _, id := range ids {
		execution, err := readSandboxExecution(ctx, connection, id)
		if err != nil {
			return nil, err
		}
		result = append(result, execution)
	}
	return result, nil
}

func (s *Store) SandboxResources(ctx context.Context, executionID domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	if err := executionID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `SELECT resource_id FROM sandbox_resources WHERE sandbox_execution_id=? ORDER BY plan_ordinal`, string(executionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.SandboxResourceID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.SandboxResourceID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.SandboxResource, 0, len(ids))
	for _, id := range ids {
		var resource domain.SandboxResource
		if err := readSandboxResource(ctx, connection, executionID, id, &resource); err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, nil
}

type sandboxQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readSandboxExecution(ctx context.Context, queryer sandboxQueryer, id domain.SandboxExecutionID) (domain.SandboxExecution, error) {
	var e domain.SandboxExecution
	var rawID, runID, stage, attempt, state, scope, plan, engine, control, token, safety, cleanup, created, updated string
	var reconciliation sql.NullString
	err := queryer.QueryRowContext(ctx, `SELECT sandbox_execution_id, run_id, stage_name, attempt_id, logical_operation_id, scope_digest, plan_digest, engine_identity_digest, watchdog_control_ref, watchdog_token_digest, state, lifecycle_version, cleanup_version, safety_deadline_utc, cleanup_deadline_utc, created_at, updated_at, reconciliation_evidence_digest FROM sandbox_executions WHERE sandbox_execution_id=?`, string(id)).Scan(&rawID, &runID, &stage, &attempt, &e.LogicalOperationID, &scope, &plan, &engine, &control, &token, &state, &e.LifecycleVersion, &e.CleanupVersion, &safety, &cleanup, &created, &updated, &reconciliation)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SandboxExecution{}, wrap(ErrNotFound, "sandbox execution does not exist", err)
	}
	if err != nil {
		return e, err
	}
	e.ID, e.RunID, e.StageName, e.AttemptID = domain.SandboxExecutionID(rawID), domain.RunID(runID), domain.StageName(stage), domain.AttemptID(attempt)
	e.ScopeDigest, e.PlanDigest, e.EngineIdentityDigest, e.WatchdogControlRef, e.WatchdogTokenDigest, e.State = domain.Digest(scope), domain.Digest(plan), domain.Digest(engine), control, domain.Digest(token), domain.SandboxExecutionState(state)
	if reconciliation.Valid {
		e.ReconciliationEvidenceDigest = domain.Digest(reconciliation.String)
	}
	var parseErr error
	if e.SafetyDeadlineUTC, parseErr = parseTime(safety); parseErr != nil {
		return e, parseErr
	}
	if e.CleanupDeadlineUTC, parseErr = parseTime(cleanup); parseErr != nil {
		return e, parseErr
	}
	if e.CreatedAt, parseErr = parseTime(created); parseErr != nil {
		return e, parseErr
	}
	if e.UpdatedAt, parseErr = parseTime(updated); parseErr != nil {
		return e, parseErr
	}
	if err := e.Validate(); err != nil {
		return e, wrap(ErrConsistency, "stored sandbox execution is invalid", err)
	}
	return e, nil
}

func readSandboxExecutionByKey(ctx context.Context, queryer sandboxQueryer, runID domain.RunID, key string, out *domain.SandboxExecution) error {
	var id string
	if err := queryer.QueryRowContext(ctx, `SELECT sandbox_execution_id FROM sandbox_executions WHERE run_id=? AND idempotency_key=?`, string(runID), key).Scan(&id); err != nil {
		return err
	}
	value, err := readSandboxExecution(ctx, queryer, domain.SandboxExecutionID(id))
	if err == nil {
		*out = value
		if resources, resourceErr := sandboxResourcesTx(ctx, queryer, value.ID); resourceErr == nil {
			out.Resources = resources
		} else {
			return resourceErr
		}
	}
	return err
}

func sandboxResourcesTx(ctx context.Context, queryer sandboxQueryer, executionID domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT resource_id FROM sandbox_resources WHERE sandbox_execution_id=? ORDER BY plan_ordinal`, string(executionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.SandboxResourceID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.SandboxResourceID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.SandboxResource, 0, len(ids))
	for _, id := range ids {
		var resource domain.SandboxResource
		if err := readSandboxResource(ctx, queryer, executionID, id, &resource); err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, nil
}

func readSandboxResource(ctx context.Context, queryer sandboxQueryer, executionID domain.SandboxExecutionID, resourceID domain.SandboxResourceID, out *domain.SandboxResource) error {
	var r domain.SandboxResource
	var id, execID, kind, role, name, expected, engineDigest, phase, created, updated string
	var physicalNull, labelsNull, engineIDNull, cgroupNull, nonceNull, stopDigestNull, stopKindNull, stopAtNull, cleanupEvidenceNull sql.NullString
	err := queryer.QueryRowContext(ctx, `SELECT resource_id, sandbox_execution_id, plan_ordinal, resource_kind, resource_role, physical_call_id, deterministic_name, expected_labels_digest, labels_digest, engine_resource_id, engine_identity_digest, cgroup_identity_digest, creation_nonce, phase, version, created_at, updated_at, stop_proof_digest, stop_proof_kind, stop_proof_at, cleanup_evidence_digest FROM sandbox_resources WHERE sandbox_execution_id=? AND resource_id=?`, string(executionID), string(resourceID)).Scan(&id, &execID, &r.PlanOrdinal, &kind, &role, &physicalNull, &name, &expected, &labelsNull, &engineIDNull, &engineDigest, &cgroupNull, &nonceNull, &phase, &r.Version, &created, &updated, &stopDigestNull, &stopKindNull, &stopAtNull, &cleanupEvidenceNull)
	if errors.Is(err, sql.ErrNoRows) {
		return wrap(ErrNotFound, "sandbox resource does not exist", err)
	}
	if err != nil {
		return err
	}
	r.ID, r.ExecutionID, r.Kind, r.Role, r.DeterministicName, r.ExpectedLabelsDigest, r.EngineIdentityDigest, r.Phase = domain.SandboxResourceID(id), domain.SandboxExecutionID(execID), kind, role, name, domain.Digest(expected), domain.Digest(engineDigest), domain.SandboxResourcePhase(phase)
	if physicalNull.Valid {
		v := domain.AttemptCallID(physicalNull.String)
		r.PhysicalCallID = &v
	}
	if labelsNull.Valid {
		r.LabelsDigest = domain.Digest(labelsNull.String)
	}
	if engineIDNull.Valid {
		r.EngineResourceID = engineIDNull.String
	}
	if cgroupNull.Valid {
		v := domain.Digest(cgroupNull.String)
		r.CgroupIdentityDigest = &v
	}
	if nonceNull.Valid {
		r.CreationNonce = nonceNull.String
	}
	if stopDigestNull.Valid {
		r.StopProofDigest = domain.Digest(stopDigestNull.String)
	}
	if stopKindNull.Valid {
		r.StopProofKind = stopKindNull.String
	}
	if stopAtNull.Valid {
		// The proof timestamp is retained in the domain as part of its digest;
		// the SQL value is validated by the migration constraint.
	}
	if cleanupEvidenceNull.Valid {
		r.CleanupEvidenceDigest = domain.Digest(cleanupEvidenceNull.String)
	}
	var parseErr error
	if r.CreatedAt, parseErr = parseTime(created); parseErr != nil {
		return parseErr
	}
	if r.UpdatedAt, parseErr = parseTime(updated); parseErr != nil {
		return parseErr
	}
	if r.ExecutionID != executionID {
		return wrap(ErrConsistency, "sandbox resource execution mismatch", nil)
	}
	if err := r.Validate(); err != nil {
		return wrap(ErrConsistency, "stored sandbox resource is invalid", err)
	}
	*out = r
	return nil
}

func sandboxRunGuard(ctx context.Context, tx *immediateTx, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID, rejectCancel bool) error {
	var state, currentStage, attemptState string
	if err := tx.QueryRowContext(ctx, `SELECT state, current_stage FROM runs WHERE run_id=?`, string(runID)).Scan(&state, &currentStage); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "run does not exist", err)
		}
		return err
	}
	if currentStage != string(stage) {
		return wrap(ErrInvalidTransition, "sandbox stage is not current", nil)
	}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM stage_attempts WHERE run_id=? AND stage_name=? AND attempt_id=?`, string(runID), string(stage), string(attempt)).Scan(&attemptState); err != nil {
		return err
	}
	if state != string(domain.RunRunning) || attemptState != string(domain.StageAttemptRunning) {
		return wrap(ErrInvalidTransition, "sandbox work requires a RUNNING run attempt", nil)
	}
	if rejectCancel {
		return sandboxCancelGuard(ctx, tx, runID)
	}
	return nil
}
func sandboxRunCancelGuard(ctx context.Context, tx *immediateTx, executionID domain.SandboxExecutionID) error {
	var runID string
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM sandbox_executions WHERE sandbox_execution_id=?`, string(executionID)).Scan(&runID); err != nil {
		return err
	}
	return sandboxCancelGuard(ctx, tx, domain.RunID(runID))
}

func sandboxExecutionRunGuard(ctx context.Context, tx *immediateTx, executionID domain.SandboxExecutionID, rejectCancel bool) error {
	var runID string
	var stage domain.StageName
	var attempt domain.AttemptID
	if err := tx.QueryRowContext(ctx, `SELECT run_id, stage_name, attempt_id FROM sandbox_executions WHERE sandbox_execution_id=?`, string(executionID)).Scan(&runID, &stage, &attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "sandbox execution does not exist", err)
		}
		return err
	}
	return sandboxRunGuard(ctx, tx, domain.RunID(runID), stage, attempt, rejectCancel)
}
func sandboxCancelGuard(ctx context.Context, tx *immediateTx, runID domain.RunID) error {
	return ValidateNoPendingCancel(ctx, tx, runID)
}

func validateWatchdogArmed(c domain.WatchdogArmed) error {
	if err := c.ExecutionID.Validate(); err != nil {
		return err
	}
	if c.ExpectedVersion <= 0 {
		return errors.New("expected execution version must be positive")
	}
	if strings.TrimSpace(c.ControlRecordRef) == "" {
		return errors.New("control record reference is required")
	}
	if err := c.ControlFileDigest.Validate(); err != nil {
		return err
	}
	if err := c.TokenDigest.Validate(); err != nil {
		return err
	}
	if err := domain.ControlRequestID(c.IdempotencyKey).Validate(); err != nil {
		return err
	}
	return validateSandboxTime("command time", c.At)
}
func validatePreCreateACK(a domain.PreCreateACK) error {
	if err := a.ExecutionID.Validate(); err != nil {
		return err
	}
	if err := a.ResourceID.Validate(); err != nil {
		return err
	}
	if a.ResourceVersion <= 0 {
		return errors.New("resource ACK version must be positive")
	}
	if err := a.LabelsDigest.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.WatchdogRecordRef) == "" {
		return errors.New("watchdog record reference is required")
	}
	if err := domain.ControlRequestID(a.IdempotencyKey).Validate(); err != nil {
		return err
	}
	return validateSandboxTime("ACK time", a.At)
}
func validateSandboxTime(name string, at time.Time) error {
	if at.IsZero() || at.Location() != time.UTC {
		return fmt.Errorf("%s must be a canonical UTC timestamp", name)
	}
	return nil
}
func allowedSandboxResourceTransition(from, to domain.SandboxResourcePhase) bool {
	if from == to {
		return true
	}
	switch from {
	case domain.SandboxResourcePlanned:
		return to == domain.SandboxResourceCreating || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceCreating:
		return to == domain.SandboxResourceDispatching || to == domain.SandboxResourceUnknown || to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceDispatching:
		return to == domain.SandboxResourceSent || to == domain.SandboxResourceUnknown || to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceSent:
		return to == domain.SandboxResourceCompleted || to == domain.SandboxResourceUnknown || to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceUnknown:
		return to == domain.SandboxResourceCompleted || to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceStopped || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceCompleted:
		return to == domain.SandboxResourceStarted || to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceStopped || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceStarted:
		return to == domain.SandboxResourceCleanupPending || to == domain.SandboxResourceStopped || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceCleanupPending:
		return to == domain.SandboxResourceStopped || to == domain.SandboxResourceCleaned || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceStopped:
		return to == domain.SandboxResourceCleaned || to == domain.SandboxResourceInterrupted
	case domain.SandboxResourceInterrupted:
		return to == domain.SandboxResourceCleanupPending
	default:
		return false
	}
}
func resultPhaseMatchesCommand(r domain.SandboxResource, c domain.AdvanceResourceRequest) bool {
	return r.Phase == c.Phase && r.Version > c.ExpectedVersion && r.EngineResourceID == c.EngineResourceID
}
func nullableAttemptCall(v *domain.AttemptCallID) any {
	if v == nil {
		return nil
	}
	return string(*v)
}
func nullableDigest(v domain.Digest) any {
	if v == "" {
		return nil
	}
	return string(v)
}
func nullableDigestPtr(v *domain.Digest) any {
	if v == nil {
		return nil
	}
	return string(*v)
}
func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
