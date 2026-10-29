package application

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

func utf8Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Bound text and metadata, not encoded image transport bytes. Original records
// remain queryable; user constraints and approval facts are never silently cut.
func boundModelContext(messages []any, budget int) ([]any, error) {
	for _, raw := range messages {
		m := raw.(map[string]any)
		if m["role"] != "tool" {
			continue
		}
		text, _ := m["content"].(string)
		if len(text) > 12000 {
			var original map[string]any
			_ = json.Unmarshal([]byte(text), &original)
			bounded := map[string]any{"truncated": true, "source_result_retained": true, "note": "历史工具结果节选；需要完整范围时使用原工具分页重读，不得推断未展示部分不存在", "excerpt": utf8Prefix(text, 8000)}
			for _, key := range []string{"goal_id", "tree_revision", "applied", "decision", "proposal_id", "has_more", "next_offset"} {
				if value, ok := original[key]; ok {
					bounded[key] = value
				}
			}
			b, _ := json.Marshal(bounded)
			m["content"] = string(b)
		}
	}
	cost := func(items []any) int {
		total := 0
		for _, raw := range items {
			m := raw.(map[string]any)
			if parts, ok := m["content"].([]any); ok {
				for _, p := range parts {
					part := p.(map[string]any)
					if part["type"] == "image_url" {
						total += 256
					} else {
						b, _ := json.Marshal(part)
						total += len(b)
					}
				}
			} else {
				b, _ := json.Marshal(m)
				total += len(b)
			}
		}
		return total
	}
	if cost(messages) <= budget {
		return messages, nil
	}
	// Remove older assistant/tool exchanges atomically, preserving every user
	// message and explicit approval result. This is deterministic context curation.
	for i := 1; i < len(messages)-4 && cost(messages) > budget; {
		m := messages[i].(map[string]any)
		role := m["role"]
		if role != "assistant" {
			i++
			continue
		}
		end := i + 1
		protected := false
		if calls, ok := m["tool_calls"].([]any); ok {
			expected := len(calls)
			for count := 0; count < expected; count++ {
				if end >= len(messages) || messages[end].(map[string]any)["role"] != "tool" {
					return nil, fmt.Errorf("unpaired tool history")
				}
				text, _ := messages[end].(map[string]any)["content"].(string)
				protected = protected || strings.Contains(text, "applied") || strings.Contains(text, "decision") || strings.Contains(text, "pending") || strings.Contains(text, "unknown")
				end++
			}
		}
		if protected {
			i = end
			continue
		}
		messages = append(messages[:i], messages[end:]...)
	}
	if cost(messages) > budget {
		return nil, fmt.Errorf("CONTEXT_BUDGET_EXCEEDED: user constraints and recent decisions exceed the text budget; use a new thread with explicit retained constraints")
	}
	return messages, nil
}
