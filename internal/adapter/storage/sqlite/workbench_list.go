package sqlite

import (
	"context"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func (s *Store) WorkbenchRecentRuns(ctx context.Context, filter domain.RunFilter) ([]port.WorkbenchRunSummary, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT run_id,state,version,current_stage,created_at,updated_at,COALESCE(json_extract(submitted_request_json,'$.brief'),'') FROM runs`
	args := []any{}
	if filter.State != nil {
		query += " WHERE state=?"
		args = append(args, string(*filter.State))
	}
	query += " ORDER BY updated_at DESC,run_id DESC LIMIT ?"
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []port.WorkbenchRunSummary{}
	for rows.Next() {
		var r port.WorkbenchRunSummary
		var created, updated string
		if err := rows.Scan(&r.RunID, &r.State, &r.Version, &r.CurrentStage, &created, &updated, &r.Brief); err != nil {
			return nil, err
		}
		r.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		r.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
