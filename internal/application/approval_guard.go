package application

import (
	"context"
	"encoding/json"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type approvalRunKey struct{}
type approvalPartKey struct{}

// The cancellation check shares the business write lock. A cancelled receipt
// cannot pass a preflight check and later apply under a different transaction.
func guardApprovalTx(ctx context.Context, tx *gorm.DB, userID string) error {
	id, _ := ctx.Value(approvalRunKey{}).(string)
	if id == "" {
		return nil
	}
	var run persistence.AgentRun
	if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
		return err
	}
	if run.CancelRequested || (run.Status != persistence.RunAwaitingApproval && run.Status != persistence.RunInterrupted) {
		return ErrApprovalNotAwaiting
	}
	partID, _ := ctx.Value(approvalPartKey{}).(string)
	if partID != "" {
		var part persistence.AgentMessagePart
		if err := tx.Where("id = ? AND user_id = ?", partID, userID).First(&part).Error; err != nil {
			return err
		}
		if part.ApprovalStatus != "pending" {
			return ErrApprovalDuplicate
		}
	}
	return nil
}

// Persist the durable truth in the same transaction as domain changes. Detailed
// presentation fields can be enriched afterwards without losing an applied receipt.
func recordAppliedApprovalTx(tx *gorm.DB, userID, proposalID, goalID string) error {
	partID, _ := tx.Statement.Context.Value(approvalPartKey{}).(string)
	if partID == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"decision": "approve", "applied": true, "proposal_id": proposalID, "goal_id": goalID})
	if err != nil {
		return err
	}
	result := tx.Model(&persistence.AgentMessagePart{}).Where("id = ? AND user_id = ? AND approval_status = 'pending'", partID, userID).Updates(map[string]any{"approval_status": "approved", "result_json": string(payload), "is_error": false, "updated_at": persistence.Now()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrApprovalDuplicate
	}
	return nil
}
