package sqlite

import (
	"context"
	"strings"

	"cpgen/internal/domain"
)

// Releasing stage artifacts must never discard the only recoverable receipt
// while its provider parent is unresolved. Check the closed private-response
// families before any writer, pin, budget or stage projection is changed.
func requireTerminalPrivateResponseParents(ctx context.Context, tx *immediateTx, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) error {
	rows, err := tx.QueryContext(ctx, `SELECT call_record_id FROM call_records WHERE run_id=? AND stage_name=? AND attempt_id=? AND provider='private-blob'
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
	for _, id := range ids {
		child, err := readCallRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		parentID, ok := privateResponseParent(child)
		if !ok {
			return wrap(ErrConsistency, "private response has no valid parent identity", nil)
		}
		parent, err := readCallRecord(ctx, tx, parentID)
		if err != nil {
			return err
		}
		if parent.RunID != runID || parent.StageName != stage || parent.AttemptID != attempt || parent.Kind != child.Kind || parent.Provider == "private-blob" {
			return wrap(ErrConsistency, "private response parent has a different attempt scope", nil)
		}
		if parent.State != domain.CallRecordTerminal {
			return wrap(ErrInvalidTransition, "private provider receipts require reconciliation before stage release", nil)
		}
	}
	return nil
}

// Only compiled response operations can settle local receipt slots. A prefix
// from another provider kind never grants access to its parent operation.
func privateResponseParent(call domain.CallRecord) (domain.CallRecordID, bool) {
	if call.Provider != "private-blob" {
		return "", false
	}
	var prefix string
	switch call.Kind {
	case domain.CallLLMGenerate:
		prefix = "llm-response:"
		if strings.HasPrefix(call.LogicalOperationID, "idea-batch-output:") {
			prefix = "idea-batch-output:"
		}
	case domain.CallSimilaritySearch:
		prefix = "similarity-response:"
	default:
		return "", false
	}
	raw, ok := strings.CutPrefix(call.LogicalOperationID, prefix)
	id := domain.CallRecordID(raw)
	return id, ok && id.Validate() == nil
}
