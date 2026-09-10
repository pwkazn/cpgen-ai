package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
)

// ReadLogicalCall also supports OPEN calls, which have no prepared bundle.
func (s *Store) ReadLogicalCall(ctx context.Context, id domain.CallRecordID) (domain.CallRecord, error) {
	if err := id.Validate(); err != nil {
		return domain.CallRecord{}, err
	}
	call, err := readCallRecord(ctx, s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return call, wrap(ErrNotFound, "logical call does not exist", err)
	}
	return call, err
}

// OpenReplayableCall commits the exact opening command with its logical call.
// The ordinary OpenCall API retains its historical behavior for callers that
// already persist their own command context.
func (s *Store) OpenReplayableCall(ctx context.Context, request domain.OpenCallRequest) (domain.CallRecord, error) {
	return s.openCall(ctx, request, true)
}

func retainCallOpen(ctx context.Context, tx *immediateTx, request domain.OpenCallRequest, digest domain.Digest, raw []byte) error {
	if len(raw) > 16384 {
		return errors.New("original call open metadata exceeds bound")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO call_open_requests(call_record_id,request_json,request_digest) VALUES(?,?,?) ON CONFLICT(call_record_id) DO NOTHING`, request.ID, raw, digest)
	return err
}

// ReadOpenCall is read-only. It does not reopen a call or grant dispatch.
func (s *Store) ReadOpenCall(ctx context.Context, id domain.CallRecordID) (domain.OpenCallRequest, error) {
	var request domain.OpenCallRequest
	if err := id.Validate(); err != nil {
		return request, err
	}
	var raw []byte
	var digest, bound domain.Digest
	err := s.db.QueryRowContext(ctx, `SELECT receipt.request_json,receipt.request_digest,call.open_command_digest
		FROM call_open_requests receipt JOIN call_records call ON call.call_record_id=receipt.call_record_id
		WHERE receipt.call_record_id=?`, id).Scan(&raw, &digest, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return request, wrap(ErrNotFound, "original call open metadata is unavailable", err)
	}
	if err != nil {
		return request, err
	}
	if len(raw) > 16384 || domain.SumBytes(raw) != digest || digest != bound {
		return request, wrap(ErrConsistency, "original call open digest differs", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return domain.OpenCallRequest{}, wrap(ErrConsistency, "original call open metadata is invalid", nil)
	}
	canonicalDigest, _, err := digestJSON(request)
	if err != nil || request.ID != id || canonicalDigest != digest || request.Validate() != nil {
		return domain.OpenCallRequest{}, wrap(ErrConsistency, "original call open binding is invalid", nil)
	}
	return request, nil
}

// FinishReplayableCall retains completion metadata atomically with settlement.
func (s *Store) FinishReplayableCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	return s.finishCall(ctx, request, true)
}

func retainCallFinish(ctx context.Context, tx *immediateTx, request domain.FinishCallRequest, digest domain.Digest, raw []byte) error {
	if len(raw) > 16384 {
		return errors.New("original call finish metadata exceeds bound")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO call_finish_requests(call_record_id,request_json,request_digest) VALUES(?,?,?) ON CONFLICT(call_record_id) DO NOTHING`, request.CallRecordID, raw, digest)
	return err
}

// ReadFinishCall reads the exact completion command, never a new settlement.
func (s *Store) ReadFinishCall(ctx context.Context, id domain.CallRecordID) (domain.FinishCallRequest, error) {
	var request domain.FinishCallRequest
	if err := id.Validate(); err != nil {
		return request, err
	}
	var raw []byte
	var digest, bound domain.Digest
	err := s.db.QueryRowContext(ctx, `SELECT receipt.request_json,receipt.request_digest,call.finish_command_digest
		FROM call_finish_requests receipt JOIN call_records call ON call.call_record_id=receipt.call_record_id
		WHERE receipt.call_record_id=? AND call.state='TERMINAL'`, id).Scan(&raw, &digest, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return request, wrap(ErrNotFound, "original call finish metadata is unavailable", err)
	}
	if err != nil {
		return request, err
	}
	if len(raw) > 16384 || domain.SumBytes(raw) != digest || digest != bound {
		return request, wrap(ErrConsistency, "original call finish digest differs", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return domain.FinishCallRequest{}, wrap(ErrConsistency, "original call finish metadata is invalid", nil)
	}
	canonicalDigest, _, err := digestJSON(request)
	if err != nil || request.CallRecordID != id || canonicalDigest != digest || request.Validate() != nil {
		return domain.FinishCallRequest{}, wrap(ErrConsistency, "original call finish binding is invalid", nil)
	}
	return request, nil
}
