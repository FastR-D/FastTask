ALTER TABLE daily_plans ADD COLUMN manual_items_removed INTEGER NOT NULL DEFAULT 0 CHECK (manual_items_removed IN (0,1));
INSERT INTO schema_migrations(version,name,applied_at) VALUES (21,'daily_plan_item_deletions',CURRENT_TIMESTAMP);
