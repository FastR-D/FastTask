CREATE TABLE panel_profiles (
    user_id TEXT PRIMARY KEY REFERENCES users(id),
    display_name TEXT NOT NULL DEFAULT '',
    seat TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '今日任务',
    avatar_data_url TEXT NOT NULL DEFAULT '',
    weather_override TEXT NOT NULL DEFAULT '',
    task_overrides_json TEXT NOT NULL DEFAULT '{}',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE TABLE panel_pairings (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    device_name TEXT NOT NULL,
    scope TEXT NOT NULL CHECK(scope = 'panel:snapshot:read'),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL,
    revoked_at DATETIME
);
CREATE INDEX idx_panel_pairings_user ON panel_pairings(user_id, revoked_at);
INSERT INTO schema_migrations(version,name,applied_at) VALUES (20,'panel_profiles',CURRENT_TIMESTAMP);
