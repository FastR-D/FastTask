-- Optional FastCAS state. Existing users and credentials retain their IDs.
ALTER TABLE sessions ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local';
ALTER TABLE sessions ADD COLUMN cas_issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN cas_sid TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN cas_link_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN cas_link_version INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_sessions_cas_link ON sessions(cas_issuer,cas_link_id,auth_source);
CREATE TABLE fastcas_transactions (state TEXT PRIMARY KEY, payload TEXT NOT NULL, expires_at DATETIME NOT NULL);
CREATE TABLE fastcas_links (id TEXT PRIMARY KEY, issuer TEXT NOT NULL, client_id TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id), subject TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('prepared','active','revoked')), version INTEGER NOT NULL, verified_at DATETIME NOT NULL);
CREATE UNIQUE INDEX idx_fastcas_user_live ON fastcas_links(issuer,user_id) WHERE state != 'revoked';
CREATE UNIQUE INDEX idx_fastcas_subject_live ON fastcas_links(issuer,subject) WHERE state != 'revoked';
CREATE TABLE fastcas_events (issuer TEXT NOT NULL,id TEXT NOT NULL,processed_at DATETIME NOT NULL,PRIMARY KEY(issuer,id));
INSERT INTO schema_migrations(version,name,applied_at) VALUES(9,'fastcas',CURRENT_TIMESTAMP);
