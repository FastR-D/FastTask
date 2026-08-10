CREATE TABLE external_imports (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    schema_version TEXT NOT NULL DEFAULT '1.0',
    trace_id TEXT NOT NULL DEFAULT '',
    source_system TEXT NOT NULL,
    source_external_id TEXT NOT NULL,
    source_url TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    suggested_goal_id TEXT REFERENCES goals(id),
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'candidate',
    task_id TEXT REFERENCES tasks(id),
    decision_note TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1,
    decided_at DATETIME,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE(user_id, source_system, source_external_id)
);

CREATE INDEX idx_external_imports_user_status
    ON external_imports(user_id, status, created_at);

CREATE INDEX idx_external_imports_task
    ON external_imports(task_id);

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (2, 'external_imports', CURRENT_TIMESTAMP);
