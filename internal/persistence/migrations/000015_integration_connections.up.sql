CREATE TABLE integration_connections (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    system TEXT NOT NULL CHECK (system IN ('fastwrite','fastread','fastnews','fastppt','fastlab')),
    api_url TEXT NOT NULL DEFAULT '',
    web_url TEXT NOT NULL DEFAULT '',
    profile TEXT NOT NULL DEFAULT 'auto',
    auth_kind TEXT NOT NULL DEFAULT 'bearer',
    credential_ciphertext TEXT NOT NULL DEFAULT '',
    personal_instance INTEGER NOT NULL DEFAULT 0,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE(user_id, system)
);
INSERT INTO schema_migrations(version,name,applied_at) VALUES (15,'integration_connections',CURRENT_TIMESTAMP);
