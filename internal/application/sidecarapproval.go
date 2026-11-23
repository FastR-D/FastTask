package application

import (
	"context"
	"encoding/json"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

func newAgentRunJob(run *persistence.AgentRun, now time.Time) persistence.AgentJob {
	return persistence.AgentJob{
		ID: persistence.NewID("job"), UserID: run.UserID, Type: "agent_run", Status: "queued",
		SubjectType: "agent_run", SubjectID: run.ID, InputJSON: "{}", MaxAttempts: 1,
		RunAfter: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func (s *AgentService) prepareSidecarRun(ctx context.Context, run *persistence.AgentRun) error {
	var count int64
	if err := s.store.DB.WithContext(ctx).Model(&persistence.AgentMessage{}).
		Where("run_id = ? AND user_id = ? AND role = 'assistant'", run.ID, run.UserID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return s.BeginHarnessRun(ctx, run.UserID, run.ID)
	}
	return nil
}

// parkSidecarRun releases the host and the job that led up to a human decision.
// An early receipt can already have assigned a replacement job. The old
// execution has then lost authority to report further state for this run.
func (s *AgentService) parkSidecarRun(ctx context.Context, userID, runID, jobID string) (bool, error) {
	parked := false
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		run, err := repo.GetRun(ctx, userID, runID)
		if err != nil {
			return err
		}
		parked = run.JobID != jobID || run.Status == persistence.RunAwaitingApproval
		if run.JobID != jobID || run.Status != persistence.RunAwaitingApproval || run.CancelRequested {
			return nil
		}
		if _, err := repo.RevokeHarnessTokensForRun(ctx, run.ID); err != nil {
			return err
		}
		if err := repo.UpdateRun(ctx, userID, run.ID, map[string]any{"job_id": "", "model_inflight": ""}); err != nil {
			return err
		}
		run.JobID = ""
		_, err = s.updateSidecarApproval(ctx, tx, run)
		return err
	})
	return parked, err
}

func (s *AgentService) continueSidecarApproval(ctx context.Context, userID, runID string) (bool, error) {
	// The decision is already durable. Complete the queue handoff even if the
	// receipt request disconnects. Reconciliation also covers a process restart.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	follow := false
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		run, err := s.repo.WithTx(tx).GetRun(ctx, userID, runID)
		if err != nil {
			return err
		}
		follow = run.Status == persistence.RunQueued || run.Status == persistence.RunRunning
		if run.Status != persistence.RunAwaitingApproval || run.CancelRequested {
			return nil
		}
		follow, err = s.updateSidecarApproval(ctx, tx, run)
		return err
	})
	return follow, err
}

// updateSidecarApproval queues at most one continuation and publishes the
// associated transcript in the same transaction as that queue transition.
func (s *AgentService) updateSidecarApproval(ctx context.Context, tx *gorm.DB, run *persistence.AgentRun) (bool, error) {
	var pending int64
	if err := tx.Table("agent_message_parts p").Joins("JOIN agent_messages m ON m.id = p.message_id").
		Where("m.run_id = ? AND p.user_id = ? AND p.approval_status = ?", run.ID, run.UserID, protocol.ApprovalPending).
		Count(&pending).Error; err != nil {
		return false, err
	}
	repo := s.repo.WithTx(tx)
	queued := pending == 0 && run.JobID == ""
	if queued {
		job := newAgentRunJob(run, persistence.Now())
		if err := tx.Create(&job).Error; err != nil {
			return false, err
		}
		if _, err := repo.RevokeHarnessTokensForRun(ctx, run.ID); err != nil {
			return false, err
		}
		// Keep approval_started_at until the new host starts. Time spent in the
		// continuation queue is excluded from the active execution budget too.
		if err := repo.UpdateRun(ctx, run.UserID, run.ID, map[string]any{
			"status": persistence.RunQueued, "job_id": job.ID, "model_inflight": "",
		}); err != nil {
			return false, err
		}
	}
	return pending == 0, s.publishSidecarApproval(ctx, tx, run, queued)
}

