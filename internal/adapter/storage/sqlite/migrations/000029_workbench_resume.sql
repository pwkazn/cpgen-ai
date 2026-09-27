-- An admission receipt is durable even when the process exits before executing.
-- Such work is never replayed automatically; the user submits a new operation.
CREATE TABLE workbench_resumes (
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    operation_key TEXT NOT NULL,
    expected_run_version INTEGER NOT NULL CHECK (expected_run_version > 0),
    PRIMARY KEY (run_id, operation_key)
) STRICT;
