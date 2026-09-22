-- Agent attachments (doc/chat-features.md §4.3).
--
-- An attachment is a server-owned file with a reference that travels in a message. The reference is what
-- keeps images out of the transcript, the chunk log and the libfx checkpoint, all of which would
-- otherwise carry base64 through every turn (§4.2). Ownership is a column, not a convention: every read
-- is scoped by user_id, and the model proxy re-checks it before it expands a reference into bytes.

CREATE TABLE IF NOT EXISTS agent_attachments (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    -- NULL until the attachment is sent with a message; orphans are reclaimed by TTL (§4.5).
    thread_id TEXT REFERENCES agent_threads(id),
    message_id TEXT REFERENCES agent_messages(id),
    -- The sniffed type, never the one the client claimed (§4.5).
    mime TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    -- Server-local path. No object storage: the deployment is one binary plus its data directory (§4.3).
    path TEXT NOT NULL,
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_attachments_thread
    ON agent_attachments(user_id, thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_attachments_orphans
    ON agent_attachments(created_at) WHERE thread_id IS NULL;

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (8, 'agent_attachments', CURRENT_TIMESTAMP);
