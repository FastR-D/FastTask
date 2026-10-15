--migration:no-transaction
-- A new-goal proposal belongs to a user before it can reference a real goal.
-- The runner uses one connection with foreign keys disabled for this rewrite.
BEGIN IMMEDIATE;
CREATE TABLE proposals_v14 (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    goal_id TEXT REFERENCES goals(id),
    kind TEXT NOT NULL DEFAULT 'task_tree_patch',
    job_id TEXT,
    base_revision INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    instruction TEXT NOT NULL DEFAULT '',
    patch_json TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
INSERT INTO proposals_v14 (id,user_id,goal_id,kind,job_id,base_revision,status,instruction,patch_json,revision,created_at,updated_at)
SELECT id,user_id,goal_id,'task_tree_patch',job_id,base_revision,status,instruction,patch_json,revision,created_at,updated_at FROM proposals;
DROP TABLE proposals;
ALTER TABLE proposals_v14 RENAME TO proposals;
CREATE INDEX idx_proposals_user_status ON proposals(user_id,status,created_at);
INSERT INTO schema_migrations(version,name,applied_at) VALUES (14,'goal_proposals',CURRENT_TIMESTAMP);
COMMIT;
