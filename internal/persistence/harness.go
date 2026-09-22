package persistence

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Harness token and run-driving queries (doc/harness.md §10, §11).
//
// Two rules shape this file:
//
//  1. A token is looked up by hash only — it IS the identity of the caller — but
//     every write it authorizes still goes through the user-scoped repository
//     methods, so a stolen token cannot widen its own scope.
//  2. The loss detector is the single place that decides a host is gone. It reads
//     the token's heartbeat, never the run's, because a run may exist before any
//     host picks it up.

// CreateHarnessToken stores a newly issued token. The plaintext never reaches the
// database; callers pass the hash.
func (r *AgentRepository) CreateHarnessToken(ctx context.Context, token *AgentHarnessToken) error {
	if token.ID == "" {
		token.ID = NewID("htok")
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = Now()
	}
	return r.db(ctx).Create(token).Error
}

// GetHarnessTokenByHash resolves a presented token. It returns
// gorm.ErrRecordNotFound for an unknown hash; expiry and revocation are the
// caller's decision so the two can be reported differently.
func (r *AgentRepository) GetHarnessTokenByHash(ctx context.Context, tokenHash string) (*AgentHarnessToken, error) {
	var token AgentHarnessToken
	if err := r.db(ctx).Where("token_hash = ?", tokenHash).First(&token).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

// ActiveHarnessTokenForRun returns the live token of a run, if any. Issuance
// revokes the previous one so a run has exactly one authorized host.
func (r *AgentRepository) ActiveHarnessTokenForRun(ctx context.Context, runID string) (*AgentHarnessToken, error) {
	var token AgentHarnessToken
	err := r.db(ctx).
		Where("run_id = ? AND revoked_at IS NULL AND expires_at > ?", runID, Now()).
		Order("created_at DESC").First(&token).Error
	if err != nil {
		return nil, err
	}
	return &token, nil
}

// GetHarnessToken returns a token by id. The heartbeat path needs the row's own
// expiry to decide whether to rotate it (doc/harness.md §10.4).
func (r *AgentRepository) GetHarnessToken(ctx context.Context, tokenID string) (*AgentHarnessToken, error) {
	var token AgentHarnessToken
	if err := r.db(ctx).Where("id = ?", tokenID).First(&token).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

// BeatHarnessToken records that a host is alive. It deliberately does NOT extend the
// expiry: a token's life is fixed at issuance, and a host that needs more time is
// handed a new token instead (§10.4). Rotating beats extending because a stolen token
// then stops working on its own schedule.
//
// It reports whether the token is still live, so one that expired or was revoked while
// the host was away is not silently accepted.
func (r *AgentRepository) BeatHarnessToken(ctx context.Context, tokenID string, now time.Time) (bool, error) {
	res := r.db(ctx).Model(&AgentHarnessToken{}).
		Where("id = ? AND revoked_at IS NULL AND expires_at > ?", tokenID, now).
		Updates(map[string]any{"last_heartbeat_at": now})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// RevokeHarnessToken invalidates one token, used when a rotation replaces it.
func (r *AgentRepository) RevokeHarnessToken(ctx context.Context, tokenID string, now time.Time) error {
	return r.db(ctx).Model(&AgentHarnessToken{}).
		Where("id = ? AND revoked_at IS NULL", tokenID).
		Update("revoked_at", now).Error
}

// RevokeHarnessTokensForRun invalidates every live token of a run. Called when the
// run reaches a terminal state (§10.2) and when a new token replaces an old one.
func (r *AgentRepository) RevokeHarnessTokensForRun(ctx context.Context, runID string) (int64, error) {
	now := Now()
	res := r.db(ctx).Model(&AgentHarnessToken{}).
		Where("run_id = ? AND revoked_at IS NULL", runID).
		Updates(map[string]any{"revoked_at": now})
	return res.RowsAffected, res.Error
}

// DeleteExpiredHarnessTokens drops tokens that can no longer be used, keeping the
// table from growing without bound. Revoked rows are removed once their run is
// terminal, which the caller has already established.
func (r *AgentRepository) DeleteExpiredHarnessTokens(ctx context.Context, before time.Time) (int64, error) {
	res := r.db(ctx).
		Where("expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)", before, before).
		Delete(&AgentHarnessToken{})
	return res.RowsAffected, res.Error
}

// StaleHarnessRun is a harness-driven run whose host stopped answering. Reason
// distinguishes "never picked up" from "lost mid-run" so the operator log says
// which happened.
type StaleHarnessRun struct {
	RunID    string
	UserID   string
	ThreadID string
	Status   string
	Reason   string
	// LastBeat is the token's last heartbeat as SQLite stored it. It is a string because the
	// column is a COALESCE expression, which the driver cannot type as DATETIME; it is diagnostic
	// only, so nothing parses it.
	LastBeat string
}

// ListStaleHarnessRuns finds the runs the loss detector must interrupt
// (doc/harness.md §10.4, §11):
//
//   - a run with a live token whose last heartbeat (or issuance, when it never
//     beat) is older than heartbeatCutoff;
//   - a run no host ever asked for a token for, older than pickupCutoff.
//
// Only harness-driven runs are considered: an in-process run has no host to beat
// and is bounded by the Worker instead.
func (r *AgentRepository) ListStaleHarnessRuns(ctx context.Context, heartbeatCutoff, pickupCutoff time.Time) ([]StaleHarnessRun, error) {
	var stale []StaleHarnessRun
	err := r.db(ctx).Raw(`
		SELECT r.id AS run_id, r.user_id AS user_id, r.thread_id AS thread_id, r.status AS status,
		       'HEARTBEAT_LOST' AS reason,
		       COALESCE(t.last_heartbeat_at, t.created_at) AS last_beat
		FROM agent_runs r
		JOIN agent_harness_tokens t ON t.run_id = r.id AND t.revoked_at IS NULL
		WHERE r.harness_mode <> ''
		  AND r.status IN ('queued', 'running', 'awaiting_approval', 'cancelling')
		  AND COALESCE(t.last_heartbeat_at, t.created_at) < ?
		UNION ALL
		SELECT r.id, r.user_id, r.thread_id, r.status,
		       'HARNESS_NEVER_STARTED', r.created_at
		FROM agent_runs r
		WHERE r.harness_mode <> ''
		  AND r.status IN ('queued', 'running')
		  AND r.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM agent_harness_tokens t WHERE t.run_id = r.id)`,
		heartbeatCutoff, pickupCutoff).
		Scan(&stale).Error
	if err != nil {
		return nil, err
	}
	return stale, nil
}

// InterruptRuns moves still-active runs to interrupted in one statement. It is the
// periodic half of the recovery pair whose other half is MarkInterruptedRuns at
// process start (doc/harness.md §11).
func (r *AgentRepository) InterruptRuns(ctx context.Context, runIDs []string, code, message string) (int64, error) {
	if len(runIDs) == 0 {
		return 0, nil
	}
	now := Now()
	res := r.db(ctx).Model(&AgentRun{}).
		Where("id IN ? AND status IN ?", runIDs, []string{RunQueued, RunRunning, RunAwaitingApproval, RunCancelling}).
		Updates(map[string]any{
			"status": RunInterrupted, "error_code": code, "error_message": message,
			"finished_at": now, "updated_at": now, "revision": gorm.Expr("revision + 1"),
		})
	return res.RowsAffected, res.Error
}

// ListCancellingRunsSince returns runs that have been sitting in cancelling since
// before the given instant. Cancellation is best effort (§5.2): when the host does
// not confirm it, the reaper finishes the transition so the user is not left with a
// run that never ends.
func (r *AgentRepository) ListCancellingRunsSince(ctx context.Context, before time.Time) ([]AgentRun, error) {
	var runs []AgentRun
	err := r.db(ctx).
		Where("status = ? AND updated_at < ?", RunCancelling, before).
		Find(&runs).Error
	return runs, err
}

// CountModelCall registers one proxied model call and returns the new total. The count is
// what enforces the turn budget now that the loop runs in a host (doc/harness.md §1.1): the
// proxy refuses the call that would exceed it.
func (r *AgentRepository) CountModelCall(ctx context.Context, userID, runID string) (int, error) {
	res := r.db(ctx).Model(&AgentRun{}).
		Where("id = ? AND user_id = ?", runID, userID).
		Updates(map[string]any{"model_calls": gorm.Expr("model_calls + 1"), "updated_at": Now()})
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, gorm.ErrRecordNotFound
	}
	var run AgentRun
	if err := r.db(ctx).Select("model_calls").Where("id = ? AND user_id = ?", runID, userID).First(&run).Error; err != nil {
		return 0, err
	}
	return run.ModelCalls, nil
}

// RequestRunCancellation flags a run for cancellation and moves it to cancelling.
// It only touches runs that are still active, so cancelling a finished run is a
// no-op rather than a resurrection.
func (r *AgentRepository) RequestRunCancellation(ctx context.Context, userID, runID string) (bool, error) {
	now := Now()
	res := r.db(ctx).Model(&AgentRun{}).
		Where("id = ? AND user_id = ? AND status IN ?", runID, userID, []string{RunQueued, RunRunning, RunAwaitingApproval}).
		Updates(map[string]any{
			"cancel_requested": true, "status": RunCancelling,
			"updated_at": now, "revision": gorm.Expr("revision + 1"),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// AddApprovalWait records time spent waiting for a user decision so the wall clock
// check can subtract it (doc/harness.md §7).
func (r *AgentRepository) AddApprovalWait(ctx context.Context, userID, runID string, waited time.Duration) error {
	res := r.db(ctx).Model(&AgentRun{}).
		Where("id = ? AND user_id = ?", runID, userID).
		Updates(map[string]any{
			"approval_wait_ms": gorm.Expr("approval_wait_ms + ?", waited.Milliseconds()),
			"updated_at":       Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
