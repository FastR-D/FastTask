ALTER TABLE agent_harness_tokens ADD COLUMN generation TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_harness_tokens ADD COLUMN next_token_cipher TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_harness_tokens ADD COLUMN rotation_until DATETIME;
UPDATE agent_harness_tokens SET generation = id;
ALTER TABLE agent_runs ADD COLUMN model_verified_stop TEXT NOT NULL DEFAULT '';
INSERT INTO schema_migrations(version,name,applied_at) VALUES(23,'harness_token_rotation',CURRENT_TIMESTAMP);
