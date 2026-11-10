-- Calendar dates describe a long-term plan, independently of daily commitments.
-- NULL preserves existing tasks as unscheduled; derived parent ranges are not stored.
ALTER TABLE tasks ADD COLUMN planned_start TEXT;
ALTER TABLE tasks ADD COLUMN planned_end TEXT;
INSERT INTO schema_migrations(version,name,applied_at) VALUES(24,'task_planned_dates',CURRENT_TIMESTAMP);
