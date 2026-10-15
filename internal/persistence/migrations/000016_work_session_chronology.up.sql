ALTER TABLE work_sessions ADD COLUMN last_transition_at DATETIME;
-- Old running sessions do not retain resume timestamps. Backfill only known
-- business events, never updated_at (which may be the time of a later backfill).
UPDATE work_sessions SET last_transition_at = COALESCE(ended_at, paused_at, started_at);
INSERT INTO schema_migrations(version,name,applied_at) VALUES (16,'work_session_chronology',CURRENT_TIMESTAMP);