func (s *AgentService) publishSidecarApproval(ctx context.Context, tx *gorm.DB, run *persistence.AgentRun, queued bool) error {
	repo := s.repo.WithTx(tx)
	sess, err := s.harnessLifecycle.loadRunSessionFrom(ctx, repo, run)
	if err != nil || sess == nil {
		return err
	}
	state := sess.state
	state.IsRunning = queued
	state.FastTask.AwaitingApproval = !queued
	if !queued {
		state.Messages[sess.assistantIdx].Status = protocol.RequiresActionStatus()
	}
	thread, err := repo.GetThread(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return err
	}
	state.FastTask.ActiveGoalID = derefString(thread.GoalID)
	state.FastTask.PendingProposals, err = pendingProposalsTx(tx, run.UserID, run.ThreadID)
	if err != nil {
		return err
	}
	values := []struct {
		path  []string
		value any
	}{
		{[]string{"messages"}, state.Messages},
		{protocol.FastTaskPath(), state.FastTask},
		{protocol.IsRunningPath(), state.IsRunning},
	}
	var ops []protocol.Operation
	for _, field := range values {
		op, err := protocol.Set(field.path, field.value)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	chunk, err := json.Marshal(protocol.UpdateState(ops...))
	if err != nil {
		return err
	}
	_, err = repo.AppendChunk(ctx, run.UserID, run.ID, string(chunk))
	return err
}

// reconcileSidecarApprovals catches a receipt committed just before a crash or
// a failed queue write. Pending proposals themselves never need a scheduler tick.
func (s *AgentService) reconcileSidecarApprovals(ctx context.Context) error {
	var ready []persistence.AgentRun
	err := s.store.DB.WithContext(ctx).Where("harness_mode = ? AND status = ? AND cancel_requested = 0 AND job_id = ''", HarnessModeSidecar, persistence.RunAwaitingApproval).
		Where(`NOT EXISTS (SELECT 1 FROM agent_message_parts p JOIN agent_messages m ON m.id = p.message_id
			WHERE m.run_id = agent_runs.id AND p.approval_status = 'pending')`).Find(&ready).Error
	if err != nil {
		return err
	}
	for _, run := range ready {
		if _, err := s.continueSidecarApproval(ctx, run.UserID, run.ID); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSidecarApprovals is called before workers claim jobs after startup.
// A still-pending proposal with a verified legacy approval timeout can safely
// be offered again. Its normal receipt path still checks the business revision.
func (s *AgentService) RestoreSidecarApprovals(ctx context.Context) error {
	var waiting []persistence.AgentRun
	db := s.store.DB.WithContext(ctx)
	if err := db.Where("harness_mode = ? AND status = ? AND job_id <> ''", HarnessModeSidecar, persistence.RunAwaitingApproval).Find(&waiting).Error; err != nil {
		return err
	}
	for _, run := range waiting {
		if _, err := s.parkSidecarRun(ctx, run.UserID, run.ID, run.JobID); err != nil {
			return err
		}
	}
	var expired []persistence.AgentRun
	err := db.Where("harness_mode = ? AND status = ? AND cancel_requested = 0 AND approval_started_at IS NOT NULL", HarnessModeSidecar, persistence.RunFailed).
		Where("(error_code = 'APPROVAL_TIMEOUT' OR (error_code = 'PROVIDER_ERROR' AND error_message LIKE ?))", `%sidecar call:%/run%Client.Timeout exceeded%`).
		Where(`EXISTS (SELECT 1 FROM agent_messages m JOIN agent_message_parts p ON p.message_id = m.id JOIN proposals pr ON pr.id = p.proposal_id
			WHERE m.run_id = agent_runs.id AND p.user_id = agent_runs.user_id AND pr.user_id = agent_runs.user_id AND p.approval_status = 'pending' AND pr.status = 'pending')`).
		Order("created_at DESC").Find(&expired).Error
	if err != nil {
		return err
	}
	for _, run := range expired {
		if err := s.restoreExpiredSidecarApproval(ctx, run); err != nil {
			return err
		}
	}
	// Retire a superseded job from a previous process so it cannot block the
	// recovered continuation behind a lease that nobody will renew.
	now := persistence.Now()
	err = db.Model(&persistence.AgentJob{}).Where("type = 'agent_run' AND status = 'running'").
		Where(`EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = agent_jobs.subject_id AND r.user_id = agent_jobs.user_id
			AND r.harness_mode = 'sidecar' AND r.job_id <> agent_jobs.id)`).
		Updates(map[string]any{"status": "succeeded", "finished_at": now, "updated_at": now, "locked_until": nil,
			"locked_by": "", "run_token_hash": "", "revision": gorm.Expr("revision + 1"),
			"output_json": `{"status":"superseded"}`}).Error
	if err != nil {
		return err
	}
	return s.reconcileSidecarApprovals(ctx)
}

func (s *AgentService) restoreExpiredSidecarApproval(ctx context.Context, run persistence.AgentRun) error {
	return s.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)
		if _, err := repo.GetActiveRunByThread(ctx, run.UserID, run.ThreadID); !persistence.IsNotFound(err) {
			return err
		}
		if _, err := repo.RevokeHarnessTokensForRun(ctx, run.ID); err != nil {
			return err
		}
		if err := repo.UpdateRun(ctx, run.UserID, run.ID, map[string]any{
			"status": persistence.RunAwaitingApproval, "error_code": "", "error_message": "", "finished_at": nil,
			"job_id": "", "model_inflight": "",
		}); err != nil {
			return err
		}
		return s.publishSidecarApproval(ctx, tx, &run, false)
	})
}

func cancelPendingSidecarApprovals(tx *gorm.DB, run *persistence.AgentRun) ([]persistence.AgentMessagePart, error) {
	var parts []persistence.AgentMessagePart
	if err := tx.Model(&persistence.AgentMessagePart{}).Joins("JOIN agent_messages m ON m.id = agent_message_parts.message_id").
		Where("m.run_id = ? AND agent_message_parts.user_id = ? AND approval_status = ?", run.ID, run.UserID, protocol.ApprovalPending).Find(&parts).Error; err != nil {
		return nil, err
	}
	now := persistence.Now()
	for i := range parts {
		part := &parts[i]
		if part.ProposalID != nil {
			if err := tx.Model(&persistence.Proposal{}).Where("id = ? AND user_id = ? AND status = 'pending'", *part.ProposalID, run.UserID).
				Updates(map[string]any{"status": "cancelled", "updated_at": now, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return nil, err
			}
		}
		part.ApprovalStatus, part.IsError = "cancelled", true
		part.ResultJSON = `{"status":"cancelled","error":"本轮已取消，提案未应用。"}`
		if err := tx.Model(part).Updates(map[string]any{"approval_status": part.ApprovalStatus, "is_error": part.IsError,
			"result_json": part.ResultJSON, "updated_at": now}).Error; err != nil {
			return nil, err
		}
	}
	return parts, nil
}
