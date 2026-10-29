package application

import (
	"context"
	"errors"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// canonicalInput binds the current model input to the stored user message. Images
// are expanded only here, never in host checkpoints or the persisted transcript.
func (s *harnessProxy) canonicalInput(ctx context.Context, run *persistence.AgentRun, messages []any) error {
	parts, err := s.repo().ListMessageParts(ctx, run.UserID, derefString(run.ParentMessageID))
	if err != nil {
		return err
	}
	content := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case "image":
			id, ok := AttachmentIDFromRef(part.Text)
			if !ok {
				return errors.New("invalid attachment reference")
			}
			attachment, err := s.repo().GetAttachment(ctx, run.UserID, id)
			if err != nil || derefString(attachment.MessageID) != derefString(run.ParentMessageID) || derefString(attachment.ThreadID) != run.ThreadID {
				return errors.New("attachment is not available for this message")
			}
			uri, ok := s.svc.attachments.DataURI(ctx, run.UserID, id)
			if !ok {
				return errors.New("attachment content is unavailable")
			}
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}})
		}
	}
	if len(content) == 0 {
		return errors.New("run has no canonical user input")
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if ok && message["role"] == "user" {
			message["content"] = content
			return nil
		}
	}
	return errors.New("model request has no current user message")
}
