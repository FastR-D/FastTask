ALTER TABLE agent_runs ADD COLUMN started_at DATETIME;
ALTER TABLE agent_runs ADD COLUMN approval_started_at DATETIME;
ALTER TABLE agent_runs ADD COLUMN model_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN model_inflight TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN usage_json TEXT NOT NULL DEFAULT '';
UPDATE agent_runs SET started_at = created_at WHERE status <> 'queued';
INSERT INTO schema_migrations(version,name,applied_at) VALUES(22,'harness_execution',CURRENT_TIMESTAMP);
