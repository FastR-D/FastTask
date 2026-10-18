CREATE TABLE fastresearch_accounts (
    account_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL UNIQUE REFERENCES users(id),
    key_id TEXT NOT NULL DEFAULT '',
    person TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
INSERT INTO schema_migrations(version,name,applied_at) VALUES (19,'fastresearch_accounts',CURRENT_TIMESTAMP);
