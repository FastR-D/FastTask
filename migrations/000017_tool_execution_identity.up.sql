-- Preserve historical calls and results verbatim. Only newly transcribed calls
-- have an explicit model turn and complete arguments, and may be claimed.
ALTER TABLE agent_message_parts ADD COLUMN tool_model_turn INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_message_parts ADD COLUMN tool_args_ready INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_message_parts ADD COLUMN tool_execution_claimed INTEGER NOT NULL DEFAULT 0;
INSERT INTO schema_migrations(version,name,applied_at) VALUES (17,'tool_execution_identity',CURRENT_TIMESTAMP);
