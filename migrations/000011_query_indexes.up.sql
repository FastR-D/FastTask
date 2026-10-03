-- Indexes for queries that run on every request or every sweep but had none.
--
-- Nothing here changes a table or a row. Every statement is IF NOT EXISTS, so the file is safe to
-- replay and an operator who added an index by hand is not broken by it.
--
--   agent_jobs(user_id, created_at)      the job queue page lists a user's jobs newest first and
--                                        polls it; the only index was the worker's queue index.
--   sessions(family_id)                  refresh-token reuse revokes a whole family.
--   idempotency_records(status_code,     the scheduler's minute purge of expired and orphaned
--                       expires_at)      records.
--   conversation_messages                messages of one conversation in order.
--     (conversation_id, created_at)

CREATE INDEX IF NOT EXISTS idx_agent_jobs_user_created ON agent_jobs(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sessions_family ON sessions(family_id);
CREATE INDEX IF NOT EXISTS idx_idempotency_expiry ON idempotency_records(status_code, expires_at);
CREATE INDEX IF NOT EXISTS idx_conversation_messages_conversation ON conversation_messages(conversation_id, created_at);

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (11, 'query_indexes', CURRENT_TIMESTAMP);
