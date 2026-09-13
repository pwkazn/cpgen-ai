package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var _ port.MutationBudgetReader = (*Store)(nil)

// ReadMutationBudget reads all quota projections and durable claim counts in
// one SQLite statement. Both mutation kinds spend the same immutable stage
// allowance, including claims whose provider work later failed or was cancelled.
func (s *Store) ReadMutationBudget(ctx context.Context, runID domain.RunID, stage domain.StageName) (domain.MutationBudgetSnapshot, error) {
	result := domain.MutationBudgetSnapshot{RunID: runID, StageName: stage}
	if ctx == nil {
		return result, errors.New("mutation budget read requires a context")
	}
	if err := runID.Validate(); err != nil {
		return result, err
	}
	if err := stage.Validate(); err != nil {
		return result, err
	}
	var limit, claimed, version sql.NullInt64
	var claimCount, kindClaimed, badKinds int64
	err := s.db.QueryRowContext(ctx, `SELECT run.version,run.max_mutations_per_stage,account.limit_value,account.claimed_value,account.account_version,
		(SELECT count(*) FROM mutation_claims c WHERE c.run_id=run.run_id AND c.stage_name=stage.stage_name),
		(SELECT coalesce(sum(k.claimed_value),0) FROM mutation_accounts k WHERE k.run_id=run.run_id AND k.stage_name=stage.stage_name),
		(SELECT count(*) FROM mutation_accounts k WHERE k.run_id=run.run_id AND k.stage_name=stage.stage_name AND
		 (k.limit_value<>run.max_mutations_per_stage OR k.claimed_value>run.max_mutations_per_stage OR k.account_version<=0 OR
		  k.claimed_value<>(SELECT count(*) FROM mutation_claims c WHERE c.run_id=k.run_id AND c.stage_name=k.stage_name AND c.kind=k.kind)))
		FROM runs run JOIN stage_records stage ON stage.run_id=run.run_id
		LEFT JOIN mutation_stage_accounts account ON account.run_id=run.run_id AND account.stage_name=stage.stage_name
		WHERE run.run_id=? AND stage.stage_name=? AND stage.workflow_revision=run.workflow_revision AND stage.schema_version=run.schema_version`, runID, stage).Scan(&result.RunVersion, &result.Limit, &limit, &claimed, &version, &claimCount, &kindClaimed, &badKinds)
	if errors.Is(err, sql.ErrNoRows) {
		return result, wrap(ErrNotFound, "mutation budget run or compatible stage does not exist", err)
	}
	if err != nil {
		return result, err
	}
	if limit.Valid != claimed.Valid || limit.Valid != version.Valid || (limit.Valid && (limit.Int64 != result.Limit || version.Int64 <= 0)) {
		return result, wrap(ErrConsistency, "mutation stage quota identity differs", nil)
	}
	if limit.Valid {
		result.Claimed, result.AccountVersion = claimed.Int64, version.Int64
	}
	if err := result.Validate(); err != nil {
		return result, err
	}
	if claimCount != result.Claimed || kindClaimed != result.Claimed || badKinds != 0 {
		return result, wrap(ErrConsistency, "mutation quota projections differ from durable claims", nil)
	}
	return result, nil
}
