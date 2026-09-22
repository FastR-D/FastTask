--migration:no-transaction
-- Agent threads (doc/chat-features.md §2.3).
--
-- Until now the conversations table doubled as the agent Thread
-- (000005_agent_runtime). Multi-session chat needs thread-owned state the
-- conversation aggregate does not have — the libfx checkpoint, the
-- regular/archived status, the client-owned `custom` blob — so threads move to
-- their own table and agent_runs / agent_messages are repointed at it.
--
-- This migration rewrites two tables that other tables reference, which SQLite
-- can only do with foreign key enforcement disabled (the documented 12-step
-- ALTER procedure). PRAGMA foreign_keys is a no-op inside a transaction, so the
-- runner executes this file on a dedicated connection outside one; the marker on
-- the first line selects that path (internal/persistence/store.go). Every
-- statement is idempotent, so a failed run can be replayed.

PRAGMA foreign_keys = OFF;

CREATE TABLE IF NOT EXISTS agent_threads (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    goal_id TEXT REFERENCES goals(id),
    -- Empty string means "no title yet"; the list endpoints fall back to a
    -- deterministic title (doc/chat-features.md §2.4).
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'regular' CHECK (status IN ('regular', 'archived')),
    -- Frontend-owned, non-authoritative display data. Nothing that participates
    -- in authorization or an invariant may live here (§2.3).
    custom TEXT NOT NULL DEFAULT '',
    -- libfx conversation checkpoint (doc/harness.md §6). Opaque, versioned by
    -- libfx_version; NULL for threads that predate the harness.
    checkpoint BLOB,
    libfx_version TEXT NOT NULL DEFAULT '',
    -- Set only by the backfill below, never written afterwards. UNIQUE makes the
    -- backfill re-entrant (§2.3).
    source_conversation_id TEXT UNIQUE,
    last_message_at DATETIME,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_threads_user
    ON agent_threads(user_id, status, last_message_at DESC);

-- Backfill step 1: one thread per conversation that carries agent messages.
INSERT INTO agent_threads (
    id, user_id, goal_id, title, status, custom, checkpoint, libfx_version,
    source_conversation_id, last_message_at, created_at, updated_at
)
SELECT
    'thr_' || lower(hex(randomblob(16))),
    c.user_id,
    c.goal_id,
    COALESCE(
        NULLIF(TRIM(c.title), ''),
        (SELECT substr(p.text, 1, 30)
           FROM agent_messages m
           JOIN agent_message_parts p ON p.message_id = m.id
          WHERE m.thread_id = c.id AND m.role = 'user' AND p.type = 'text' AND p.text <> ''
          ORDER BY m.seq, p.idx
          LIMIT 1),
        '历史对话'
    ),
    'regular',
    '',
    NULL,
    '',
    c.id,
    (SELECT MAX(m.created_at) FROM agent_messages m WHERE m.thread_id = c.id),
    c.created_at,
    c.updated_at
FROM conversations c
WHERE EXISTS (SELECT 1 FROM agent_messages m WHERE m.thread_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM agent_threads t WHERE t.source_conversation_id = c.id);

-- Repoint the rows that referenced a backfilled conversation.
UPDATE agent_runs
   SET thread_id = (SELECT t.id FROM agent_threads t WHERE t.source_conversation_id = agent_runs.thread_id)
 WHERE EXISTS (SELECT 1 FROM agent_threads t WHERE t.source_conversation_id = agent_runs.thread_id);
UPDATE agent_messages
   SET thread_id = (SELECT t.id FROM agent_threads t WHERE t.source_conversation_id = agent_messages.thread_id)
 WHERE EXISTS (SELECT 1 FROM agent_threads t WHERE t.source_conversation_id = agent_messages.thread_id);

-- Backfill step 2 (§2.3): agent messages that belong to no conversation at all
-- land on one "历史对话" fallback thread per user. The conversations foreign key
-- makes this empty in practice; it is kept so the migration cannot leave a
-- message without a thread once thread_id becomes NOT NULL against agent_threads.
INSERT INTO agent_threads (
    id, user_id, goal_id, title, status, custom, checkpoint, libfx_version,
    source_conversation_id, last_message_at, created_at, updated_at
)
SELECT
    'thr_' || lower(hex(randomblob(16))),
    o.user_id,
    NULL,
    '历史对话',
    'regular',
    '',
    NULL,
    '',
    NULL,
    o.last_message_at,
    o.last_message_at,
    o.last_message_at
FROM (
    SELECT m.user_id AS user_id, COALESCE(MAX(m.created_at), CURRENT_TIMESTAMP) AS last_message_at
      FROM agent_messages m
     WHERE NOT EXISTS (SELECT 1 FROM agent_threads t WHERE t.id = m.thread_id)
     GROUP BY m.user_id
) o
WHERE NOT EXISTS (
    SELECT 1 FROM agent_threads t
     WHERE t.user_id = o.user_id AND t.title = '历史对话' AND t.source_conversation_id IS NULL
);

UPDATE agent_messages
   SET thread_id = (SELECT t.id FROM agent_threads t
                     WHERE t.user_id = agent_messages.user_id
                       AND t.title = '历史对话'
                       AND t.source_conversation_id IS NULL
                     LIMIT 1)
 WHERE NOT EXISTS (SELECT 1 FROM agent_threads t WHERE t.id = agent_messages.thread_id);
UPDATE agent_runs
   SET thread_id = (SELECT t.id FROM agent_threads t
                     WHERE t.user_id = agent_runs.user_id
                       AND t.title = '历史对话'
                       AND t.source_conversation_id IS NULL
                     LIMIT 1)
 WHERE NOT EXISTS (SELECT 1 FROM agent_threads t WHERE t.id = agent_runs.thread_id);

-- Rebuild agent_runs so thread_id references agent_threads and stays NOT NULL
-- (backfill step 3).
CREATE TABLE IF NOT EXISTS agent_runs_new (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    thread_id TEXT NOT NULL REFERENCES agent_threads(id),
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

INSERT INTO agent_runs_new (
    id, user_id, thread_id, job_id, status, state_json, checkpoint_seq,
    parent_message_id, error_code, error_message, revision, created_at, updated_at, finished_at
)
SELECT id, user_id, thread_id, job_id, status, state_json, checkpoint_seq,
       parent_message_id, error_code, error_message, revision, created_at, updated_at, finished_at
FROM agent_runs;

DROP TABLE IF EXISTS agent_runs;
ALTER TABLE agent_runs_new RENAME TO agent_runs;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_runs_active_thread
    ON agent_runs(thread_id) WHERE status IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS idx_agent_runs_user
    ON agent_runs(user_id, created_at DESC);

-- Rebuild agent_messages the same way.
CREATE TABLE IF NOT EXISTS agent_messages_new (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    thread_id TEXT NOT NULL REFERENCES agent_threads(id),
    run_id TEXT NOT NULL REFERENCES agent_runs(id),
    parent_id TEXT REFERENCES agent_messages(id),
    role TEXT NOT NULL,
    seq INTEGER NOT NULL,
    created_at DATETIME NOT NULL
);

INSERT INTO agent_messages_new (id, user_id, thread_id, run_id, parent_id, role, seq, created_at)
SELECT id, user_id, thread_id, run_id, parent_id, role, seq, created_at
FROM agent_messages;

DROP TABLE IF EXISTS agent_messages;
ALTER TABLE agent_messages_new RENAME TO agent_messages;

CREATE INDEX IF NOT EXISTS idx_agent_messages_thread_seq
    ON agent_messages(thread_id, seq);
CREATE INDEX IF NOT EXISTS idx_agent_messages_run
    ON agent_messages(run_id, seq);

PRAGMA foreign_keys = ON;

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (6, 'agent_threads', CURRENT_TIMESTAMP);
