CREATE TABLE task_coords (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    lens TEXT NOT NULL DEFAULT 'research_risk',
    x INTEGER NOT NULL,
    y INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'agent',
    pinned INTEGER NOT NULL DEFAULT 0,
    rationale TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE(task_id, lens)
);

CREATE INDEX idx_task_coords_user_lens ON task_coords(user_id, lens);

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (4, 'task_coords', CURRENT_TIMESTAMP);
