package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cpgen/internal/clock"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Config struct {
	Path        string
	BusyTimeout time.Duration
	MaxReaders  int

	// migrationStartHook is a package-private test seam used to line up real
	// simultaneous first opens before writer serialization begins.
	migrationStartHook func()
}

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code == e.Code
}

var (
	ErrStorageBusy       = &Error{Code: "storage_busy"}
	ErrMigrationDrift    = &Error{Code: "migration_drift"}
	ErrMigrationGap      = &Error{Code: "migration_gap"}
	ErrConsistency       = &Error{Code: "consistency_error"}
	ErrVersionConflict   = &Error{Code: "version_conflict"}
	ErrNotFound          = &Error{Code: "not_found"}
	ErrInvalidTransition = &Error{Code: "invalid_transition"}
	ErrCancelPending     = &Error{Code: "cancel_pending"}
	ErrReviewPending     = &Error{Code: "review_pending"}
)

type Store struct {
	db     *sql.DB
	config Config
	clock  clock.Clock
	closed atomic.Bool

	// endTransactionHook is an internal fault-injection seam for proving that
	// uncertain COMMIT/ROLLBACK connections are discarded. Normal callers leave
	// it nil.
	endTransactionHook func(operation string) error
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	return OpenWithClock(ctx, cfg, clock.Real{})
}

func OpenWithClock(ctx context.Context, cfg Config, source clock.Clock) (*Store, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("sqlite clock is nil")
	}
	absolute, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	if filepath.Clean(absolute) != filepath.Clean(cfg.Path) {
		return nil, errors.New("sqlite path must be absolute and clean")
	}
	milliseconds := cfg.BusyTimeout.Milliseconds()
	query := make(url.Values)
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(milliseconds, 10)+")")
	dsn := "file:" + filepath.ToSlash(absolute) + "?" + query.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxReaders + 1)
	db.SetMaxIdleConns(cfg.MaxReaders + 1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db, config: cfg, clock: source}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Path) == "" || !filepath.IsAbs(cfg.Path) {
		return errors.New("sqlite path must be absolute")
	}
	if cfg.BusyTimeout <= 0 || cfg.BusyTimeout < time.Millisecond {
		return errors.New("sqlite busy timeout must be at least one millisecond")
	}
	if cfg.BusyTimeout.Milliseconds() > int64(^uint32(0)>>1) {
		return errors.New("sqlite busy timeout is too large")
	}
	if cfg.MaxReaders <= 0 {
		return errors.New("sqlite max readers must be positive")
	}
	return nil
}

func (s *Store) initialize(ctx context.Context) error {
	connection, err := s.connection(ctx)
	if err != nil {
		return err
	}
	if err := connection.Close(); err != nil {
		return fmt.Errorf("release initialized sqlite connection: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	return s.checkIntegrity(ctx)
}

func (s *Store) connection(ctx context.Context) (*sql.Conn, error) {
	if s == nil || s.db == nil || s.closed.Load() {
		return nil, errors.New("sqlite store is closed")
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire sqlite connection: %w", err)
	}
	milliseconds := s.config.BusyTimeout.Milliseconds()
	pragmas := []string{
		"PRAGMA foreign_keys = 1",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = " + strconv.FormatInt(milliseconds, 10),
	}
	for _, pragma := range pragmas {
		if _, err := connection.ExecContext(ctx, pragma); err != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("apply sqlite connection pragma: %w", err)
		}
	}
	return connection, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close sqlite: %w", err)
	}
	return nil
}

func wrap(code *Error, message string, cause error) error {
	return &Error{Code: code.Code, Message: message, Cause: cause}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}
