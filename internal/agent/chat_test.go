package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/config"
)

// collectSink records streamed text deltas.
type collectSink struct {
	deltas []string
}

func (c *collectSink) TextDelta(_ context.Context, delta string) error {
	c.deltas = append(c.deltas, delta)
	return nil
}

func (c *collectSink) joined() string { return strings.Join(c.deltas, "") }

// sseServer returns a test server that streams the given raw SSE frames from
// /chat/completions, asserting the request carried stream:true and the tools.
func sseServer(t *testing.T, frames string, checkRequest func(*testing.T, *http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/incorrect authorization header")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if checkRequest != nil {
			checkRequest(t, r, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frames)
	}))
}

// TestChatStreamsTextDeltas asserts streaming text is forwarded to the sink
// incrementally (not buffered), which is what makes append-text real (§5.1.1).
func TestChatStreamsTextDeltas(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"，世界"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	var sawStream bool
	server := sseServer(t, frames, func(t *testing.T, r *http.Request, body map[string]any) {
		if stream, _ := body["stream"].(bool); !stream {
			t.Errorf("request did not set stream:true")
		}
		sawStream = true
	})
	defer server.Close()

	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "test-model", OpenAIAPIKey: "test-key"})
	sink := &collectSink{}
	result, err := provider.Chat(context.Background(), ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if !sawStream {
		t.Fatal("request check did not run")
	}
	if len(sink.deltas) != 2 {
		t.Fatalf("sink got %d deltas (%q), want 2 incremental", len(sink.deltas), sink.deltas)
	}
	if sink.joined() != "你好，世界" {
		t.Fatalf("joined text=%q", sink.joined())
	}
	if result.Text != "你好，世界" || result.FinishReason != "stop" {
		t.Fatalf("result=%#v", result)
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("unexpected tool calls: %#v", result.ToolCalls)
	}
}

// TestChatAssemblesFragmentedToolCalls asserts tool-call arguments that arrive in
// fragments keyed by index are reassembled into complete calls, and that a turn
// can carry both text and tool calls (§6: same-turn text + tool calls is normal).
func TestChatAssemblesFragmentedToolCalls(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"让我查一下"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_goals","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"sta"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"tus\":\"active\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_daily_plan","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	server := sseServer(t, frames, nil)
	defer server.Close()

	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "m", OpenAIAPIKey: "test-key"})
	sink := &collectSink{}
	result, err := provider.Chat(context.Background(), ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "今天做什么"}},
		Tools:    []ToolDefinition{{Name: "list_goals", Description: "d", Parameters: map[string]any{"type": "object"}}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "让我查一下" {
		t.Fatalf("text=%q, want the same-turn preamble", result.Text)
	}
	if result.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", result.FinishReason)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("tool calls=%#v, want 2", result.ToolCalls)
	}
	if result.ToolCalls[0].ID != "call_a" || result.ToolCalls[0].Name != "list_goals" || result.ToolCalls[0].Arguments != `{"status":"active"}` {
		t.Fatalf("first call reassembly wrong: %#v", result.ToolCalls[0])
	}
	if result.ToolCalls[1].ID != "call_b" || result.ToolCalls[1].Name != "get_daily_plan" || result.ToolCalls[1].Arguments != "{}" {
		t.Fatalf("second call wrong: %#v", result.ToolCalls[1])
	}
}

// TestChatSendsToolsInOpenAIShape asserts the request advertises tools in the
// OpenAI function schema and requests auto tool choice (§5.1.1).
func TestChatSendsToolsInOpenAIShape(t *testing.T) {
	frames := `data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" + `data: [DONE]` + "\n\n"
	server := sseServer(t, frames, func(t *testing.T, r *http.Request, body map[string]any) {
		if body["tool_choice"] != "auto" {
			t.Errorf("tool_choice=%v, want auto", body["tool_choice"])
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools=%#v, want 1", body["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["type"] != "function" {
			t.Errorf("tool type=%v", tool["type"])
		}
		fn, _ := tool["function"].(map[string]any)
		if fn["name"] != "list_goals" {
			t.Errorf("tool name=%v", fn["name"])
		}
		if _, ok := fn["parameters"]; !ok {
			t.Errorf("tool missing parameters schema")
		}
	})
	defer server.Close()

	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "m", OpenAIAPIKey: "test-key"})
	_, err := provider.Chat(context.Background(), ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "list_goals", Description: "list active goals", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
	}, &collectSink{})
	if err != nil {
		t.Fatal(err)
	}
}

// TestChatReturnsNoToolSupport asserts a provider that rejects the tools
// parameter surfaces ErrNoToolSupport so the run fails loudly instead of
// silently degrading (§5.1.1).
func TestChatReturnsNoToolSupport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"model does not support tools parameter","type":"invalid_request_error"}}`)
	}))
	defer server.Close()

	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "m", OpenAIAPIKey: "test-key"})
	_, err := provider.Chat(context.Background(), ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "x"}},
	}, &collectSink{})
	if !errors.Is(err, ErrNoToolSupport) {
		t.Fatalf("err=%v, want ErrNoToolSupport", err)
	}
}

// TestChatPropagatesNon2xx asserts an ordinary provider error is returned (not
// mistaken for no-tool-support) so the loop can classify it as retryable or not.
func TestChatPropagatesNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"upstream"}`)
	}))
	defer server.Close()
	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "m", OpenAIAPIKey: "test-key"})
	_, err := provider.Chat(context.Background(), ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, &collectSink{})
	if err == nil || errors.Is(err, ErrNoToolSupport) {
		t.Fatalf("err=%v, want a generic provider error", err)
	}
}

// TestChatHonorsContextCancellation asserts a cancelled context aborts the stream
// read, which is how the loop's per-turn and wall-clock timeouts take effect.
func TestChatHonorsContextCancellation(t *testing.T) {
	// A stream that never terminates; the client cancels mid-read.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "m", OpenAIAPIKey: "test-key"})
	sink := &cancelSink{cancel: cancel, after: 3}
	_, err := provider.Chat(ctx, ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, sink)
	if err == nil {
		t.Fatal("expected cancellation to abort the stream")
	}
}

// cancelSink cancels the context after N deltas.
type cancelSink struct {
	cancel context.CancelFunc
	after  int
	count  int
}

func (c *cancelSink) TextDelta(_ context.Context, _ string) error {
	c.count++
	if c.count >= c.after {
		c.cancel()
	}
	return nil
}
