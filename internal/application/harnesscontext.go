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
func (s *harnessProxy) modelContext(ctx context.Context, run *persistence.AgentRun, instructions string, includeReasoning ...bool) ([]any, error) {
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
		var text, reasoning strings.Builder
		reasoningEnabled := len(includeReasoning) > 0 && includeReasoning[0]
		assistant := func(content string, calls []any) {
			message := map[string]any{"role": "assistant", "content": content}
			if reasoningEnabled {
				message["reasoning_content"] = reasoning.String()
			}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			out = append(out, message)
			text.Reset()
			reasoning.Reset()
		}
		for index := 0; index < len(parts); index++ {
			p := parts[index]
			switch p.Type {
			case "reasoning":
				reasoning.WriteString(p.Text)
			case "text":
				text.WriteString(p.Text)
			case "tool-call":
				id := derefString(p.ToolCallID)
				if id == "" || !json.Valid([]byte(p.ArgsJSON)) {
					text.WriteString(fmt.Sprintf("[未完成的工具调用 %s，不能作为成功依据]", p.ToolName))
					assistant(text.String(), nil)
					continue
				}
				group := []persistence.AgentMessagePart{p}
				// One provider response may call several tools. Preserve that single
				// assistant response and its exact reasoning, then emit its paired results.
				for index+1 < len(parts) {
					next := parts[index+1]
					if next.Type != "tool-call" || next.ToolModelTurn != p.ToolModelTurn || derefString(next.ToolCallID) == "" || !json.Valid([]byte(next.ArgsJSON)) {
						break
					}
					group = append(group, next)
					index++
				}
				calls := []any{}
				for _, call := range group {
					calls = append(calls, map[string]any{"id": derefString(call.ToolCallID), "type": "function", "function": map[string]any{"name": call.ToolName, "arguments": call.ArgsJSON}})
				}
				assistant(text.String(), calls)
				for _, call := range group {
					result := call.ResultJSON
					if result == "" {
						result = `{"status":"unknown","note":"执行结果未知，先核对，不得重复副作用"}`
					}
					if call.ApprovalStatus == "pending" {
						result = `{"status":"pending","applied":false}`
					}
					out = append(out, map[string]any{"role": "tool", "tool_call_id": derefString(call.ToolCallID), "name": call.ToolName, "content": result})
				}
			}
		}
		if text.Len() > 0 {
			assistant(text.String(), nil)
		}
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
