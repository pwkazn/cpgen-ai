package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"cpgen/internal/domain"
)

// BudgetSnapshot exposes a read-only projection for the immutable RunView.
// Physical account rows remain authoritative for metered dimensions. Package
// size and logical mutation allowance come from the immutable run columns in
// the same read; neither is represented by a physical budget account.
func (s *Store) BudgetSnapshot(ctx context.Context, runID domain.RunID) (domain.BudgetSnapshot, error) {
	if err := runID.Validate(); err != nil {
		return domain.BudgetSnapshot{}, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.BudgetSnapshot{}, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `
		SELECT account.request_snapshot_digest, account.dimension, account.limit_value, account.reserved_value, account.consumed_value, account.account_version,
		       run.max_package_bytes, run.max_mutations_per_stage, run.submitted_request_digest, run.max_llm_tokens, run.token_budget,
		       COALESCE(json_extract(run.submitted_request_json,'$.budget_limits.split_token_budget'),0)
		FROM budget_accounts account JOIN runs run ON run.run_id=account.run_id WHERE account.run_id = ? ORDER BY account.dimension`, string(runID))
	if err != nil {
		return domain.BudgetSnapshot{}, err
	}
	defer rows.Close()
	result := domain.BudgetSnapshot{Remaining: make(map[domain.BudgetDimension]int64)}
	var tokenRemaining int64
	tokenAccounts := 0
	for rows.Next() {
		var requestDigest, submittedDigest, raw string
		var limit, reserved, consumed, version int64
		if err := rows.Scan(&requestDigest, &raw, &limit, &reserved, &consumed, &version, &result.Limits.MaxPackageBytes, &result.Limits.MaxMutationsPerStage, &submittedDigest, &result.Limits.MaxLLMTokens, &result.Limits.TokenBudget, &result.Limits.SplitTokenBudget); err != nil {
			return domain.BudgetSnapshot{}, err
		}
		if requestDigest != submittedDigest {
			return domain.BudgetSnapshot{}, wrap(ErrConsistency, "budget account request binding differs from its run", nil)
		}
		dimension := domain.BudgetDimension(raw)
		account := domain.BudgetAccount{RunID: runID, RequestSnapshotDigest: domain.Digest(requestDigest), Dimension: dimension, Limit: limit, Reserved: reserved, Consumed: consumed, Version: version}
		if err := account.Validate(); err != nil {
			return domain.BudgetSnapshot{}, fmt.Errorf("budget account %q: %w", dimension, err)
		}
		result.Remaining[dimension] = account.Remaining()
		if result.Limits.UsesTokenBudget() && (dimension == domain.BudgetLLMInputTokens || dimension == domain.BudgetLLMOutputTokens) {
			if tokenAccounts == 0 {
				tokenRemaining = result.Limits.MaxLLMTokens
			}
			for _, amount := range []int64{reserved, consumed} {
				if amount > tokenRemaining {
					return domain.BudgetSnapshot{}, wrap(ErrConsistency, "shared token usage exceeds the run limit", nil)
				}
				tokenRemaining -= amount
			}
			tokenAccounts++
		}
		if version > result.Version {
			result.Version = version
		}
		switch dimension {
		case domain.BudgetLLMCalls:
			result.Limits.MaxLLMCalls = limit
		case domain.BudgetLLMInputTokens:
			result.Limits.MaxLLMInputTokens = limit
		case domain.BudgetLLMOutputTokens:
			result.Limits.MaxLLMOutputTokens = limit
		case domain.BudgetExternalCostMicroUSD:
			result.Limits.MaxLLMCostMicroUSD = limit
		case domain.BudgetSimilarityCalls:
			result.Limits.MaxSimilarityCalls = limit
		case domain.BudgetSimilarityCostMicroUSD:
			result.Limits.MaxSimilarityCostMicroUSD = limit
		case domain.BudgetDockerContainerCreates:
			result.Limits.MaxSandboxCreates = limit
		case domain.BudgetArtifactPhysicalNewBytes:
			result.Limits.MaxArtifactBytes = limit
		case domain.BudgetActiveTimeNS:
			result.Limits.MaxActiveTimeMilliseconds = limit / int64(time.Millisecond)
		}
	}
	if err := rows.Err(); err != nil {
		return domain.BudgetSnapshot{}, err
	}
	if result.Version == 0 {
		return domain.BudgetSnapshot{}, sql.ErrNoRows
	}
	if result.Limits.UsesTokenBudget() {
		if tokenAccounts != 2 {
			return domain.BudgetSnapshot{}, wrap(ErrConsistency, "shared token budget account is missing", nil)
		}
		result.Remaining[domain.BudgetLLMTokens] = tokenRemaining
	}
	if err := result.Validate(); err != nil {
		return domain.BudgetSnapshot{}, err
	}
	return result, nil
}

// CommittedArtifactReferences returns immutable references only; the blob
// reader remains a separate capability and is never exposed through RunView.
func (s *Store) CommittedArtifactReferences(ctx context.Context, runID domain.RunID) ([]domain.CommittedArtifactRef, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `
		SELECT occurrence_id, digest, size, role, logical_path
		FROM artifact_occurrences WHERE run_id = ? ORDER BY created_at, occurrence_id`, string(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.CommittedArtifactRef, 0)
	for rows.Next() {
		var occurrence, digest, role, path string
		var size int64
		if err := rows.Scan(&occurrence, &digest, &size, &role, &path); err != nil {
			return nil, err
		}
		item := domain.CommittedArtifactRef{OccurrenceID: domain.ArtifactOccurrenceID(occurrence), Blob: domain.BlobRef{Digest: domain.Digest(digest), Size: size}, Role: domain.ArtifactRole(role), LogicalPath: domain.SafeRelPath(path)}
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("artifact occurrence %q: %w", occurrence, err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// RunViewDocuments returns defensive copies of the canonical request and
// effective configuration documents bound to the run.
func (s *Store) RunViewDocuments(ctx context.Context, runID domain.RunID) ([]byte, []byte, error) {
	if err := runID.Validate(); err != nil {
		return nil, nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer connection.Close()
	var requestJSON, configJSON []byte
	if err := connection.QueryRowContext(ctx, `SELECT submitted_request_json, redacted_effective_config_json FROM runs WHERE run_id = ?`, string(runID)).Scan(&requestJSON, &configJSON); err != nil {
		return nil, nil, err
	}
	return append([]byte(nil), requestJSON...), append([]byte(nil), configJSON...), nil
}
