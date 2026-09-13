package application_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cpgen/internal/cli"
	"cpgen/internal/domain"
	sqlitedriver "modernc.org/sqlite"
)

// The crash hook exists only in this test binary. Production CLI/storage has
// no environment-controlled failure hook or alternative verification path.
func TestMVPCLICommitCrashHelper(t *testing.T) {
	configPath := os.Getenv("CPGEN_TEST_PACKAGE_CRASH_CONFIG")
	if configPath == "" {
		t.Skip("subprocess helper")
	}
	marker := os.Getenv("CPGEN_TEST_PACKAGE_CRASH_MARKER")
	err := sqlitedriver.RegisterScalarFunction("cpgen_test_package_crash", 0, func(*sqlitedriver.FunctionContext, []driver.Value) (driver.Value, error) {
		if err := os.WriteFile(marker, []byte("inside verified package transaction"), 0o600); err != nil {
			return nil, err
		}
		os.Exit(97)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	code := cli.Run([]string{"--config", configPath, "run", "resume", os.Getenv("CPGEN_TEST_PACKAGE_CRASH_RUN")}, os.Stdout, os.Stderr)
	os.Exit(code)
}

func packageCrashSQL(t *testing.T, databasePath, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func pausePublicPackageCommit(t *testing.T, databasePath string) {
	t.Helper()
	packageCrashSQL(t, databasePath, `CREATE TRIGGER cpgen_test_package_pause BEFORE INSERT ON verified_packages BEGIN SELECT RAISE(ABORT,'test pause before package commit'); END`)
}

// The first unmodified CLI run has already completed every Docker operation
// and published its archive before the test-only transaction pause. This
// helper resumes only the package boundary, then exits during its SQLite write.
func crashPublicPackageCommit(t *testing.T, ctx context.Context, configPath, databasePath string, runID domain.RunID) {
	t.Helper()
	packageCrashSQL(t, databasePath, `DROP TRIGGER cpgen_test_package_pause; CREATE TRIGGER cpgen_test_package_crash BEFORE INSERT ON verified_packages BEGIN SELECT cpgen_test_package_crash(); END`)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "package-crash-marker")
	command := exec.CommandContext(ctx, executable, "-test.run=^TestMVPCLICommitCrashHelper$")
	command.Env = append(os.Environ(), "CPGEN_TEST_PACKAGE_CRASH_CONFIG="+configPath, "CPGEN_TEST_PACKAGE_CRASH_MARKER="+marker, "CPGEN_TEST_PACKAGE_CRASH_RUN="+string(runID))
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 97 {
		t.Fatalf("package crash helper: %v %s", err, output)
	}
	observed, err := os.ReadFile(marker)
	if err != nil || string(observed) != "inside verified package transaction" {
		t.Fatalf("package crash did not reach transaction: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state string
	var final sql.NullString
	var packages, occurrences, attempts int
	err = db.QueryRow(`SELECT r.state,r.final_package_occurrence_id,
		(SELECT count(*) FROM verified_packages WHERE run_id=r.run_id),
		(SELECT count(*) FROM artifact_occurrences WHERE run_id=r.run_id AND stage_name='package'),
		(SELECT attempt_count FROM stage_records WHERE run_id=r.run_id AND stage_name='package')
		FROM runs r WHERE run_id=?`, runID).Scan(&state, &final, &packages, &occurrences, &attempts)
	if err != nil || state != "RUNNING" || final.Valid || packages != 0 || occurrences != 0 || attempts != 1 {
		t.Fatalf("partial crashed package state: %s %v %d %d %d %v", state, final, packages, occurrences, attempts, err)
	}
	if _, err := db.Exec(`DROP TRIGGER cpgen_test_package_crash`); err != nil {
		t.Fatal(err)
	}
}
