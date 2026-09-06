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
	if s.config.migrationStartHook != nil {
		s.config.migrationStartHook()
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		applied, err := appliedMigrationsTx(ctx, tx)
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
			// M16 is immutable historical SQL. Its tightened physical-call
			// check rejected a legal M14 VOLUME row whose call identity was
			// intentionally NULL. Park only those legacy phase values before
			// M16 rebuilds the table; M17 restores them under the explicit
			// volume compatibility rule. This keeps M1-M16 bytes and hashes
			// unchanged while making an actual M14 -> latest upgrade safe.
			if item.version == 16 {
				if err := prepareM16VolumeCompatibility(ctx, tx); err != nil {
					return err
				}
			}
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
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func prepareM16VolumeCompatibility(ctx context.Context, tx *immediateTx) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sandbox_resources'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sandbox_m16_legacy_volume_phases(resource_id TEXT PRIMARY KEY, phase TEXT NOT NULL) STRICT`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sandbox_m16_legacy_volume_phases(resource_id, phase)
		SELECT resource_id, phase FROM sandbox_resources
		WHERE resource_kind='VOLUME' AND physical_call_id IS NULL
		  AND phase NOT IN ('PLANNED','CREATING','INTERRUPTED')`); err != nil {
		return err
	}
	var parked int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_m16_legacy_volume_phases`).Scan(&parked); err != nil {
		return err
	}
	if parked == 0 {
		return nil
	}
	for _, trigger := range []string{
		"sandbox_resource_phase_monotone",
		// M14/M15/M16 all install this guard.  The compatibility parking
		// update deliberately changes a legacy phase before M16 rebuilds the
		// table, so the version guard must be removed together with the phase
		// guards or the update is rejected before the rebuild starts.
		"sandbox_resource_version_guard",
		"sandbox_resource_stopped_requires_proof",
		"sandbox_resource_cleaned_requires_evidence",
	} {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE sandbox_resources SET phase='PLANNED'
		WHERE resource_id IN (SELECT resource_id FROM sandbox_m16_legacy_volume_phases)`)
	return err
}

type appliedMigration struct {
	version int
	name    string
	hash    string
}

func appliedMigrationsTx(ctx context.Context, tx *immediateTx) ([]appliedMigration, error) {
	var exists int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("inspect migration table: %w", err)
	}
	if exists == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT version, name, sha256 FROM schema_migrations ORDER BY version")
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

const sqliteTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(value time.Time) string { return value.Format(sqliteTimeFormat) }
