package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ChatProvider is the tool-calling model port required by the agent loop
// (doc/agent-impl.md §5.1.1). It coexists with the legacy Provider interface
// (still used by the conversation / task_tree_* jobs, §8.1); it does not replace
// it.
//
// A single Chat call sends the full message history plus the tools the loop
// authorizes this turn, streams the model output into sink (text deltas), and
// returns the assembled result: the finish reason and any tool calls.
type ChatProvider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest, sink ChatSink) (ChatResult, error)
}

// ChatSink receives streamed assistant text as it arrives. Its implementation
// lives in the application layer and converts deltas into assistant-transport
// append-text chunks (§5.1.1). The agent package never depends on the protocol.
type ChatSink interface {
	TextDelta(ctx context.Context, delta string) error
}

// ChatMessage is one turn of history. Role is one of system/user/assistant/tool.
// Assistant messages that invoked tools carry ToolCalls; tool-role messages carry
// the result for ToolCallID.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a model-requested tool invocation. Arguments is the raw JSON the
// model produced; it is untrusted and must be schema-validated before use
// (arch.md §12, agent-impl.md §5.2).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition is the wire schema for one tool advertised to the model.
// Parameters is a JSON Schema object; the same schema is used to validate the
// model's arguments locally (§5.2).
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is one model turn.
type ChatRequest struct {
	Messages  []ChatMessage
	Tools     []ToolDefinition
	MaxTokens int
}

// ChatResult is the assembled outcome of one model turn.
type ChatResult struct {
	FinishReason string
	Text         string
	ToolCalls    []ToolCall
}

// ErrNoToolSupport signals the configured model cannot do tool calling. The run
// is failed with PROVIDER_NO_TOOL_SUPPORT rather than silently degraded to a
// single-turn reply, which would mislead the user into thinking the agent is
// working (§5.1.1).
var ErrNoToolSupport = errors.New("model does not support tool calling")

// Chat implements ChatProvider over an OpenAI-compatible /chat/completions
// endpoint with stream:true and the tools/tool_choice parameters (§5.1.1). It
// reuses the same base URL, API key and auth as the legacy complete() path.
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest, sink ChatSink) (ChatResult, error) {
	payload := map[string]any{
		"model":       o.model,
		"messages":    toWireMessages(req.Messages),
		"stream":      true,
		"tool_choice": "auto",
	}
	if len(req.Tools) > 0 {
		payload["tools"] = toWireTools(req.Tools)
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ChatResult{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return ChatResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+o.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	// Streaming responses must not be bounded by the legacy 45s client timeout;
	// the loop enforces wall-clock via ctx. Use a timeout-free client.
	client := o.chatClient()
	response, err := client.Do(request)
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		if looksLikeNoToolSupport(response.StatusCode, string(body)) {
			return ChatResult{}, ErrNoToolSupport
		}
		return ChatResult{}, fmt.Errorf("chat returned status %d: %s", response.StatusCode, truncate(string(body), 300))
	}
	return parseChatStream(ctx, response.Body, sink)
}

// chatClient returns a client without a total timeout for streaming, created once.
func (o *OpenAI) chatClient() *http.Client {
	if o.streamClient != nil {
		return o.streamClient
	}
	return &http.Client{}
}

func toWireMessages(messages []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		wire := map[string]any{"role": m.Role, "content": m.Content}
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, call := range m.ToolCalls {
					calls = append(calls, map[string]any{
						"id":   call.ID,
						"type": "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					})
				}
				wire["tool_calls"] = calls
				// OpenAI requires content to be present (may be null) with tool_calls.
				if m.Content == "" {
					wire["content"] = nil
				}
			}
		case "tool":
			wire["tool_call_id"] = m.ToolCallID
			if m.Name != "" {
				wire["name"] = m.Name
			}
		}
		out = append(out, wire)
	}
	return out
}

func toWireTools(tools []ToolDefinition) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return out
}

// streamChunk mirrors the OpenAI streaming choice/delta shape.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// parseChatStream consumes the SSE body, forwarding text deltas to the sink and
// assembling tool calls (whose arguments arrive in fragments keyed by index).
func parseChatStream(ctx context.Context, body io.Reader, sink ChatSink) (ChatResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var result ChatResult
	var text strings.Builder
	type pending struct {
		id, name, args string
	}
	byIndex := map[int]*pending{}
	maxIndex := -1
	sawAnyData := false

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue // SSE comment / keep-alive
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Tolerate a malformed keep-alive frame; do not abort the turn.
			continue
		}
		sawAnyData = true
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" && sink != nil {
				if err := sink.TextDelta(ctx, choice.Delta.Content); err != nil {
					return result, err
				}
			}
			text.WriteString(choice.Delta.Content)
			for _, call := range choice.Delta.ToolCalls {
				idx := 0
				if call.Index != nil {
					idx = *call.Index
				}
				slot := byIndex[idx]
				if slot == nil {
					slot = &pending{}
					byIndex[idx] = slot
				}
				if idx > maxIndex {
					maxIndex = idx
				}
				if call.ID != "" {
					slot.id = call.ID
				}
				if call.Function.Name != "" {
					slot.name = call.Function.Name
				}
				slot.args += call.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				result.FinishReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read chat stream: %w", err)
	}
	if !sawAnyData {
		return result, errors.New("chat stream produced no data")
	}

	// Assemble tool calls in index order.
	for i := 0; i <= maxIndex; i++ {
		slot := byIndex[i]
		if slot == nil || slot.name == "" {
			continue
		}
		args := slot.args
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: slot.id, Name: slot.name, Arguments: args})
	}
	result.Text = text.String()
	if result.FinishReason == "" {
		if len(result.ToolCalls) > 0 {
			result.FinishReason = "tool_calls"
		} else {
			result.FinishReason = "stop"
		}
	}
	return result, nil
}

// looksLikeNoToolSupport maps a provider rejection of the tools parameter to
// ErrNoToolSupport so the run fails loudly with PROVIDER_NO_TOOL_SUPPORT instead
// of silently degrading (§5.1.1).
func looksLikeNoToolSupport(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotImplemented && status != http.StatusUnprocessableEntity {
		return false
	}
	lowered := strings.ToLower(body)
	mentionsTools := strings.Contains(lowered, "tool") || strings.Contains(lowered, "function calling") || strings.Contains(lowered, "function_call")
	mentionsUnsupported := strings.Contains(lowered, "not support") || strings.Contains(lowered, "unsupported") ||
		strings.Contains(lowered, "does not support") || strings.Contains(lowered, "invalid") || strings.Contains(lowered, "unknown parameter")
	return mentionsTools && mentionsUnsupported
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}
