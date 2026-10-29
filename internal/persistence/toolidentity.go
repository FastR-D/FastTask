package persistence

import "context"

// FinalizeToolCall marks arguments complete only after the proxy finished the
// model response. A replay can never rewrite a claimed call's arguments.
func (r *AgentRepository) FinalizeToolCall(ctx context.Context, userID, partID string, modelTurn int, argsJSON string) error {
	return r.db(ctx).Model(&AgentMessagePart{}).
		Where("id = ? AND user_id = ? AND tool_model_turn = ? AND tool_execution_claimed = 0 AND tool_args_ready = 0", partID, userID, modelTurn).
		Updates(map[string]any{"args_json": argsJSON, "tool_args_ready": true, "updated_at": Now()}).Error
}

// ClaimToolCall is a single compare-and-set in SQLite. The call id (and part id)
// is the stable execution identity across HTTP retries and server restarts.
// An interrupted execution remains claimed: its external effect is unknown and
// must be reconciled, never silently executed a second time.
func (r *AgentRepository) ClaimToolCall(ctx context.Context, userID, runID, messageID, partID string, turn int, tokens ...string) (bool, error) {
	q := r.db(ctx).Model(&AgentMessagePart{}).
		Where("id = ? AND user_id = ? AND message_id = ? AND type = 'tool-call' AND tool_model_turn = ? AND tool_args_ready = 1 AND tool_execution_claimed = 0 AND (result_json IS NULL OR result_json = '')", partID, userID, messageID, turn).
		Where("EXISTS (SELECT 1 FROM agent_runs WHERE id = ? AND user_id = ? AND model_calls = ? AND cancel_requested = 0 AND status IN ?)", runID, userID, turn, []string{RunRunning, RunAwaitingApproval})
	if len(tokens) > 0 && tokens[0] != "" {
		q = q.Where("EXISTS (SELECT 1 FROM agent_harness_tokens WHERE id = ? AND run_id = ? AND expires_at > ? AND (revoked_at IS NULL OR rotation_until > ?))", tokens[0], runID, Now(), Now())
	}
	res := q.Updates(map[string]any{"tool_execution_claimed": true, "updated_at": Now()})
	return res.RowsAffected == 1, res.Error
}
