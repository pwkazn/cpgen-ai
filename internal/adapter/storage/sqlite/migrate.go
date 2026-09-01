package sqlite

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"

	"cpgen/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	hash    string
	sql     string
}

var migrationNamePattern = regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.sql$`)

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationNamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse migration version: %w", err)
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{
			version: version, name: entry.Name(), hash: string(domain.SumBytes(contents)), sql: string(contents),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for index, item := range migrations {
		if item.version != index+1 {
			return nil, wrap(ErrMigrationGap, fmt.Sprintf("compiled migration version %d follows %d", item.version, index), nil)
		}
	}
	return migrations, nil
}

func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}
	if len(applied) > len(migrations) {
		return wrap(ErrMigrationGap, "database contains an unknown future migration", nil)
	}
	for index, record := range applied {
		expectedVersion := index + 1
		if record.version != expectedVersion {
			return wrap(ErrMigrationGap, fmt.Sprintf("database is missing migration version %d", expectedVersion), nil)
		}
		expected := migrations[index]
		if record.name != expected.name || record.hash != expected.hash {
			return wrap(ErrMigrationDrift, fmt.Sprintf("migration %d name or hash changed", record.version), nil)
		}
	}
	for _, item := range migrations[len(applied):] {
		item := item
		if err := s.immediate(ctx, func(tx *immediateTx) error {
			if _, err := tx.ExecContext(ctx, item.sql); err != nil {
				return fmt.Errorf("execute migration %d: %w", item.version, err)
			}
			appliedAt := s.clock.Now().UTC()
			if appliedAt.IsZero() {
				return errors.New("migration clock returned a zero time")
			}
			_, err := tx.ExecContext(ctx,
				"INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)",
				item.version, item.name, item.hash, formatTime(appliedAt),
			)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

type appliedMigration struct {
	version int
	name    string
	hash    string
}

func (s *Store) appliedMigrations(ctx context.Context) ([]appliedMigration, error) {
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	var exists int
	if err := connection.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("inspect migration table: %w", err)
	}
	if exists == 0 {
		return nil, nil
	}
	rows, err := connection.QueryContext(ctx, "SELECT version, name, sha256 FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	var result []appliedMigration
	for rows.Next() {
		var record appliedMigration
		if err := rows.Scan(&record.version, &record.name, &record.hash); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return result, nil
}

func (s *Store) checkIntegrity(ctx context.Context) error {
	connection, err := s.connection(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if rows.Next() {
		_ = rows.Close()
		return errors.New("foreign key check reported a violation")
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("foreign key check rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close foreign key check: %w", err)
	}
	var integrity string
	if err := connection.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity check returned %q", integrity)
	}
	return nil
}

func formatTime(value time.Time) string { return value.Format(time.RFC3339Nano) }
