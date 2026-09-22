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

// The OpenAI-compatible streaming half of the model port (doc/agent-impl.md §5.1.1,
// doc/harness.md §4.4).
//
// What used to live here was a ChatProvider: the server built a request, called the
// model and drove a tool loop. Under the harness the host builds the request and drives
// the loop, and the server is a proxy that must read the same stream on its way past —
// so what survives is the parser, now incremental, and what is gone is the caller.

// ChatMessage is one turn of history. Role is one of system/user/assistant/tool.
// Assistant messages that invoked tools carry ToolCalls; tool-role messages carry the
// result for ToolCallID. The harness keeps this shape for the degraded path that
// rebuilds a summary from persisted parts (doc/harness.md §6.3).
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a model-requested tool invocation. Arguments is the raw JSON the model
// produced; it is untrusted and must be schema-validated before use (arch.md §12,
// agent-impl.md §5.2).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition is the wire schema for one tool advertised to the model. Parameters is a
// JSON Schema object; the same schema validates the model's arguments locally (§5.2).
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatResult is the assembled outcome of one streamed model call.
type ChatResult struct {
	FinishReason string
	Text         string
	Reasoning    string
	ToolCalls    []ToolCall
}

// ErrNoToolSupport signals the configured model cannot do tool calling. The run is failed
// with PROVIDER_NO_TOOL_SUPPORT rather than silently degraded to a single-turn reply, which
// would mislead the user into thinking the agent is working (§5.1.1).
var ErrNoToolSupport = errors.New("model does not support tool calling")

// ErrEmptyStream signals a stream that carried no data frame at all: an upstream that
// accepted the request and then said nothing.
var ErrEmptyStream = errors.New("chat stream produced no data")

// ToolCallDelta is one fragment of a streamed tool call. OpenAI-compatible providers send
// the id and name once and the arguments in pieces, all keyed by an index that identifies
// the call within the turn.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// StreamSink receives a stream as it arrives. The harness proxy implements it to write the
// authoritative transcript while the same bytes are forwarded to the host
// (doc/harness.md §4.4).
type StreamSink interface {
	TextDelta(ctx context.Context, delta string) error
	ReasoningDelta(ctx context.Context, delta string) error
	ToolCallDelta(ctx context.Context, delta ToolCallDelta) error
	Finish(ctx context.Context, reason string) error
}

// nopSink discards everything; it keeps the parser usable when a caller only wants the
// assembled result.
type nopSink struct{}

func (nopSink) TextDelta(context.Context, string) error            { return nil }
func (nopSink) ReasoningDelta(context.Context, string) error       { return nil }
func (nopSink) ToolCallDelta(context.Context, ToolCallDelta) error { return nil }
func (nopSink) Finish(context.Context, string) error               { return nil }

// StreamParser turns the bytes of an OpenAI-compatible SSE response into sink calls, one
// buffer at a time. It is incremental because the proxy forwards bytes it has already
// parsed: there is no point at which the whole body is in memory.
//
// Fragments are not assumed to be nested or ordered (§4.4.1): text, reasoning and several
// tool calls may interleave freely, so each is tracked on its own.
type StreamParser struct {
	sink StreamSink

	buf      []byte
	result   ChatResult
	pending  map[int]*pendingCall
	maxIndex int
	sawData  bool
	finished bool

	text      strings.Builder
	reasoning strings.Builder
}

type pendingCall struct {
	id, name, args string
}

// NewStreamParser builds a parser writing to sink. A nil sink assembles the result only.
func NewStreamParser(sink StreamSink) *StreamParser {
	if sink == nil {
		sink = nopSink{}
	}
	return &StreamParser{sink: sink, pending: map[int]*pendingCall{}, maxIndex: -1}
}

// Feed hands the parser the next bytes read from upstream. It returns the first sink error,
// which the caller turns into an aborted stream.
func (p *StreamParser) Feed(ctx context.Context, data []byte) error {
	p.buf = append(p.buf, data...)
	for {
		idx := bytes.IndexByte(p.buf, '\n')
		if idx < 0 {
			return nil
		}
		line := string(p.buf[:idx])
		p.buf = p.buf[idx+1:]
		if err := p.line(ctx, line); err != nil {
			return err
		}
	}
}

// Close flushes a trailing line that arrived without its newline and reports a stream that
// never carried data.
func (p *StreamParser) Close(ctx context.Context) error {
	if len(p.buf) > 0 {
		line := string(p.buf)
		p.buf = nil
		if err := p.line(ctx, line); err != nil {
			return err
		}
	}
	if !p.sawData {
		return ErrEmptyStream
	}
	return nil
}

