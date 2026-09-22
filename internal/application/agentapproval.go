package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Approval receipt errors, mapped to HTTP statuses by the transport layer (§7.4).
var (
	// ErrApprovalDuplicate is a repeated receipt for an already-resolved tool call
	// (§7.4: "重复回执要幂等 ... 返回 409").
	ErrApprovalDuplicate = errors.New("approval receipt already resolved")
	// ErrApprovalNotAwaiting means the target run is not paused for approval.
	ErrApprovalNotAwaiting = errors.New("run is not awaiting approval")
)

// approvalDecision is the add-tool-result payload for a proposal (§7.1): the
// frontend sends {"decision":"approve"} or {"decision":"reject","reason":"..."}
// via addToolResult, never via assistant-ui's approval API (ADR-0002 §3.1).
type approvalDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// firstToolResult returns the first add-tool-result command carrying a toolCallId.
func firstToolResult(commands []Command) (Command, bool) {
	for _, command := range commands {
		if command.Type == "add-tool-result" && strings.TrimSpace(command.ToolCallID) != "" {
			return command, true
		}
	}
	return Command{}, false
}

// ResolveApproval handles an add-tool-result receipt for a pending proposal (§7).
//
// Per §7.3 the proposal is applied SYNCHRONOUSLY in the request path, so the user
// immediately sees 412 (base revision moved) or 409 rather than waiting for a
// Worker round-trip. On success it re-queues the SAME run with a NEW job (§7.2,
// §4.0) and returns the resume cursor so the client streams only the continuation.
//
// Ordering (§7.3): auth (done by the caller) → apply/reject → record tool result
// → wake Worker. On apply failure the run is NOT resumed and the error is returned.
func (s *AgentService) ResolveApproval(ctx context.Context, userID string, cmd Command) (SubmitResult, error) {
	toolCallID := strings.TrimSpace(cmd.ToolCallID)
	if toolCallID == "" {
		return SubmitResult{}, ErrEmptyCommand
	}

	// Locate the tool-call part, user-scoped: a cross-user or missing toolCallId is
	// indistinguishable → not-found → 404 (§7.4).
	part, err := s.repo.GetPartByToolCallID(ctx, userID, toolCallID)
	if err != nil {
		return SubmitResult{}, err
	}
	if part.Type != "tool-call" || part.ApprovalStatus == "" {
		return SubmitResult{}, ErrApprovalNotAwaiting
	}
	// Idempotency: a second receipt for the same toolCallId is a conflict (§7.4).
	if part.ApprovalStatus != protocol.ApprovalPending {
		return SubmitResult{}, ErrApprovalDuplicate
	}

	msg, err := s.repo.GetMessage(ctx, userID, part.MessageID)
	if err != nil {
		return SubmitResult{}, err
	}
	run, err := s.repo.GetRun(ctx, userID, msg.RunID)
	if err != nil {
		return SubmitResult{}, err
	}
	if run.Status != persistence.RunAwaitingApproval {
		return SubmitResult{}, ErrApprovalNotAwaiting
	}

	var decision approvalDecision
	if len(cmd.Result) > 0 {
		_ = json.Unmarshal(cmd.Result, &decision)
	}
	decision.Decision = strings.ToLower(strings.TrimSpace(decision.Decision))
	if decision.Decision != "approve" && decision.Decision != "reject" {
		return SubmitResult{}, fmt.Errorf("%w: decision must be approve or reject", ErrEmptyCommand)
	}

	// The client has already rendered chunks [0, CheckpointSeq); the continuation
	// streams from there so no append-text is re-applied (§2.8).
	fromSeq := run.CheckpointSeq

	if decision.Decision == "approve" {
		if part.ProposalID == nil {
			return SubmitResult{}, ErrApprovalNotAwaiting
		}
		var proposal persistence.Proposal
		err := s.store.DB.WithContext(ctx).
			Where("id = ? AND user_id = ? AND status = 'pending'", *part.ProposalID, userID).
			First(&proposal).Error
		if err != nil {
			if persistence.IsNotFound(err) {
				return SubmitResult{}, ErrApprovalDuplicate
			}
			return SubmitResult{}, err
		}
		// Route by the tool that staged the proposal: the task-tree patch applies via
		// ApplyProposal (structural, base-revision guarded); the daily plan applies
		// via ApplyDailyPlanProposal (deterministic top-3 selection, §6).
		var resultJSON []byte
		if part.ToolName == "propose_daily_plan" {
			var payload dailyPlanPayload
			if err := json.Unmarshal([]byte(proposal.PatchJSON), &payload); err != nil {
				s.markProposalConflict(ctx, proposal.ID)
				return SubmitResult{}, fmt.Errorf("%w: malformed daily plan proposal", ErrValidation)
			}
			plan, items, applyErr := s.app.ApplyDailyPlanProposal(ctx, userID, &proposal, payload)
			if applyErr != nil {
				s.markProposalConflict(ctx, proposal.ID)
				if errors.Is(applyErr, ErrRevision) {
					return SubmitResult{}, ErrRevision
				}
				return SubmitResult{}, applyErr
			}
			resultJSON, _ = json.Marshal(map[string]any{
				"decision": "approve", "applied": true, "kind": "daily_plan",
				"plan_id": plan.ID, "revision": plan.CurrentRevision,
				"item_count": len(items), "proposal_id": proposal.ID,
			})
		} else {
			var patches []map[string]any
			if err := json.Unmarshal([]byte(proposal.PatchJSON), &patches); err != nil {
				return SubmitResult{}, err
			}
			// ApplyProposal re-validates ownership, base revision, parent/child and
			// cycles inside its transaction (invariant 4). A moved base revision yields
			// ErrRevision → 412, and the proposal is marked conflict, never overwritten.
			newRevision, applyErr := s.app.ApplyProposal(ctx, userID, &proposal, patches, proposal.BaseRevision)
			if applyErr != nil {
				if errors.Is(applyErr, ErrRevision) {
					s.markProposalConflict(ctx, proposal.ID)
					return SubmitResult{}, ErrRevision
				}
				return SubmitResult{}, applyErr
			}
			resultJSON, _ = json.Marshal(map[string]any{
				"decision": "approve", "applied": true, "kind": "task_tree_patch",
				"tree_revision": newRevision, "proposal_id": proposal.ID,
			})
		}
		if err := s.recordApproval(ctx, userID, part.ID, protocol.ApprovalApproved, string(resultJSON)); err != nil {
			return SubmitResult{}, err
		}
	} else {
		// Reject: the reason is recorded as the tool result so the model sees it on
		// resume and can re-propose (§7: "拒绝是有价值的信号").
		if part.ProposalID != nil {
			var proposal persistence.Proposal
			err := s.store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", *part.ProposalID, userID).First(&proposal).Error
			if err == nil {
				_, _ = s.app.RejectProposal(ctx, userID, proposal.GoalID, proposal.ID, proposal.Revision)
			}
		}
		resultJSON, _ := json.Marshal(map[string]any{
			"decision": "reject", "reason": decision.Reason, "applied": false,
		})
		if err := s.recordApproval(ctx, userID, part.ID, protocol.ApprovalRejected, string(resultJSON)); err != nil {
			return SubmitResult{}, err
		}
	}

	if err := s.requeueRun(ctx, userID, run); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{ThreadID: run.ThreadID, RunID: run.ID, FromSeq: fromSeq}, nil
}

