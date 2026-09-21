 PRAGMA foreign_keys = ON;

 -- Agent runtime (doc/agent-impl.md §3). Adds four tables only; existing
 -- conversations/tasks/daily_plans structures are untouched (§3.2). The
 -- conversations table is reused as the Thread (§3.1): conversations.id is the
 -- protocol threadId. conversation_messages is retained for pre-migration
 -- history but is not written by the agent runtime.

 CREATE TABLE IF NOT EXISTS agent_runs (
     id TEXT PRIMARY KEY,
     user_id TEXT NOT NULL REFERENCES users(id),
     thread_id TEXT NOT NULL REFERENCES conversations(id),
     -- job_id mirrors proposals.job_id: a plain optional reference to the
     -- AgentJob currently carrying the run, with no FK. A run outlives any
     -- single job (an approval receipt starts a new job against the same run,
     -- agent-impl.md §4.0), and the empty string means "no job attached yet".
     job_id TEXT NOT NULL DEFAULT '',
     status TEXT NOT NULL DEFAULT 'queued',
     state_json TEXT NOT NULL DEFAULT '{}',
     checkpoint_seq INTEGER NOT NULL DEFAULT 0,
     parent_message_id TEXT REFERENCES agent_messages(id),
     error_code TEXT NOT NULL DEFAULT '',
     error_message TEXT NOT NULL DEFAULT '',
     revision INTEGER NOT NULL DEFAULT 1,
     created_at DATETIME NOT NULL,
     updated_at DATETIME NOT NULL,
     finished_at DATETIME
 );

 -- At most one queued/running run per thread, enforced at the database level as
 -- defence-in-depth for the conditional-write rule in agent-impl.md §3.1.
 -- awaiting_approval is intentionally excluded: the run holds no lease while it
 -- waits for a user decision (§4.0).
 CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_runs_active_thread
     ON agent_runs(thread_id) WHERE status IN ('queued', 'running');
 CREATE INDEX IF NOT EXISTS idx_agent_runs_user
     ON agent_runs(user_id, created_at DESC);

 CREATE TABLE IF NOT EXISTS agent_messages (
     id TEXT PRIMARY KEY,
     user_id TEXT NOT NULL REFERENCES users(id),
     thread_id TEXT NOT NULL REFERENCES conversations(id),
     run_id TEXT NOT NULL REFERENCES agent_runs(id),
     parent_id TEXT REFERENCES agent_messages(id),
     role TEXT NOT NULL,
     seq INTEGER NOT NULL,
     created_at DATETIME NOT NULL
 );

 CREATE INDEX IF NOT EXISTS idx_agent_messages_thread_seq
     ON agent_messages(thread_id, seq);
 CREATE INDEX IF NOT EXISTS idx_agent_messages_run
     ON agent_messages(run_id, seq);

 CREATE TABLE IF NOT EXISTS agent_message_parts (
     id TEXT PRIMARY KEY,
     user_id TEXT NOT NULL REFERENCES users(id),
     message_id TEXT NOT NULL REFERENCES agent_messages(id),
     idx INTEGER NOT NULL,
     type TEXT NOT NULL,
     text TEXT NOT NULL DEFAULT '',
     tool_call_id TEXT,
     tool_name TEXT NOT NULL DEFAULT '',
     args_json TEXT NOT NULL DEFAULT '{}',
     result_json TEXT NOT NULL DEFAULT '',
     is_error INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0, 1)),
     artifact_json TEXT NOT NULL DEFAULT '',
     approval_status TEXT NOT NULL DEFAULT '',
     proposal_id TEXT REFERENCES proposals(id),
     created_at DATETIME NOT NULL,
     updated_at DATETIME NOT NULL
 );

 -- tool_call_id is the locator for add-tool-result receipts (§3.1, §7). Unique
 -- only when present; SQLite treats NULLs as distinct so text parts coexist.
 CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_parts_tool_call
     ON agent_message_parts(tool_call_id) WHERE tool_call_id IS NOT NULL;
 CREATE INDEX IF NOT EXISTS idx_agent_parts_message
     ON agent_message_parts(message_id, idx);

 CREATE TABLE IF NOT EXISTS agent_run_chunks (
     run_id TEXT NOT NULL REFERENCES agent_runs(id),
     seq INTEGER NOT NULL,
     user_id TEXT NOT NULL REFERENCES users(id),
     chunk_json TEXT NOT NULL,
     created_at DATETIME NOT NULL,
     PRIMARY KEY (run_id, seq)
 );

 CREATE INDEX IF NOT EXISTS idx_agent_run_chunks_user
     ON agent_run_chunks(user_id, run_id, seq);

 INSERT INTO schema_migrations(version, name, applied_at)
 VALUES (5, 'agent_runtime', CURRENT_TIMESTAMP);
