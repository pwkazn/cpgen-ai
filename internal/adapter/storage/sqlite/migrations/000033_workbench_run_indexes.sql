CREATE INDEX runs_updated_at_run_id ON runs(updated_at DESC, run_id DESC);
CREATE INDEX runs_state_updated_at_run_id ON runs(state, updated_at DESC, run_id DESC);
