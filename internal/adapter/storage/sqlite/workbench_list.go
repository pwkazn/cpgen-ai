package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type workbenchRunCursor struct {
	Version   string       `json:"version"`
	Filter    string       `json:"filter"`
	UpdatedAt string       `json:"updated_at"`
	RunID     domain.RunID `json:"run_id"`
}

type workbenchRunRow struct {
	port.WorkbenchRunSummary
	updatedAt string
}

// WorkbenchRuns applies the filter before selecting a bounded page. Compound
// filters read each state through its ordered index in one snapshot, then merge
// the bounded results. This avoids scanning unrelated runs or sorting the full
// matching history for a state IN (...) query.
func (s *Store) WorkbenchRuns(ctx context.Context, q port.WorkbenchRunQuery) (port.WorkbenchRunPage, error) {
	result := port.WorkbenchRunPage{Runs: []port.WorkbenchRunSummary{}}
	if err := q.Validate(); err != nil {
		return result, err
	}
	states, _ := q.States()
	stateNames := make([]string, len(states))
	for i, state := range states {
		stateNames[i] = string(state)
	}
	filter := strings.Join(stateNames, ",")
	cursor, err := decodeWorkbenchRunCursor(q.Cursor, filter)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if len(stateNames) == 0 {
		stateNames = []string{""}
	}
	var merged []workbenchRunRow
	for _, state := range stateNames {
		statement, args := workbenchRunPageSQL(state, cursor, q.Limit+1)
		rows, err := readWorkbenchRunPage(ctx, tx, statement, args)
		if err != nil {
			return result, err
		}
		merged = append(merged, rows...)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	// Compare the original TEXT keys, exactly as SQLite does. Reformatting an
	// API timestamp would lose precision and change the next page's boundary.
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].updatedAt != merged[j].updatedAt {
			return merged[i].updatedAt > merged[j].updatedAt
		}
		return merged[i].RunID > merged[j].RunID
	})
	if len(merged) > q.Limit {
		merged = merged[:q.Limit]
		last := merged[len(merged)-1]
		raw, err := json.Marshal(workbenchRunCursor{Version: "1", Filter: filter, UpdatedAt: last.updatedAt, RunID: last.RunID})
		if err != nil {
			return result, err
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, row := range merged {
		result.Runs = append(result.Runs, row.WorkbenchRunSummary)
	}
	return result, nil
}

func decodeWorkbenchRunCursor(token, filter string) (*workbenchRunCursor, error) {
	if token == "" {
		return nil, nil
	}
	invalid := fmt.Errorf("%w: malformed or mismatched cursor", port.ErrInvalidWorkbenchRunQuery)
	if len(token) > 512 {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != token {
		return nil, invalid
	}
	var cursor workbenchRunCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, invalid
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(raw, canonical) || cursor.Version != "1" || cursor.Filter != filter || cursor.RunID.Validate() != nil {
		return nil, invalid
	}
	updated, err := parseTime(cursor.UpdatedAt)
	if err != nil || updated.IsZero() || formatTime(updated) != cursor.UpdatedAt {
		return nil, invalid
	}
	return &cursor, nil
}

func workbenchRunPageSQL(state string, cursor *workbenchRunCursor, limit int) (string, []any) {
	query := `SELECT run_id,state,version,current_stage,created_at,updated_at,COALESCE(json_extract(submitted_request_json,'$.brief'),'') FROM runs`
	var where []string
	var args []any
	if state != "" {
		where = append(where, "state=?")
		args = append(args, state)
	}
	if cursor != nil {
		where = append(where, "(updated_at,run_id) < (?,?)")
		args = append(args, cursor.UpdatedAt, string(cursor.RunID))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at DESC,run_id DESC LIMIT ?"
	return query, append(args, limit)
}

func readWorkbenchRunPage(ctx context.Context, tx *sql.Tx, query string, args []any) ([]workbenchRunRow, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []workbenchRunRow
	for rows.Next() {
		var row workbenchRunRow
		var created string
		if err := rows.Scan(&row.RunID, &row.State, &row.Version, &row.CurrentStage, &created, &row.updatedAt, &row.Brief); err != nil {
			return nil, err
		}
		row.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		row.UpdatedAt, err = parseTime(row.updatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// WorkbenchRecentRuns retains the earlier single-state entry point for callers
// that do not paginate. Its zero-limit behavior remains an empty list.
func (s *Store) WorkbenchRecentRuns(ctx context.Context, filter domain.RunFilter) ([]port.WorkbenchRunSummary, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if filter.Limit == 0 {
		return []port.WorkbenchRunSummary{}, nil
	}
	query := port.WorkbenchRunQuery{Limit: filter.Limit}
	if filter.State != nil {
		query.State = string(*filter.State)
	}
	page, err := s.WorkbenchRuns(ctx, query)
	return page.Runs, err
}
