ALTER TABLE review_decisions ADD COLUMN revision_target_stage TEXT;

CREATE TRIGGER review_revision_target_insert_guard
BEFORE INSERT ON review_decisions
WHEN (NEW.kind = 'REVISE' AND NEW.revision_target_stage IS NULL) OR
     (NEW.kind <> 'REVISE' AND NEW.revision_target_stage IS NOT NULL) OR
     (NEW.revision_target_stage IS NOT NULL AND NOT EXISTS (
         SELECT 1 FROM stage_records target
         JOIN stage_records current ON current.run_id = target.run_id
         WHERE target.run_id = NEW.run_id
           AND target.stage_name = NEW.revision_target_stage
           AND current.stage_name = NEW.stage_name
           AND target.ordinal <= current.ordinal
     ))
BEGIN
    SELECT RAISE(ABORT, 'review revision target must be a current or upstream producer stage');
END;
