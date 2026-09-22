package persistence

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Attachment queries (doc/chat-features.md §4).
//
// Two rules shape this file. Ownership is part of every lookup, so a reference to somebody else's
// attachment reads as "does not exist". And the per-thread totals are computed here rather than tracked
// in a counter, because a counter that drifts would let a thread grow past its quota silently (§4.5).

// CreateAttachment records an uploaded file.
func (r *AgentRepository) CreateAttachment(ctx context.Context, attachment *AgentAttachment) error {
	if attachment.ID == "" {
		attachment.ID = NewID("att")
	}
	if attachment.CreatedAt.IsZero() {
		attachment.CreatedAt = Now()
	}
	return r.db(ctx).Create(attachment).Error
}

// GetAttachment returns one attachment owned by userID, or gorm.ErrRecordNotFound.
func (r *AgentRepository) GetAttachment(ctx context.Context, userID, attachmentID string) (*AgentAttachment, error) {
	var attachment AgentAttachment
	if err := r.db(ctx).Where("id = ? AND user_id = ?", attachmentID, userID).First(&attachment).Error; err != nil {
		return nil, err
	}
	return &attachment, nil
}

// AttachAttachment binds an orphan to the thread and message that carried it, so a thread deletion takes
// its attachments with it (§2.5). An empty id becomes NULL: the empty string is not a row, and a foreign
// key column that holds one is a constraint violation waiting to be found.
func (r *AgentRepository) AttachAttachment(ctx context.Context, userID, attachmentID, threadID, messageID string) error {
	updates := map[string]any{}
	if threadID == "" {
		updates["thread_id"] = nil
	} else {
		updates["thread_id"] = threadID
	}
	if messageID == "" {
		updates["message_id"] = nil
	} else {
		updates["message_id"] = messageID
	}
	res := r.db(ctx).Model(&AgentAttachment{}).
		Where("id = ? AND user_id = ?", attachmentID, userID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteAttachment removes one attachment row and returns it, so the caller can delete the file after the
// row is gone — never before, or a crash in between leaves bytes nobody can reach.
func (r *AgentRepository) DeleteAttachment(ctx context.Context, userID, attachmentID string) (*AgentAttachment, error) {
	attachment, err := r.GetAttachment(ctx, userID, attachmentID)
	if err != nil {
		return nil, err
	}
	if err := r.db(ctx).Where("id = ? AND user_id = ?", attachmentID, userID).Delete(&AgentAttachment{}).Error; err != nil {
		return nil, err
	}
	return attachment, nil
}

// ThreadAttachments lists what a thread holds, for the cascade in §2.5.
func (r *AgentRepository) ThreadAttachments(ctx context.Context, userID, threadID string) ([]AgentAttachment, error) {
	var attachments []AgentAttachment
	err := r.db(ctx).Where("user_id = ? AND thread_id = ?", userID, threadID).Find(&attachments).Error
	return attachments, err
}

// AttachmentTotals counts a thread's attachments and their combined size, which is what the §4.5 quota is
// checked against.
func (r *AgentRepository) AttachmentTotals(ctx context.Context, userID, threadID string) (int64, int64, error) {
	var totals struct {
		Count int64
		Bytes int64
	}
	err := r.db(ctx).Model(&AgentAttachment{}).
		Select("COUNT(*) AS count, COALESCE(SUM(bytes), 0) AS bytes").
		Where("user_id = ? AND thread_id = ?", userID, threadID).
		Scan(&totals).Error
	return totals.Count, totals.Bytes, err
}

// OrphanAttachments lists attachments that were never sent, older than the given instant (§4.5: the
// existing cleanup task reclaims them by TTL).
func (r *AgentRepository) OrphanAttachments(ctx context.Context, before time.Time) ([]AgentAttachment, error) {
	var attachments []AgentAttachment
	err := r.db(ctx).Where("thread_id IS NULL AND created_at < ?", before).Find(&attachments).Error
	return attachments, err
}

// DeleteAttachments removes rows by id, for the orphan sweep.
func (r *AgentRepository) DeleteAttachments(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db(ctx).Where("id IN ?", ids).Delete(&AgentAttachment{})
	return res.RowsAffected, res.Error
}