// Result is the assembled outcome. It is complete once Close returns nil.
func (p *StreamParser) Result() ChatResult {
	result := p.result
	result.Text = p.text.String()
	result.Reasoning = p.reasoning.String()
	if result.FinishReason == "" {
		if len(result.ToolCalls) > 0 {
			result.FinishReason = "tool_calls"
		} else {
			result.FinishReason = "stop"
		}
	}
	return result
}

func (p *StreamParser) line(ctx context.Context, raw string) error {
	line := strings.TrimRight(raw, "\r")
	if line == "" || strings.HasPrefix(line, ":") {
		return nil // SSE comment / keep-alive
	}
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		p.sawData = true
		return p.assemble()
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// Tolerate a malformed keep-alive frame; do not abort the turn.
		return nil
	}
	p.sawData = true
	for _, choice := range chunk.Choices {
		if choice.Delta.ReasoningContent != "" {
			p.reasoning.WriteString(choice.Delta.ReasoningContent)
			if err := p.sink.ReasoningDelta(ctx, choice.Delta.ReasoningContent); err != nil {
				return err
			}
		}
		if choice.Delta.Content != "" {
			p.text.WriteString(choice.Delta.Content)
			if err := p.sink.TextDelta(ctx, choice.Delta.Content); err != nil {
				return err
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			idx := 0
			if call.Index != nil {
				idx = *call.Index
			}
			slot := p.pending[idx]
			if slot == nil {
				slot = &pendingCall{}
				p.pending[idx] = slot
			}
			if idx > p.maxIndex {
				p.maxIndex = idx
			}
			if call.ID != "" {
				slot.id = call.ID
			}
			if call.Function.Name != "" {
				slot.name = call.Function.Name
			}
			slot.args += call.Function.Arguments
			delta := ToolCallDelta{Index: idx, ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments}
			if err := p.sink.ToolCallDelta(ctx, delta); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			p.result.FinishReason = *choice.FinishReason
			if err := p.sink.Finish(ctx, *choice.FinishReason); err != nil {
				return err
			}
		}
	}
	return nil
}

// assemble freezes the tool-call list once the stream says it is over.
func (p *StreamParser) assemble() error {
	if p.finished {
		return nil
	}
	p.finished = true
	for i := 0; i <= p.maxIndex; i++ {
		slot := p.pending[i]
		if slot == nil || slot.name == "" {
			continue
		}
		args := slot.args
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		p.result.ToolCalls = append(p.result.ToolCalls, ToolCall{ID: slot.id, Name: slot.name, Arguments: args})
	}
	return nil
}

// Calls returns the tool calls seen so far, assembled in index order. Unlike Result it works
// mid-stream, which is what a proxy that must record a call before the host executes it
// needs.
func (p *StreamParser) Calls() []ToolCall {
	_ = p.assemble()
	p.finished = false
	return append([]ToolCall(nil), p.result.ToolCalls...)
}

// ParseChatStream consumes a whole SSE body through the incremental parser. It is the
// convenience form used by tests and by any caller that does not need to forward bytes.
func ParseChatStream(ctx context.Context, body io.Reader, sink StreamSink) (ChatResult, error) {
	parser := NewStreamParser(sink)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return parser.Result(), err
		}
		if err := parser.Feed(ctx, append(scanner.Bytes(), '\n')); err != nil {
			return parser.Result(), err
		}
	}
	if err := scanner.Err(); err != nil {
		return parser.Result(), fmt.Errorf("read chat stream: %w", err)
	}
	if err := parser.Close(ctx); err != nil {
		return parser.Result(), err
	}
	return parser.Result(), nil
}

// streamChunk mirrors the OpenAI streaming choice/delta shape, including the
// reasoning_content extension qwen providers use (doc/chat-features.md §3.1).
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
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

// WireTools renders tool definitions in the OpenAI-compatible shape. The harness proxy uses
// it to replace whatever a host claimed, because the registry — not the client — decides
// what may be called (doc/harness.md §4.3).
func WireTools(tools []ToolDefinition) []map[string]any {
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

// LooksLikeNoToolSupport maps a provider rejection of the tools parameter to
// ErrNoToolSupport so the run fails loudly with PROVIDER_NO_TOOL_SUPPORT instead of silently
// degrading (§5.1.1).
func LooksLikeNoToolSupport(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotImplemented && status != http.StatusUnprocessableEntity {
		return false
	}
	lowered := strings.ToLower(body)
	mentionsTools := strings.Contains(lowered, "tool") || strings.Contains(lowered, "function calling") || strings.Contains(lowered, "function_call")
	mentionsUnsupported := strings.Contains(lowered, "not support") || strings.Contains(lowered, "unsupported") ||
		strings.Contains(lowered, "does not support") || strings.Contains(lowered, "invalid") ||
		strings.Contains(lowered, "unknown parameter")
	return mentionsTools && mentionsUnsupported
}

// Truncate shortens a value for a log line or an error message, keeping it readable.
func Truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}
