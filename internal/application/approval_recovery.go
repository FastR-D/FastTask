package application

import (
	"context"
	"encoding/json"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// An explicit user receipt from a restored browser must not turn a parked run
// with no local host back to running. End its drive authority first, atomically
// with token revocation, then let the ordinary approval path validate/apply the
// existing proposal. A concurrent user cancellation wins the conditional write.
func (s *approvalService) detachPendingBrowserApproval(ctx context.Context, userID, runID, partID string) (*persistence.AgentRun, error) {
	var run persistence.AgentRun
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", runID, userID).First(&run).Error; err != nil {
			return err
		}
		if run.HarnessMode != HarnessModeWASM || run.CancelRequested || run.Status != persistence.RunAwaitingApproval {
			return ErrApprovalNotAwaiting
		}
		var pending int64
		if err := tx.Table("agent_message_parts p").Joins("JOIN agent_messages m ON m.id = p.message_id").
			Joins("JOIN proposals proposal ON proposal.id = p.proposal_id").
			Where("p.id = ? AND p.user_id = ? AND m.user_id = ? AND m.run_id = ? AND p.approval_status = ? AND proposal.user_id = ? AND proposal.status = 'pending'", partID, userID, userID, runID, protocol.ApprovalPending, userID).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending != 1 {
			return ErrApprovalNotAwaiting
		}
		now := persistence.Now()
		changed := tx.Model(&persistence.AgentRun{}).
			Where("id = ? AND user_id = ? AND revision = ? AND harness_mode = ? AND status = ? AND cancel_requested = ?", runID, userID, run.Revision, HarnessModeWASM, persistence.RunAwaitingApproval, false).
			Updates(map[string]any{"status": persistence.RunInterrupted, "error_code": persistence.RunErrorHostDetached, "error_message": "pending proposal explicitly settled from a restored browser", "finished_at": now, "updated_at": now, "revision": run.Revision + 1})
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrApprovalNotAwaiting
		}
		repo := s.repo.WithTx(tx)
		if _, err := repo.RevokeHarnessTokensForRun(ctx, runID); err != nil {
			return err
		}
		return repo.SaveThreadCheckpoint(ctx, userID, run.ThreadID, nil, "")
	})
	if err != nil {
		return nil, err
	}
	current, err := s.repo.GetRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}
	if err := s.refreshDetachedApproval(ctx, userID, current, partID); err != nil {
		return nil, err
	}
	return current, nil
}

// Persist the run, proposal part, stream chunks and resume snapshot together.
// A reader must never see a confirmable card before the run accepts its receipt.
func (s *harnessTools) commitPendingApproval(ctx context.Context, sess *runSession, run *persistence.AgentRun, part *persistence.AgentMessagePart, result ToolResult) error {
	return s.svc.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo().WithTx(tx)
		now := persistence.Now()
		changed := tx.Model(&persistence.AgentRun{}).
			Where("id = ? AND user_id = ? AND cancel_requested = ? AND status IN ?", run.ID, run.UserID, false, []string{persistence.RunRunning, persistence.RunAwaitingApproval}).
			Updates(map[string]any{"status": persistence.RunAwaitingApproval, "approval_started_at": gorm.Expr("COALESCE(approval_started_at, ?)", now), "revision": gorm.Expr("revision + 1"), "updated_at": now})
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrRunNotActive
		}
		proposalID := result.Proposal.ProposalID
		resultJSON, isError := resultPayload(result)
		if err := repo.UpdatePartResult(ctx, part.UserID, part.ID, resultJSON, isError); err != nil {
			return err
		}
		if err := repo.UpdatePartApproval(ctx, part.UserID, part.ID, protocol.ApprovalPending, &proposalID); err != nil {
			return err
		}
		// Keep this transaction's stream writer local to the request.
		stream := *sess.sessionStream
		writer := *sess.sink
		writer.repo = repo
		stream.sink = &writer
		current, err := repo.GetRun(ctx, run.UserID, run.ID)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(current.StateJSON), &stream.state); err != nil {
			return err
		}
		assistantIdx := -1
		for index, message := range stream.state.Messages {
			if message.ID == sess.assistantID {
				assistantIdx = index
				break
			}
		}
		if assistantIdx < 0 {
			return ErrValidation
		}
		// A different proposal may have committed since this tool loaded its
		// session. Merge the durable parts under the same write lock.
		parts, err := repo.ListMessageParts(ctx, run.UserID, sess.assistantID)
		if err != nil {
			return err
		}
		for _, stored := range parts {
			for len(stream.state.Messages[assistantIdx].Parts) <= stored.Idx {
				stream.state.Messages[assistantIdx].Parts = append(stream.state.Messages[assistantIdx].Parts, protocol.TextPart(""))
			}
			stream.state.Messages[assistantIdx].Parts[stored.Idx] = partFromRow(&stored)
		}
		stream.state.Messages[assistantIdx].Status = protocol.RequiresActionStatus()
		stream.state.FastTask.PendingProposals, err = pendingProposalsTx(tx, run.UserID, run.ThreadID)
		if err != nil {
			return err
		}
		if err := stream.emitSet(protocol.PartPath(assistantIdx, part.Idx), stream.state.Messages[assistantIdx].Parts[part.Idx]); err != nil {
			return err
		}
		if err := stream.emitSet(protocol.MessageStatusPath(assistantIdx), protocol.RequiresActionStatus()); err != nil {
			return err
		}
		if err := stream.emitSet(protocol.FastTaskPath(), stream.state.FastTask); err != nil {
			return err
		}
		encoded, err := json.Marshal(stream.state)
		if err != nil {
			return err
		}
		return repo.SaveRunState(ctx, run.UserID, run.ID, string(encoded), writer.checkpoint())
	})
}

