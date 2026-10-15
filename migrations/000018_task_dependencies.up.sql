-- Dependencies are explicit task IDs, separate from the parent/child tree.
-- Existing user tasks retain their original content, status and revisions.
ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]';
INSERT INTO schema_migrations(version,name,applied_at) VALUES (18,'task_dependencies',CURRENT_TIMESTAMP);
