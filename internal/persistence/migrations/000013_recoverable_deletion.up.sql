-- Tombstones retain task/goal identity and all progress/session foreign keys.
-- A deletion batch restores only nodes deleted by that operation, leaving
-- separately deleted descendants in the recycle bin.
ALTER TABLE goals ADD COLUMN deleted_at DATETIME;
ALTER TABLE goals ADD COLUMN deletion_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN deleted_at DATETIME;
ALTER TABLE tasks ADD COLUMN deletion_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_goals_user_deleted ON goals(user_id, deleted_at, status);
CREATE INDEX idx_tasks_user_deleted ON tasks(user_id, deleted_at, goal_id);

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (13, 'recoverable_deletion', CURRENT_TIMESTAMP);
