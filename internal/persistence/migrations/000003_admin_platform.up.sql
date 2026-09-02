 PRAGMA foreign_keys = ON;

 CREATE TABLE IF NOT EXISTS model_providers (
     id TEXT PRIMARY KEY,
     name TEXT NOT NULL UNIQUE,
     provider_type TEXT NOT NULL DEFAULT 'openai_compatible',
     base_url TEXT NOT NULL,
     model_name TEXT NOT NULL,
     transcription_model TEXT NOT NULL DEFAULT '',
     api_key_ciphertext TEXT NOT NULL,
     api_key_hint TEXT NOT NULL DEFAULT '',
     status TEXT NOT NULL DEFAULT 'active',
     is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
     revision INTEGER NOT NULL DEFAULT 1,
     created_at DATETIME NOT NULL,
     updated_at DATETIME NOT NULL
 );

 CREATE INDEX IF NOT EXISTS idx_model_providers_default
     ON model_providers(status, is_default, updated_at DESC);

 CREATE TABLE IF NOT EXISTS admin_audit_events (
     id TEXT PRIMARY KEY,
     actor_user_id TEXT NOT NULL REFERENCES users(id),
     target_user_id TEXT REFERENCES users(id),
     action TEXT NOT NULL,
     detail_json TEXT NOT NULL DEFAULT '{}',
     created_at DATETIME NOT NULL
 );

 CREATE INDEX IF NOT EXISTS idx_admin_audit_actor
     ON admin_audit_events(actor_user_id, created_at DESC);
 CREATE INDEX IF NOT EXISTS idx_admin_audit_target
     ON admin_audit_events(target_user_id, created_at DESC);

 INSERT INTO schema_migrations(version, name, applied_at)
 VALUES (3, 'admin_platform', CURRENT_TIMESTAMP);

