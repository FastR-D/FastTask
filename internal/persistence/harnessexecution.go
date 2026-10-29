package persistence

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"time"
)

var ErrModelBusy = errors.New("model attempt already active or budget exhausted")

func (r *AgentRepository) AcquireModelAttempt(ctx context.Context, userID, runID string, maxAttempts, maxTurns int, tokens ...string) (string, error) {
	id := NewID("attempt")
	q := r.db(ctx).Model(&AgentRun{}).Where("id = ? AND user_id = ? AND status = ? AND cancel_requested = 0 AND model_inflight = ''", runID, userID, RunRunning)
	if len(tokens) > 0 && tokens[0] != "" {
		q = q.Where("EXISTS (SELECT 1 FROM agent_harness_tokens WHERE id = ? AND run_id = ? AND expires_at > ? AND (revoked_at IS NULL OR rotation_until > ?))", tokens[0], runID, Now(), Now())
	}
	if maxAttempts > 0 {
		q = q.Where("model_attempts < ?", maxAttempts)
	}
	if maxTurns > 0 {
		q = q.Where("model_calls < ?", maxTurns)
	}
	res := q.Updates(map[string]any{"model_verified_stop": "", "model_inflight": id, "model_attempts": gorm.Expr("model_attempts + 1"), "started_at": gorm.Expr("COALESCE(started_at, ?)", Now())})
	if res.Error != nil {
		return "", res.Error
	}
	if res.RowsAffected != 1 {
		return "", ErrModelBusy
	}
	return id, nil
}
func (r *AgentRepository) ReleaseModelAttempt(ctx context.Context, userID, runID, id string) error {
	return r.db(ctx).Model(&AgentRun{}).Where("id = ? AND user_id = ? AND model_inflight = ?", runID, userID, id).Update("model_inflight", "").Error
}

// SetApprovalStatus measures the union of waiting intervals once per run.
// Call under the same transaction that changes pending proposal state.
func (r *AgentRepository) SetApprovalStatus(ctx context.Context, userID, runID, status string, now time.Time) error {
	var run AgentRun
	if err := r.db(ctx).Where("id = ? AND user_id = ?", runID, userID).First(&run).Error; err != nil {
		return err
	}
	if run.CancelRequested || !containsActiveStatus(run.Status) {
		return nil
	}
	fields := map[string]any{"status": status, "updated_at": now, "revision": gorm.Expr("revision + 1")}
	if status == RunAwaitingApproval && run.ApprovalStartedAt == nil {
		fields["approval_started_at"] = now
	}
	if status != RunAwaitingApproval && run.ApprovalStartedAt != nil {
		fields["approval_wait_ms"] = run.ApprovalWaitMs + max(int64(0), now.Sub(*run.ApprovalStartedAt).Milliseconds())
		fields["approval_started_at"] = nil
	}
	return r.db(ctx).Model(&AgentRun{}).Where("id = ? AND user_id = ? AND revision = ?", runID, userID, run.Revision).Updates(fields).Error
}
func containsActiveStatus(status string) bool {
	for _, s := range activeRunStatuses {
		if s == status {
			return true
		}
	}
	return false
}
