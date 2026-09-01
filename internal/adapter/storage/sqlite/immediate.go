package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

type immediateTx struct {
	connection *sql.Conn
}

func (tx *immediateTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.connection.ExecContext(ctx, query, args...)
}

func (tx *immediateTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.connection.QueryContext(ctx, query, args...)
}

func (tx *immediateTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.connection.QueryRowContext(ctx, query, args...)
}

func (s *Store) immediate(ctx context.Context, fn func(*immediateTx) error) error {
	if fn == nil {
		return errors.New("immediate transaction callback is nil")
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return err
	}
	releaseNormally := true
	defer func() {
		if releaseNormally {
			_ = connection.Close()
		}
	}()
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if isSQLiteBusy(err) || errors.Is(err, context.DeadlineExceeded) {
			return wrap(ErrStorageBusy, "begin immediate transaction", err)
		}
		return fmt.Errorf("begin immediate transaction: %w", err)
	}
	tx := &immediateTx{connection: connection}
	callbackErr := fn(tx)
	operation := "commit"
	statement := "COMMIT"
	if callbackErr != nil {
		operation = "rollback"
		statement = "ROLLBACK"
	}
	if s.endTransactionHook != nil {
		if hookErr := s.endTransactionHook(operation); hookErr != nil {
			releaseNormally = false
			discardConnection(connection)
			return errors.Join(callbackErr, hookErr)
		}
	}
	if _, endErr := connection.ExecContext(ctx, statement); endErr != nil {
		releaseNormally = false
		discardConnection(connection)
		return errors.Join(callbackErr, fmt.Errorf("%s immediate transaction: %w", operation, endErr))
	}
	return callbackErr
}

func discardConnection(connection *sql.Conn) {
	_ = connection.Raw(func(any) error { return driver.ErrBadConn })
	_ = connection.Close()
}
