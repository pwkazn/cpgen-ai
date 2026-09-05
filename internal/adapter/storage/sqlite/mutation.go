package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cpgen/internal/domain"
)

var _ interface {
	ClaimMutation(context.Context, domain.MutationClaimRequest) (domain.MutationGrant, error)
	RecordMutation(context.Context, domain.MutationRecordRequest) error
} = (*Store)(nil)

func (s *Store) ClaimMutation(ctx context.Context, request domain.MutationClaimRequest) (domain.MutationGrant, error) {
	if err := request.Validate(); err != nil {
		return domain.MutationGrant{}, err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return domain.MutationGrant{}, err
	}
	var result domain.MutationGrant
	err = s.immediate(ctx, func(tx *immediateTx) error {
		var claimID, storedCommand string
		intentErr := tx.QueryRowContext(ctx, `SELECT claim_id, command_digest FROM mutation_intents WHERE intent_digest = ? AND run_id = ?`, request.IntentDigest, request.RunID).Scan(&claimID, &storedCommand)
		if intentErr == nil {
			if storedCommand != string(commandDigest) {
				return wrap(ErrConsistency, "mutation intent was reused with different content", nil)
			}
			result, err = readMutationGrant(ctx, tx, claimID)
			return err
		}
		if !errors.Is(intentErr, sql.ErrNoRows) {
			return intentErr
		}
		var runLimit int64
		if err := tx.QueryRowContext(ctx, `SELECT max_mutations_per_stage FROM runs WHERE run_id = ?`, request.RunID).Scan(&runLimit); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "mutation run does not exist", err)
		} else if err != nil {
			return err
		}
		effectiveLimit := request.LimitSnapshot
		if effectiveLimit == 0 {
			effectiveLimit = request.Limit
		}
		if effectiveLimit != runLimit {
			return wrap(ErrConsistency, "mutation claim limit is not the immutable run limit", nil)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_accounts(run_id, stage_name, kind, limit_value, claimed_value, account_version)
			VALUES (?, ?, ?, ?, 0, 1) ON CONFLICT(run_id, stage_name, kind) DO NOTHING`, request.RunID, request.StageName, request.Kind, runLimit); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_stage_accounts(run_id, stage_name, limit_value, claimed_value, account_version)
			VALUES (?, ?, ?, 0, 1) ON CONFLICT(run_id, stage_name) DO NOTHING`, request.RunID, request.StageName, runLimit); err != nil {
			return err
		}
		// The stage account is the authoritative quota shared by CONTENT and
		// METADATA. The explicit max-int guard keeps the increment integer-only
		// even when a caller supplies the largest representable SQLite integer.
		stageUpdate, err := tx.ExecContext(ctx, `UPDATE mutation_stage_accounts SET claimed_value = claimed_value + 1, account_version = account_version + 1
			WHERE run_id = ? AND stage_name = ? AND claimed_value < limit_value AND claimed_value < 9223372036854775807`, request.RunID, request.StageName)
		if err != nil {
			return err
		}
		stageCount, err := stageUpdate.RowsAffected()
		if err != nil {
			return err
		}
		if stageCount != 1 {
			return wrap(ErrConsistency, "mutation budget exhausted", nil)
		}
		resultUpdate, err := tx.ExecContext(ctx, `UPDATE mutation_accounts SET claimed_value = claimed_value + 1, account_version = account_version + 1
			WHERE run_id = ? AND stage_name = ? AND kind = ? AND claimed_value < limit_value AND claimed_value < 9223372036854775807`, request.RunID, request.StageName, request.Kind)
		if err != nil {
			return err
		}
		count, err := resultUpdate.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return wrap(ErrConsistency, "mutation budget exhausted", nil)
		}
		generated, err := domain.NewID("mutation_claim")
		if err != nil {
			return err
		}
		result = domain.MutationGrant{ClaimID: generated, RunID: request.RunID, StageName: request.StageName, ScopeDigest: request.ScopeDigest, SourceBatchDigest: request.SourceBatchDigest, Ordinal: request.Ordinal, LimitSnapshot: effectiveLimit, Kind: request.Kind, IntentDigest: request.IntentDigest}
		result.GrantDigest, err = mutationGrantDigest(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_claims(claim_id, run_id, stage_name, scope_digest, source_batch_digest, ordinal, limit_snapshot, kind, intent_digest, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, result.ClaimID, request.RunID, request.StageName, request.ScopeDigest, request.SourceBatchDigest, request.Ordinal, effectiveLimit, request.Kind, request.IntentDigest, formatTime(request.At)); err != nil {
			return fmt.Errorf("insert mutation claim: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_intents(intent_digest, claim_id, run_id, command_digest, created_at) VALUES (?, ?, ?, ?, ?)`, request.IntentDigest, result.ClaimID, request.RunID, commandDigest, formatTime(request.At)); err != nil {
			return fmt.Errorf("insert mutation intent: %w", err)
		}
		return result.Validate()
	})
	return result, err
}