// recordApproval writes the decision onto the tool-call part so the resumed run
// feeds it back to the model and the client renders the resolved approval.
func (s *AgentService) recordApproval(ctx context.Context, userID, partID, status, resultJSON string) error {
	if err := s.repo.UpdatePartResult(ctx, userID, partID, resultJSON, false); err != nil {
		return err
	}
	return s.repo.UpdatePartApproval(ctx, userID, partID, status, nil)
}

// markProposalConflict flags a proposal whose base revision moved, so it can no
// longer be applied and never silently overwrites newer data (§7.4).
func (s *AgentService) markProposalConflict(ctx context.Context, proposalID string) {
	s.store.DB.WithContext(ctx).Model(&persistence.Proposal{}).
		Where("id = ? AND status = 'pending'", proposalID).
		Updates(map[string]any{"status": "conflict", "revision": gorm.Expr("revision + 1"), "updated_at": persistence.Now()})
}

// requeueRun wakes the Worker to continue the SAME run under a NEW job (§7.2):
// awaiting_approval holds no lease, so approval creates a fresh AgentJob while
// reusing the run, assistant message and message index.
func (s *AgentService) requeueRun(ctx context.Context, userID string, run *persistence.AgentRun) error {
	return s.store.Transaction(ctx, func(tx *gorm.DB) error {
		now := persistence.Now()
		job := persistence.AgentJob{
			ID: persistence.NewID("job"), UserID: userID, Type: "agent_run", Status: "queued",
			SubjectType: "agent_run", SubjectID: run.ID, InputJSON: "{}",
			MaxAttempts: 1, RunAfter: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&job).Error; err != nil {
			return err
		}
		return tx.WithContext(ctx).Model(&persistence.AgentRun{}).
			Where("id = ? AND user_id = ?", run.ID, userID).
			Updates(map[string]any{
				"status": persistence.RunRunning, "job_id": job.ID,
				"revision": gorm.Expr("revision + 1"), "updated_at": now,
			}).Error
	})
}
