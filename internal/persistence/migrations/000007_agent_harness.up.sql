-- Agent harness runtime (doc/harness.md §10, §11).
--
-- A harness token is the capability that lets a host (the browser's WASM
-- instance or the Node sidecar) drive one run: it authorizes the model proxy,
-- the tool execution surface, the checkpoint endpoints, the approval long poll
-- and the heartbeat — and nothing else. It is NOT the run_token of
-- doc/interface.md §13, which leases an AgentJob attempt to a Worker; the two
-- never share issuance or validation code (doc/harness.md §10.1).
--
-- Only the token's hash is stored, so a database leak does not hand out running
-- capability.

CREATE TABLE IF NOT EXISTS agent_harness_tokens (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL REFERENCES users(id),
    thread_id TEXT NOT NULL REFERENCES agent_threads(id),
    run_id TEXT NOT NULL REFERENCES agent_runs(id),
    mode TEXT NOT NULL DEFAULT 'wasm' CHECK (mode IN ('wasm', 'sidecar')),
    tools_etag TEXT NOT NULL DEFAULT '',
    expires_at DATETIME NOT NULL,
    -- NULL until the first heartbeat; the loss detector falls back to created_at
    -- so a host that never beats is still reaped (doc/harness.md §10.4).
    last_heartbeat_at DATETIME,
    revoked_at DATETIME,
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_harness_tokens_run
    ON agent_harness_tokens(run_id);
CREATE INDEX IF NOT EXISTS idx_agent_harness_tokens_expiry
    ON agent_harness_tokens(expires_at);

-- Who drives a run: '' = the in-process loop (no model configured, or a client
-- that does not speak harness), 'wasm' = the browser host, 'sidecar' = the Node
-- host behind the Worker (doc/harness.md §1.2).
ALTER TABLE agent_runs ADD COLUMN harness_mode TEXT NOT NULL DEFAULT '';
-- Set by POST /agent/runs/{id}/cancellation. Cancellation is best effort and
-- travels over three channels; this flag is the durable one (doc/harness.md §5.2).
ALTER TABLE agent_runs ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0
    CHECK (cancel_requested IN (0, 1));
-- Approval waiting does not count against the 180s run wall clock
-- (doc/harness.md §7), so the accumulated wait is subtracted before the check.
ALTER TABLE agent_runs ADD COLUMN approval_wait_ms INTEGER NOT NULL DEFAULT 0;
-- Model calls made for this run. The loop moved to the host, but the turn budget did
-- not: the proxy counts calls and refuses the one past the limit (doc/harness.md §1.1,
-- agent-impl.md §6).
ALTER TABLE agent_runs ADD COLUMN model_calls INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (7, 'agent_harness', CURRENT_TIMESTAMP);
