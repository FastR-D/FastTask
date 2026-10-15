package persistence

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrUnknownThreadStatus is returned for a status filter that is neither regular nor archived. It maps
// to 422: the client asked for something the model does not have (doc/chat-features.md §2.2).
var ErrUnknownThreadStatus = errors.New("unknown thread status")

// Thread catalogue queries (doc/chat-features.md §2).
//
// A thread is the unit a user thinks in: it owns the messages, the checkpoint that makes the
// conversation portable, and the attachments. Every query here is user-scoped, so a thread belonging to
// somebody else is indistinguishable from one that does not exist (arch.md §12).

// ListThreads returns a page of threads, newest activity first. The ordering matches the index the
// migration creates, and the cursor is (last_message_at, id), so a page boundary can neither skip nor
// repeat a row when two threads share a timestamp.
func (r *AgentRepository) ListThreads(ctx context.Context, userID, status string, cursorTime *time.Time, cursorID string, limit int) ([]AgentThread, error) {
	if limit <= 0 {
		limit = 20
	}
	query := r.db(ctx).Model(&AgentThread{}).Where("user_id = ?", userID)
	switch status {
	case ThreadRegular, ThreadArchived:
		query = query.Where("status = ?", status)
	case "":
		// No filter: a client that asks for everything gets everything.
	default:
		return nil, ErrUnknownThreadStatus
	}
	if cursorTime != nil {
		query = query.Where(
			"COALESCE(last_message_at, created_at) < ? OR (COALESCE(last_message_at, created_at) = ? AND id < ?)",
			*cursorTime, *cursorTime, cursorID,
		)
	}
	var threads []AgentThread
	err := query.Order("COALESCE(last_message_at, created_at) DESC, id DESC").Limit(limit).Find(&threads).Error
	return threads, err
}

// UpdateThread applies fields to a thread. It returns gorm.ErrRecordNotFound when the thread is not the
// user's, which callers turn into a 404.
func (r *AgentRepository) UpdateThread(ctx context.Context, userID, threadID string, fields map[string]any) error {
	updates := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		updates[key] = value
	}
	updates["updated_at"] = Now()
	res := r.db(ctx).Model(&AgentThread{}).Where("id = ? AND user_id = ?", threadID, userID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// FirstThreadMessageText returns the text of a thread's first user message, which is what a
// deterministic title is made of (doc/chat-features.md §2.4). It reads the authoritative parts rather
// than a client-supplied title, so a title can never say something the conversation did not.
func (r *AgentRepository) FirstThreadMessageText(ctx context.Context, userID, threadID string) (string, error) {
	texts, err := r.FirstThreadMessageTexts(ctx, userID, []string{threadID})
	return texts[threadID], err
}

// unicode.IsSpace's characters match strings.Fields when deciding whether a
// text part can identify a conversation. Image-only and blank parts are skipped.
const threadTextWhitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

// FirstThreadMessageTexts reads only the requested owner's page in one query.
// It does not fetch entire transcripts or mutate title/recency metadata.
func (r *AgentRepository) FirstThreadMessageTexts(ctx context.Context, userID string, threadIDs []string) (map[string]string, error) {
	texts := make(map[string]string, len(threadIDs))
	if len(threadIDs) == 0 {
		return texts, nil
	}
	var rows []struct {
		ThreadID string
		Text     string
	}
	err := r.db(ctx).Raw(`
		SELECT t.id AS thread_id, COALESCE((
			SELECT p.text FROM agent_messages m
			JOIN agent_message_parts p ON p.message_id = m.id AND p.user_id = m.user_id
			WHERE m.thread_id = t.id AND m.user_id = t.user_id AND m.role = 'user'
				AND p.type = 'text' AND TRIM(p.text, ?) <> ''
			ORDER BY m.seq, p.idx LIMIT 1
		), '') AS text
		FROM agent_threads t WHERE t.user_id = ? AND t.id IN ?`, threadTextWhitespace, userID, threadIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		texts[row.ThreadID] = row.Text
	}
	return texts, nil
}

// DeleteThread removes a thread and everything that belongs to it, in one transaction (§2.5): runs,
// messages, parts, the chunk log, harness tokens, attachment rows and the checkpoint column. The
// attachment FILES are deleted by the caller, which owns the storage layout and must not remove a file
// before the row that references it is gone.
//
// Deleting is not archiving: nothing here is recoverable, which is why the UI must not present it as
// undoable.
func (r *AgentRepository) DeleteThread(ctx context.Context, userID, threadID string) error {
	return r.store.Transaction(ctx, func(tx *gorm.DB) error {
		db := tx.WithContext(ctx)

		// Attachments reference both the thread and a message, so their rows go first — before either of
		// the things they point at disappears. The caller deletes the files afterwards: bytes are removed
		// once nothing references them (§2.5).
		if err := db.Where("thread_id = ? AND user_id = ?", threadID, userID).
			Delete(&AgentAttachment{}).Error; err != nil {
			return err
		}

		var runIDs []string
		if err := db.Model(&AgentRun{}).Where("thread_id = ? AND user_id = ?", threadID, userID).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := db.Where("run_id IN ?", runIDs).Delete(&AgentRunChunk{}).Error; err != nil {
				return err
			}
			if err := db.Where("run_id IN ?", runIDs).Delete(&AgentHarnessToken{}).Error; err != nil {
				return err
			}
		}
		var messageIDs []string
		if err := db.Model(&AgentMessage{}).Where("thread_id = ? AND user_id = ?", threadID, userID).
			Pluck("id", &messageIDs).Error; err != nil {
			return err
		}
		if len(messageIDs) > 0 {
			if err := db.Where("message_id IN ?", messageIDs).Delete(&AgentMessagePart{}).Error; err != nil {
				return err
			}
		}
		// agent_runs.parent_message_id and agent_messages.run_id reference each other, so neither table can
		// be emptied first. Dropping the parent link breaks the cycle; both tables are going anyway.
		if len(runIDs) > 0 {
			if err := db.Model(&AgentRun{}).Where("id IN ?", runIDs).
				Update("parent_message_id", nil).Error; err != nil {
				return err
			}
		}
		if len(messageIDs) > 0 {
			if err := db.Where("id IN ?", messageIDs).Delete(&AgentMessage{}).Error; err != nil {
				return err
			}
		}
		if len(runIDs) > 0 {
			if err := db.Where("id IN ?", runIDs).Delete(&AgentRun{}).Error; err != nil {
				return err
			}
		}
		res := db.Where("id = ? AND user_id = ?", threadID, userID).Delete(&AgentThread{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}