func (s *Store) RecordMutation(ctx context.Context, request domain.MutationRecordRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	commandDigest, _, err := digestJSON(request)
	if err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var runID, stageName, scopeDigest, sourceBatchDigest, kind, intentDigest string
		var ordinal int64
		if err := tx.QueryRowContext(ctx, `SELECT run_id, stage_name, scope_digest, source_batch_digest, ordinal, kind, intent_digest FROM mutation_claims WHERE claim_id = ?`, request.Grant.ClaimID).Scan(&runID, &stageName, &scopeDigest, &sourceBatchDigest, &ordinal, &kind, &intentDigest); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "mutation claim does not exist", err)
		} else if err != nil {
			return err
		}
		if runID != string(request.Grant.RunID) || stageName != string(request.Grant.StageName) || scopeDigest != string(request.Grant.ScopeDigest) || sourceBatchDigest != string(request.Grant.SourceBatchDigest) || ordinal != request.Grant.Ordinal || kind != string(request.Grant.Kind) || intentDigest != string(request.Grant.IntentDigest) {
			return wrap(ErrConsistency, "mutation grant does not match immutable claim", nil)
		}
		grantDigest, err := mutationGrantDigest(request.Grant)
		if err != nil {
			return err
		}
		if grantDigest != request.Grant.GrantDigest {
			return wrap(ErrConsistency, "mutation grant digest is invalid", nil)
		}
		var storedDigest string
		recordErr := tx.QueryRowContext(ctx, `SELECT command_digest FROM mutation_records WHERE claim_id = ?`, request.Grant.ClaimID).Scan(&storedDigest)
		if recordErr == nil {
			if storedDigest != string(commandDigest) {
				return wrap(ErrConsistency, "mutation record replay drifted", nil)
			}
			return nil
		}
		if !errors.Is(recordErr, sql.ErrNoRows) {
			return recordErr
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_records(record_id, claim_id, run_id, stage_name, scope_digest, source_batch_digest, ordinal, kind, intent_digest, command_digest, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, request.RecordID, request.Grant.ClaimID, request.Grant.RunID, request.Grant.StageName, request.Grant.ScopeDigest, request.Grant.SourceBatchDigest, request.Grant.Ordinal, request.Grant.Kind, request.Grant.IntentDigest, commandDigest, formatTime(request.At)); err != nil {
			return fmt.Errorf("insert mutation record: %w", err)
		}
		for index, item := range request.Operations {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_record_operations(record_id, operation_ordinal, run_id, stage_name, call_record_id, attempt_id) VALUES (?, ?, ?, ?, ?, ?)`, request.RecordID, index+1, request.Grant.RunID, request.Grant.StageName, item.CallRecordID, item.AttemptID); err != nil {
				return fmt.Errorf("insert mutation operation: %w", err)
			}
		}
		for index, item := range request.Reservations {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_record_reservations(record_id, reservation_ordinal, run_id, stage_name, reservation_id, call_record_id, attempt_call_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, request.RecordID, index+1, request.Grant.RunID, request.Grant.StageName, item.ReservationID, item.CallRecordID, item.AttemptCallID); err != nil {
				return fmt.Errorf("insert mutation reservation: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_record_output_occurrences(record_id, run_id, stage_name, occurrence_id) VALUES (?, ?, ?, ?)`, request.RecordID, request.Grant.RunID, request.Grant.StageName, request.OutputOccurrenceID); err != nil {
			return fmt.Errorf("insert mutation output: %w", err)
		}
		return nil
	})
}

func mutationGrantDigest(grant domain.MutationGrant) (domain.Digest, error) {
	grant.GrantDigest = ""
	digest, _, err := digestJSON(grant)
	return digest, err
}

func readMutationGrant(ctx context.Context, queryer rowQuerier, claimID string) (domain.MutationGrant, error) {
	var result domain.MutationGrant
	var runID, stageName, kind string
	if err := queryer.QueryRowContext(ctx, `SELECT claim_id, run_id, stage_name, scope_digest, source_batch_digest, ordinal, limit_snapshot, kind, intent_digest FROM mutation_claims WHERE claim_id = ?`, claimID).Scan(&result.ClaimID, &runID, &stageName, &result.ScopeDigest, &result.SourceBatchDigest, &result.Ordinal, &result.LimitSnapshot, &kind, &result.IntentDigest); err != nil {
		return result, err
	}
	result.RunID, result.StageName, result.Kind = domain.RunID(runID), domain.StageName(stageName), domain.MutationKind(kind)
	var err error
	result.GrantDigest, err = mutationGrantDigest(result)
	if err != nil {
		return result, err
	}
	return result, result.Validate()
}

func isMutationConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}
