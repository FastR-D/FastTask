-- A planning preference and stable daily commitment identity. Historical
-- revisions of the same task on one day share the first item's logical id;
-- work sessions retain their original item foreign key and can follow it.
ALTER TABLE daily_plan_items ADD COLUMN logical_id TEXT NOT NULL DEFAULT '';
UPDATE daily_plan_items
SET logical_id = COALESCE((
    SELECT original.id FROM daily_plan_items AS original
    WHERE original.plan_id = daily_plan_items.plan_id
      AND original.task_id = daily_plan_items.task_id
      AND original.task_id IS NOT NULL
    ORDER BY original.plan_revision, original.created_at, original.id LIMIT 1
), id);
CREATE INDEX idx_plan_items_logical ON daily_plan_items(logical_id, plan_revision);

ALTER TABLE daily_plans ADD COLUMN frozen_at DATETIME;
UPDATE daily_plans SET frozen_at = created_at;

CREATE TABLE user_planning_settings (
    user_id TEXT PRIMARY KEY REFERENCES users(id),
    primary_goal_id TEXT REFERENCES goals(id),
    daily_budget_minutes INTEGER NOT NULL DEFAULT 120 CHECK(daily_budget_minutes BETWEEN 30 AND 480),
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (12, 'daily_planning', CURRENT_TIMESTAMP);
