-- PostgreSQL baseline equivalent to SQLite migrations 1–25.

CREATE FUNCTION json_valid(doc TEXT) RETURNS BOOLEAN IMMUTABLE STRICT LANGUAGE SQL
AS $$ SELECT pg_input_is_valid(doc, 'jsonb') $$;
CREATE FUNCTION json_extract(doc TEXT, path TEXT) RETURNS TEXT IMMUTABLE STRICT LANGUAGE SQL
AS $$ SELECT CASE WHEN json_valid(doc) THEN doc::jsonb #>> string_to_array(substr(path,3),'.') END $$;

CREATE TABLE schema_migrations (version BIGINT PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL);

CREATE TABLE users (id TEXT PRIMARY KEY, identifier TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, display_name TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai', locale TEXT NOT NULL DEFAULT 'zh-CN', role TEXT NOT NULL DEFAULT 'member', status TEXT NOT NULL DEFAULT 'active', revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);

CREATE TABLE sessions (id TEXT PRIMARY KEY, family_id TEXT NOT NULL, user_id TEXT NOT NULL, refresh_hash TEXT NOT NULL UNIQUE, replaced_by_hash TEXT, status TEXT NOT NULL DEFAULT 'active', expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, auth_source TEXT NOT NULL DEFAULT 'local', cas_issuer TEXT NOT NULL DEFAULT '', cas_sid TEXT NOT NULL DEFAULT '', cas_link_id TEXT NOT NULL DEFAULT '', cas_link_version BIGINT NOT NULL DEFAULT 0);

CREATE TABLE goals (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', success_criteria TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', target_date TEXT, revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ, deletion_id TEXT NOT NULL DEFAULT '');

CREATE TABLE tasks (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, goal_id TEXT NOT NULL, parent_id TEXT, type TEXT NOT NULL DEFAULT 'task', title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'ready', priority BIGINT NOT NULL DEFAULT 50, position BIGINT NOT NULL DEFAULT 0, estimate_minutes BIGINT NOT NULL DEFAULT 25, success_criteria TEXT NOT NULL, minimum_action TEXT NOT NULL, blocked_reason TEXT NOT NULL DEFAULT '', revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ, deletion_id TEXT NOT NULL DEFAULT '', depends_on TEXT NOT NULL DEFAULT '[]', planned_start TEXT, planned_end TEXT);

CREATE TABLE task_tree_revisions (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, goal_id TEXT NOT NULL, revision BIGINT NOT NULL, reason TEXT NOT NULL, source TEXT NOT NULL, snapshot_json TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(goal_id, revision));

CREATE TABLE daily_plans (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, local_date TEXT NOT NULL, timezone TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', algorithm_version TEXT NOT NULL DEFAULT 'deterministic-v1', current_revision BIGINT NOT NULL DEFAULT 1, revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, frozen_at TIMESTAMPTZ, manual_items_removed BIGINT NOT NULL DEFAULT 0 CHECK (manual_items_removed IN (0,1)), UNIQUE(user_id, local_date, timezone));

CREATE TABLE daily_plan_revisions (id TEXT PRIMARY KEY, plan_id TEXT NOT NULL, revision BIGINT NOT NULL, input_snapshot TEXT NOT NULL, input_hash TEXT NOT NULL, plan_snapshot TEXT NOT NULL, reason TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(plan_id, revision));

CREATE TABLE daily_plan_items (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, plan_id TEXT NOT NULL, plan_revision BIGINT NOT NULL, task_id TEXT, kind TEXT NOT NULL, title TEXT NOT NULL, commitment TEXT NOT NULL, minimum_action TEXT NOT NULL, allowed_types TEXT NOT NULL, target_minutes BIGINT NOT NULL DEFAULT 50, status TEXT NOT NULL DEFAULT 'planned', completion_type TEXT NOT NULL DEFAULT '', completion_summary TEXT NOT NULL DEFAULT '', position BIGINT NOT NULL DEFAULT 0, revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, logical_id TEXT NOT NULL DEFAULT '');

CREATE TABLE work_sessions (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, task_id TEXT NOT NULL, daily_plan_item_id TEXT, session_type TEXT NOT NULL DEFAULT 'pomodoro', target_minutes BIGINT NOT NULL DEFAULT 25, status TEXT NOT NULL DEFAULT 'running', started_at TIMESTAMPTZ NOT NULL, paused_at TIMESTAMPTZ, accumulated_seconds BIGINT NOT NULL DEFAULT 0, ended_at TIMESTAMPTZ, duration_seconds BIGINT NOT NULL DEFAULT 0, outcome TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, last_transition_at TIMESTAMPTZ);

CREATE TABLE progress_events (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, goal_id TEXT, task_id TEXT, plan_item_id TEXT, type TEXT NOT NULL, summary TEXT NOT NULL, evidence_json TEXT NOT NULL DEFAULT '[]', occurred_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL);

CREATE TABLE conversations (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, goal_id TEXT, title TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);

CREATE TABLE conversation_messages (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, conversation_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, job_id TEXT, transcription_job_id TEXT, created_at TIMESTAMPTZ NOT NULL);

CREATE TABLE agent_jobs (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, type TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', retry_of_job_id TEXT, subject_type TEXT NOT NULL DEFAULT '', subject_id TEXT NOT NULL DEFAULT '', base_revision BIGINT NOT NULL DEFAULT 0, input_json TEXT NOT NULL DEFAULT '{}', output_json TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '', attempt_count BIGINT NOT NULL DEFAULT 0, max_attempts BIGINT NOT NULL DEFAULT 3, run_after TIMESTAMPTZ NOT NULL, locked_by TEXT NOT NULL DEFAULT '', locked_until TIMESTAMPTZ, lease_version BIGINT NOT NULL DEFAULT 0, run_token_hash TEXT NOT NULL DEFAULT '', cancel_requested BIGINT NOT NULL DEFAULT 0, revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, updated_at TIMESTAMPTZ NOT NULL);

CREATE TABLE devices (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'eink_panel', timezone TEXT NOT NULL, capabilities_json TEXT NOT NULL DEFAULT '{}', token_hash TEXT NOT NULL UNIQUE, status TEXT NOT NULL DEFAULT 'active', last_seen_at TIMESTAMPTZ, revision BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);

CREATE TABLE idempotency_records (id TEXT PRIMARY KEY, principal TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, idem_key TEXT NOT NULL, request_hash TEXT NOT NULL, status_code BIGINT NOT NULL, response_json TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(principal, method, path, idem_key));

CREATE TABLE outbox_events (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL, processed_at TIMESTAMPTZ);

CREATE TABLE external_imports (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    schema_version TEXT NOT NULL DEFAULT '1.0',
    trace_id TEXT NOT NULL DEFAULT '',
    source_system TEXT NOT NULL,
    source_external_id TEXT NOT NULL,
    source_url TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    suggested_goal_id TEXT,
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'candidate',
    task_id TEXT,
    decision_note TEXT NOT NULL DEFAULT '',
    revision BIGINT NOT NULL DEFAULT 1,
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(user_id, source_system, source_external_id)
);

CREATE TABLE model_providers (
     id TEXT PRIMARY KEY,
     name TEXT NOT NULL UNIQUE,
     provider_type TEXT NOT NULL DEFAULT 'openai_compatible',
     base_url TEXT NOT NULL,
     model_name TEXT NOT NULL,
     transcription_model TEXT NOT NULL DEFAULT '',
     api_key_ciphertext TEXT NOT NULL,
     api_key_hint TEXT NOT NULL DEFAULT '',
     status TEXT NOT NULL DEFAULT 'active',
     is_default BIGINT NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
     revision BIGINT NOT NULL DEFAULT 1,
     created_at TIMESTAMPTZ NOT NULL,
     updated_at TIMESTAMPTZ NOT NULL
 );

CREATE TABLE admin_audit_events (
     id TEXT PRIMARY KEY,
     actor_user_id TEXT NOT NULL,
     target_user_id TEXT,
     action TEXT NOT NULL,
     detail_json TEXT NOT NULL DEFAULT '{}',
     created_at TIMESTAMPTZ NOT NULL
 );

CREATE TABLE task_coords (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    lens TEXT NOT NULL DEFAULT 'research_risk',
    x BIGINT NOT NULL,
    y BIGINT NOT NULL,
    source TEXT NOT NULL DEFAULT 'agent',
    pinned BIGINT NOT NULL DEFAULT 0,
    rationale TEXT NOT NULL DEFAULT '',
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(task_id, lens)
);

CREATE TABLE agent_message_parts (
     id TEXT PRIMARY KEY,
     user_id TEXT NOT NULL,
     message_id TEXT NOT NULL,
     idx BIGINT NOT NULL,
     type TEXT NOT NULL,
     text TEXT NOT NULL DEFAULT '',
     tool_call_id TEXT,
     tool_name TEXT NOT NULL DEFAULT '',
     args_json TEXT NOT NULL DEFAULT '{}',
     result_json TEXT NOT NULL DEFAULT '',
     is_error BIGINT NOT NULL DEFAULT 0 CHECK (is_error IN (0, 1)),
     artifact_json TEXT NOT NULL DEFAULT '',
     approval_status TEXT NOT NULL DEFAULT '',
     proposal_id TEXT,
     created_at TIMESTAMPTZ NOT NULL,
     updated_at TIMESTAMPTZ NOT NULL
 , tool_model_turn BIGINT NOT NULL DEFAULT 0, tool_args_ready BIGINT NOT NULL DEFAULT 0, tool_execution_claimed BIGINT NOT NULL DEFAULT 0);

CREATE TABLE agent_run_chunks (
     run_id TEXT NOT NULL,
     seq BIGINT NOT NULL,
     user_id TEXT NOT NULL,
     chunk_json TEXT NOT NULL,
     created_at TIMESTAMPTZ NOT NULL,
     PRIMARY KEY (run_id, seq)
 );

CREATE TABLE agent_threads (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    goal_id TEXT,
    -- Empty string means "no title yet"; the list endpoints fall back to a
    -- deterministic title (doc/chat-features.md §2.4).
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'regular' CHECK (status IN ('regular', 'archived')),
    -- Frontend-owned, non-authoritative display data. Nothing that participates
    -- in authorization or an invariant may live here (§2.3).
    custom TEXT NOT NULL DEFAULT '',
    -- libfx conversation checkpoint (doc/harness.md §6). Opaque, versioned by
    -- libfx_version; NULL for threads that predate the harness.
    checkpoint BYTEA,
    libfx_version TEXT NOT NULL DEFAULT '',
    -- Set only by the backfill below, never written afterwards. UNIQUE makes the
    -- backfill re-entrant (§2.3).
    source_conversation_id TEXT UNIQUE,
    last_message_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE "agent_runs" (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    job_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued',
    state_json TEXT NOT NULL DEFAULT '{}',
    checkpoint_seq BIGINT NOT NULL DEFAULT 0,
    parent_message_id TEXT,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
, harness_mode TEXT NOT NULL DEFAULT '', cancel_requested BIGINT NOT NULL DEFAULT 0
    CHECK (cancel_requested IN (0, 1)), approval_wait_ms BIGINT NOT NULL DEFAULT 0, model_calls BIGINT NOT NULL DEFAULT 0, started_at TIMESTAMPTZ, approval_started_at TIMESTAMPTZ, model_attempts BIGINT NOT NULL DEFAULT 0, model_inflight TEXT NOT NULL DEFAULT '', usage_json TEXT NOT NULL DEFAULT '', model_verified_stop TEXT NOT NULL DEFAULT '');

CREATE TABLE "agent_messages" (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    parent_id TEXT,
    role TEXT NOT NULL,
    seq BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE agent_harness_tokens (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'wasm' CHECK (mode IN ('wasm', 'sidecar')),
    tools_etag TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    -- NULL until the first heartbeat; the loss detector falls back to created_at
    -- so a host that never beats is still reaped (doc/harness.md §10.4).
    last_heartbeat_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
, generation TEXT NOT NULL DEFAULT '', next_token_cipher TEXT NOT NULL DEFAULT '', rotation_until TIMESTAMPTZ);

CREATE TABLE agent_attachments (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    -- NULL until the attachment is sent with a message; orphans are reclaimed by TTL (§4.5).
    thread_id TEXT,
    message_id TEXT,
    -- The sniffed type, never the one the client claimed (§4.5).
    mime TEXT NOT NULL,
    bytes BIGINT NOT NULL,
    width BIGINT NOT NULL DEFAULT 0,
    height BIGINT NOT NULL DEFAULT 0,
    -- Server-local path. No object storage: the deployment is one binary plus its data directory (§4.3).
    path TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE fastcas_transactions (state TEXT PRIMARY KEY, payload TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL);

CREATE TABLE fastcas_links (id TEXT PRIMARY KEY, issuer TEXT NOT NULL, client_id TEXT NOT NULL, user_id TEXT NOT NULL, subject TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('prepared','active','revoked')), version BIGINT NOT NULL, verified_at TIMESTAMPTZ NOT NULL);

CREATE TABLE fastcas_events (issuer TEXT NOT NULL,id TEXT NOT NULL,processed_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(issuer,id));

CREATE TABLE notification_channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL CHECK (provider IN ('telegram', 'bark', 'fcm', 'apns')),
    -- Provider base URL. Empty means the adapter's own default (api.telegram.org,
    -- fcm.googleapis.com, the APNs host implied by environment).
    endpoint TEXT NOT NULL DEFAULT '',
    settings_json TEXT NOT NULL DEFAULT '{}',
    secret_ciphertext TEXT NOT NULL DEFAULT '',
    secret_hint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- The last credential check an administrator ran, and what it said. NULL last_check_at
    -- means the channel was never checked; an empty last_check_status with a timestamp is
    -- not a state the application can write.
    last_check_at TIMESTAMPTZ,
    last_check_status TEXT NOT NULL DEFAULT '' CHECK (last_check_status IN ('', 'ok', 'failed')),
    last_error TEXT NOT NULL DEFAULT '',
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE notification_targets (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    address_ciphertext TEXT NOT NULL,
    -- sha256 of the plaintext address: it makes the one-registration-per-address rule
    -- exact without keeping a readable copy of a device token.
    address_hash TEXT NOT NULL,
    -- The masked address an administrator or user recognises the target by.
    address_hint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'invalid')),
    failure_count BIGINT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    last_sent_at TIMESTAMPTZ,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(user_id, channel_id, address_hash)
);

CREATE TABLE notification_messages (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    target_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '{}',
    -- queued  waiting for run_after
    -- sending claimed by a dispatcher; run_after is the claim's lease expiry, so a
    --         dispatcher that died mid-send leaves a row that becomes due again
    -- sent    the provider accepted it
    -- failed  the attempts ran out, or the provider will never accept it
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts BIGINT NOT NULL DEFAULT 0,
    max_attempts BIGINT NOT NULL DEFAULT 3,
    provider_message_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    run_after TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ
);

CREATE TABLE user_planning_settings (
    user_id TEXT PRIMARY KEY,
    primary_goal_id TEXT,
    daily_budget_minutes BIGINT NOT NULL DEFAULT 120 CHECK(daily_budget_minutes BETWEEN 30 AND 480),
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE "proposals" (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    goal_id TEXT,
    kind TEXT NOT NULL DEFAULT 'task_tree_patch',
    job_id TEXT,
    base_revision BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    instruction TEXT NOT NULL DEFAULT '',
    patch_json TEXT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE integration_connections (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    system TEXT NOT NULL CHECK (system IN ('fastwrite','fastread','fastnews','fastppt','fastlab')),
    api_url TEXT NOT NULL DEFAULT '',
    web_url TEXT NOT NULL DEFAULT '',
    profile TEXT NOT NULL DEFAULT 'auto',
    auth_kind TEXT NOT NULL DEFAULT 'bearer',
    credential_ciphertext TEXT NOT NULL DEFAULT '',
    personal_instance BIGINT NOT NULL DEFAULT 0,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(user_id, system)
);

CREATE TABLE fastresearch_accounts (
    account_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL UNIQUE,
    key_id TEXT NOT NULL DEFAULT '',
    person TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE panel_profiles (
    user_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    seat TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '今日任务',
    avatar_data_url TEXT NOT NULL DEFAULT '',
    weather_override TEXT NOT NULL DEFAULT '',
    task_overrides_json TEXT NOT NULL DEFAULT '{}',
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE panel_pairings (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    device_name TEXT NOT NULL,
    scope TEXT NOT NULL CHECK(scope = 'panel:snapshot:read'),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE fastcas_signups (
 binding TEXT PRIMARY KEY,
 issuer TEXT NOT NULL,
 subject TEXT NOT NULL,
 email TEXT NOT NULL,
 name TEXT NOT NULL,
 prepared TEXT NOT NULL DEFAULT '',
 expires_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE "sessions" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "goals" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "tasks" ADD FOREIGN KEY ("parent_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "tasks" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "tasks" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "task_tree_revisions" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "task_tree_revisions" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "daily_plans" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "daily_plan_revisions" ADD FOREIGN KEY ("plan_id") REFERENCES "daily_plans" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "daily_plan_items" ADD FOREIGN KEY ("task_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "daily_plan_items" ADD FOREIGN KEY ("plan_id") REFERENCES "daily_plans" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "daily_plan_items" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "work_sessions" ADD FOREIGN KEY ("daily_plan_item_id") REFERENCES "daily_plan_items" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "work_sessions" ADD FOREIGN KEY ("task_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "work_sessions" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "progress_events" ADD FOREIGN KEY ("plan_item_id") REFERENCES "daily_plan_items" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "progress_events" ADD FOREIGN KEY ("task_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "progress_events" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "progress_events" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "conversations" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "conversations" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "conversation_messages" ADD FOREIGN KEY ("conversation_id") REFERENCES "conversations" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "conversation_messages" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_jobs" ADD FOREIGN KEY ("retry_of_job_id") REFERENCES "agent_jobs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_jobs" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "devices" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "external_imports" ADD FOREIGN KEY ("task_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "external_imports" ADD FOREIGN KEY ("suggested_goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "external_imports" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "admin_audit_events" ADD FOREIGN KEY ("target_user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "admin_audit_events" ADD FOREIGN KEY ("actor_user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "task_coords" ADD FOREIGN KEY ("task_id") REFERENCES "tasks" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "task_coords" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_message_parts" ADD FOREIGN KEY ("proposal_id") REFERENCES "proposals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_message_parts" ADD FOREIGN KEY ("message_id") REFERENCES "agent_messages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_message_parts" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_run_chunks" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_run_chunks" ADD FOREIGN KEY ("run_id") REFERENCES "agent_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_threads" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_threads" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_runs" ADD FOREIGN KEY ("parent_message_id") REFERENCES "agent_messages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_runs" ADD FOREIGN KEY ("thread_id") REFERENCES "agent_threads" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_runs" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_messages" ADD FOREIGN KEY ("parent_id") REFERENCES "agent_messages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_messages" ADD FOREIGN KEY ("run_id") REFERENCES "agent_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_messages" ADD FOREIGN KEY ("thread_id") REFERENCES "agent_threads" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_messages" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_harness_tokens" ADD FOREIGN KEY ("run_id") REFERENCES "agent_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_harness_tokens" ADD FOREIGN KEY ("thread_id") REFERENCES "agent_threads" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_harness_tokens" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_attachments" ADD FOREIGN KEY ("message_id") REFERENCES "agent_messages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_attachments" ADD FOREIGN KEY ("thread_id") REFERENCES "agent_threads" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "agent_attachments" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "fastcas_links" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "notification_targets" ADD FOREIGN KEY ("channel_id") REFERENCES "notification_channels" ("id") ON UPDATE NO ACTION ON DELETE CASCADE DEFERRABLE;

ALTER TABLE "notification_targets" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE DEFERRABLE;

ALTER TABLE "notification_messages" ADD FOREIGN KEY ("target_id") REFERENCES "notification_targets" ("id") ON UPDATE NO ACTION ON DELETE CASCADE DEFERRABLE;

ALTER TABLE "notification_messages" ADD FOREIGN KEY ("channel_id") REFERENCES "notification_channels" ("id") ON UPDATE NO ACTION ON DELETE CASCADE DEFERRABLE;

ALTER TABLE "notification_messages" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE DEFERRABLE;

ALTER TABLE "user_planning_settings" ADD FOREIGN KEY ("primary_goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "user_planning_settings" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "proposals" ADD FOREIGN KEY ("goal_id") REFERENCES "goals" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "proposals" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "integration_connections" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "fastresearch_accounts" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "panel_profiles" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

ALTER TABLE "panel_pairings" ADD FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE;

CREATE INDEX idx_sessions_user ON sessions(user_id, status);

CREATE INDEX idx_goals_user ON goals(user_id, status, created_at);

CREATE INDEX idx_tasks_goal ON tasks(goal_id, parent_id, status, position);

CREATE INDEX idx_tasks_user ON tasks(user_id, status, priority);

CREATE INDEX idx_plan_items ON daily_plan_items(plan_id, plan_revision, kind, position);

CREATE UNIQUE INDEX idx_one_active_session ON work_sessions(user_id) WHERE status IN ('running', 'paused');

CREATE INDEX idx_progress_task ON progress_events(task_id, occurred_at);

CREATE INDEX idx_jobs_queue ON agent_jobs(status, run_after, created_at);

CREATE INDEX idx_external_imports_user_status
    ON external_imports(user_id, status, created_at);

CREATE INDEX idx_external_imports_task
    ON external_imports(task_id);

CREATE INDEX idx_model_providers_default
     ON model_providers(status, is_default, updated_at DESC);

CREATE INDEX idx_admin_audit_actor
     ON admin_audit_events(actor_user_id, created_at DESC);

CREATE INDEX idx_admin_audit_target
     ON admin_audit_events(target_user_id, created_at DESC);

CREATE INDEX idx_task_coords_user_lens ON task_coords(user_id, lens);

CREATE UNIQUE INDEX idx_agent_parts_tool_call
     ON agent_message_parts(tool_call_id) WHERE tool_call_id IS NOT NULL;

CREATE INDEX idx_agent_parts_message
     ON agent_message_parts(message_id, idx);

CREATE INDEX idx_agent_run_chunks_user
     ON agent_run_chunks(user_id, run_id, seq);

CREATE INDEX idx_agent_threads_user
    ON agent_threads(user_id, status, last_message_at DESC);

CREATE UNIQUE INDEX idx_agent_runs_active_thread
    ON agent_runs(thread_id) WHERE status IN ('queued', 'running');

CREATE INDEX idx_agent_runs_user
    ON agent_runs(user_id, created_at DESC);

CREATE INDEX idx_agent_messages_thread_seq
    ON agent_messages(thread_id, seq);

CREATE INDEX idx_agent_messages_run
    ON agent_messages(run_id, seq);

CREATE INDEX idx_agent_harness_tokens_run
    ON agent_harness_tokens(run_id);

CREATE INDEX idx_agent_harness_tokens_expiry
    ON agent_harness_tokens(expires_at);

CREATE INDEX idx_agent_attachments_thread
    ON agent_attachments(user_id, thread_id, created_at DESC);

CREATE INDEX idx_agent_attachments_orphans
    ON agent_attachments(created_at) WHERE thread_id IS NULL;

CREATE INDEX idx_sessions_cas_link ON sessions(cas_issuer,cas_link_id,auth_source);

CREATE UNIQUE INDEX idx_fastcas_user_live ON fastcas_links(issuer,user_id) WHERE state != 'revoked';

CREATE UNIQUE INDEX idx_fastcas_subject_live ON fastcas_links(issuer,subject) WHERE state != 'revoked';

CREATE INDEX idx_notification_channels_status
    ON notification_channels(status, provider, updated_at DESC);

CREATE INDEX idx_notification_targets_user
    ON notification_targets(user_id, status);

CREATE INDEX idx_notification_targets_channel
    ON notification_targets(channel_id, status, created_at DESC);

CREATE INDEX idx_notification_messages_due
    ON notification_messages(status, run_after);

CREATE INDEX idx_notification_messages_user
    ON notification_messages(user_id, created_at DESC);

CREATE INDEX idx_notification_messages_channel
    ON notification_messages(channel_id, created_at DESC);

CREATE INDEX idx_notification_messages_prune
    ON notification_messages(created_at) WHERE status IN ('sent', 'failed');

CREATE INDEX idx_agent_jobs_user_created ON agent_jobs(user_id, created_at);

CREATE INDEX idx_sessions_family ON sessions(family_id);

CREATE INDEX idx_idempotency_expiry ON idempotency_records(status_code, expires_at);

CREATE INDEX idx_conversation_messages_conversation ON conversation_messages(conversation_id, created_at);

CREATE INDEX idx_plan_items_logical ON daily_plan_items(logical_id, plan_revision);

CREATE INDEX idx_goals_user_deleted ON goals(user_id, deleted_at, status);

CREATE INDEX idx_tasks_user_deleted ON tasks(user_id, deleted_at, goal_id);

CREATE INDEX idx_proposals_user_status ON proposals(user_id,status,created_at);

CREATE INDEX idx_panel_pairings_user ON panel_pairings(user_id, revoked_at);

INSERT INTO schema_migrations(version,name,applied_at) VALUES
 (1,'init',CURRENT_TIMESTAMP),
 (2,'external_imports',CURRENT_TIMESTAMP),
 (3,'admin_platform',CURRENT_TIMESTAMP),
 (4,'task_coords',CURRENT_TIMESTAMP),
 (5,'agent_runtime',CURRENT_TIMESTAMP),
 (6,'agent_threads',CURRENT_TIMESTAMP),
 (7,'agent_harness',CURRENT_TIMESTAMP),
 (8,'agent_attachments',CURRENT_TIMESTAMP),
 (9,'fastcas',CURRENT_TIMESTAMP),
 (10,'notifications',CURRENT_TIMESTAMP),
 (11,'query_indexes',CURRENT_TIMESTAMP),
 (12,'daily_planning',CURRENT_TIMESTAMP),
 (13,'recoverable_deletion',CURRENT_TIMESTAMP),
 (14,'goal_proposals',CURRENT_TIMESTAMP),
 (15,'integration_connections',CURRENT_TIMESTAMP),
 (16,'work_session_chronology',CURRENT_TIMESTAMP),
 (17,'tool_execution_identity',CURRENT_TIMESTAMP),
 (18,'task_dependencies',CURRENT_TIMESTAMP),
 (19,'fastresearch_accounts',CURRENT_TIMESTAMP),
 (20,'panel_profiles',CURRENT_TIMESTAMP),
 (21,'daily_plan_item_deletions',CURRENT_TIMESTAMP),
 (22,'harness_execution',CURRENT_TIMESTAMP),
 (23,'harness_token_rotation',CURRENT_TIMESTAMP),
 (24,'task_planned_dates',CURRENT_TIMESTAMP),
 (25,'fastcas_signup',CURRENT_TIMESTAMP);