func pendingProposalsTx(tx *gorm.DB, userID, threadID string) ([]protocol.PendingProposal, error) {
	out := []protocol.PendingProposal{}
	var proposals []persistence.Proposal
	if err := tx.Model(&persistence.Proposal{}).Select("proposals.*").
		Joins("JOIN agent_message_parts p ON p.proposal_id = proposals.id").
		Joins("JOIN agent_messages m ON m.id = p.message_id").
		Where("proposals.user_id = ? AND p.user_id = ? AND m.user_id = ? AND m.thread_id = ? AND proposals.status = 'pending' AND p.approval_status = ?", userID, userID, userID, threadID, protocol.ApprovalPending).
		Order("proposals.created_at DESC").Limit(20).Find(&proposals).Error; err != nil {
		return nil, err
	}
	for _, p := range proposals {
		out = append(out, protocol.PendingProposal{ID: p.ID, GoalID: p.GoalID, BaseRevision: p.BaseRevision, Summary: p.Instruction})
	}
	return out, nil
}

// One resolved tool call does not release other pending decisions in the run.
// The conditional write also prevents a late receipt from reviving a lost host.
func (s *approvalService) setApprovalRunStatus(ctx context.Context, userID, runID string) (string, error) {
	status := ""
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		var run persistence.AgentRun
		if err := tx.Where("id = ? AND user_id = ?", runID, userID).First(&run).Error; err != nil {
			return err
		}
		status = run.Status
		if IsTerminalRunStatus(status) || run.CancelRequested || status == persistence.RunCancelling {
			return nil
		}
		var pending int64
		if err := tx.Model(&persistence.AgentMessagePart{}).
			Joins("JOIN agent_messages m ON m.id = agent_message_parts.message_id").
			Where("agent_message_parts.user_id = ? AND m.user_id = ? AND m.run_id = ? AND approval_status = ?", userID, userID, runID, protocol.ApprovalPending).Count(&pending).Error; err != nil {
			return err
		}
		status = persistence.RunRunning
		if pending > 0 {
			status = persistence.RunAwaitingApproval
		}
		return s.repo.WithTx(tx).SetApprovalStatus(ctx, userID, runID, status, s.svc.harnessLifecycle.clock())
	})
	return status, err
}

func (s *approvalService) refreshDetachedApproval(ctx context.Context, userID string, run *persistence.AgentRun, partID string) error {
	sess, err := s.svc.harnessLifecycle.loadRunSession(ctx, run)
	if err != nil {
		return err
	}
	if sess != nil {
		sess.state.IsRunning = false
		sess.state.Messages[sess.assistantIdx].Status = protocol.IncompleteStatus(protocol.ReasonError)
		if thread, err := s.repo.GetThread(ctx, userID, run.ThreadID); err == nil {
			sess.state.FastTask.ActiveGoalID = derefString(thread.GoalID)
		}
		if part, err := s.repo.GetPart(ctx, userID, partID); err == nil {
			if err := sess.emitSet(protocol.PartPath(sess.assistantIdx, part.Idx), partFromRow(part)); err != nil {
				return err
			}
		}
		if err := sess.emitSet(protocol.FastTaskPath(), sess.state.FastTask); err != nil {
			return err
		}
		if err := sess.emitSet(protocol.IsRunningPath(), false); err != nil {
			return err
		}
		if err := sess.saveState(run.ErrorCode == persistence.RunErrorHostDetached); err != nil {
			return err
		}
	}
	// The old opaque host checkpoint predates this decision. The next run rebuilds
	// context from the complete, preserved transcript instead of missing the result.
	if run.ErrorCode == persistence.RunErrorHostDetached {
		return nil // already cleared atomically before another turn could start
	}
	return s.repo.SaveThreadCheckpoint(ctx, userID, run.ThreadID, nil, "")
}
