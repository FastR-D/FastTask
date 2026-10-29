package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// modelContext always rebuilds from durable records. Runtime checkpoints are an
// optional host cache, never the authority for history or late approval results.
func (s *harnessProxy) modelContext(ctx context.Context, run *persistence.AgentRun, instructions string) ([]any, error) {
	messages, err := s.repo().ListThreadMessages(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return nil, err
	}
	out := []any{map[string]any{"role": "system", "content": instructions}}
	for _, m := range messages {
		parts, err := s.repo().ListMessageParts(ctx, run.UserID, m.ID)
		if err != nil {
			return nil, err
		}
		if m.Role == "user" {
			content := []any{}
			for _, p := range parts {
				switch p.Type {
				case "text":
					content = append(content, map[string]any{"type": "text", "text": p.Text})
				case "image":
					// Current images are checked strictly by canonicalInput below. Historical
					// unavailable files are explicitly marked, rather than fabricated.
					id, ok := AttachmentIDFromRef(p.Text)
					if ok {
						a, e := s.repo().GetAttachment(ctx, run.UserID, id)
						if e == nil && derefString(a.MessageID) == m.ID {
							if uri, ok := s.svc.attachments.DataURI(ctx, run.UserID, id); ok {
								content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}})
								continue
							}
						}
					}
					content = append(content, map[string]any{"type": "text", "text": "[历史附件不可用]"})
				}
			}
			out = append(out, map[string]any{"role": "user", "content": content})
			continue
		}
		if m.Role != "assistant" {
			continue
		}
		var text strings.Builder
		flush := func() {
			if text.Len() > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": text.String()})
				text.Reset()
			}
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				text.WriteString(p.Text)
			case "tool-call":
				flush()
				id := derefString(p.ToolCallID)
				if id == "" || !json.Valid([]byte(p.ArgsJSON)) {
					out = append(out, map[string]any{"role": "assistant", "content": fmt.Sprintf("[未完成的工具调用 %s，不能作为成功依据]", p.ToolName)})
					continue
				}
				out = append(out, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": p.ToolName, "arguments": p.ArgsJSON}}}})
				result := p.ResultJSON
				if result == "" {
					result = `{"status":"unknown","note":"执行结果未知，先核对，不得重复副作用"}`
				}
				if p.ApprovalStatus == "pending" {
					result = `{"status":"pending","applied":false}`
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "name": p.ToolName, "content": result})
			}
		}
		flush()
	}
	if err := s.canonicalInput(ctx, run, out); err != nil {
		return nil, err
	}
	budget := s.svc.limits.ContextBytes
	if budget <= 0 {
		budget = 128 * 1024
	}
	return boundModelContext(out, budget)
}
