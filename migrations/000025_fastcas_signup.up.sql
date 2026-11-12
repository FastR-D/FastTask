CREATE TABLE IF NOT EXISTS fastcas_signups (
 binding TEXT PRIMARY KEY,
 issuer TEXT NOT NULL,
 subject TEXT NOT NULL,
 email TEXT NOT NULL,
 name TEXT NOT NULL,
 prepared TEXT NOT NULL DEFAULT '',
 expires_at DATETIME NOT NULL
);
INSERT INTO schema_migrations(version,name,applied_at) VALUES(25,'fastcas_signup',CURRENT_TIMESTAMP);
