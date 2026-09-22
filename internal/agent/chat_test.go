package agent

import (
	"context"
	"strings"
	"testing"
)

// The OpenAI-compatible stream parser (doc/agent-impl.md §5.1.1, doc/harness.md §4.4).
//
// The parser is incremental because the harness proxy forwards bytes it has already parsed: there
// is no point at which the whole body is in memory, and a frame may be split across reads.

// recorder is a StreamSink that keeps every callback, so a test can assert the order and the
// granularity of what the parser saw — not just the assembled result.
type recorder struct {
	text      []string
	reasoning []string
	deltas    []ToolCallDelta
	finish    []string
	failOn    int
	seen      int
}

func (r *recorder) TextDelta(_ context.Context, delta string) error {
	r.seen++
	if r.failOn > 0 && r.seen >= r.failOn {
		return context.Canceled
	}
	r.text = append(r.text, delta)
	return nil
}

func (r *recorder) ReasoningDelta(_ context.Context, delta string) error {
	r.reasoning = append(r.reasoning, delta)
	return nil
}

func (r *recorder) ToolCallDelta(_ context.Context, delta ToolCallDelta) error {
	r.deltas = append(r.deltas, delta)
	return nil
}

func (r *recorder) Finish(_ context.Context, reason string) error {
	r.finish = append(r.finish, reason)
	return nil
}

// parse feeds whole frames through the incremental parser the way the proxy does.
func parse(t *testing.T, sink StreamSink, frames ...string) ChatResult {
	t.Helper()
	parser := NewStreamParser(sink)
	ctx := context.Background()
	for _, frame := range frames {
		if err := parser.Feed(ctx, []byte(frame)); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if err := parser.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return parser.Result()
}

// TestParseChatStreamForwardsTextDeltas asserts streamed text reaches the sink incrementally
// rather than buffered, which is what makes append-text real (§5.1.1).
func TestParseChatStreamForwardsTextDeltas(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	if len(sink.text) != 2 {
		t.Fatalf("sink got %d deltas (%q), want 2 incremental ones", len(sink.text), sink.text)
	}
	if got := strings.Join(sink.text, ""); got != "你好，世界" {
		t.Fatalf("joined text=%q", got)
	}
	if result.Text != "你好，世界" || result.FinishReason != "stop" {
		t.Fatalf("result=%#v", result)
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("unexpected tool calls: %#v", result.ToolCalls)
	}
	if len(sink.finish) != 1 || sink.finish[0] != "stop" {
		t.Fatalf("finish callbacks=%v, want one stop", sink.finish)
	}
}

// TestParseChatStreamAssemblesFragmentedToolCalls asserts tool-call arguments that arrive in
// fragments keyed by index are reassembled into complete calls, and that one turn can carry both
// text and tool calls (§6: same-turn text + tool calls is normal).
func TestParseChatStreamAssemblesFragmentedToolCalls(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"让我查一下\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"list_goals\",\"arguments\":\"\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"sta\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"tus\\\":\\\"active\\\"}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"get_daily_plan\",\"arguments\":\"{}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	if result.Text != "让我查一下" {
		t.Fatalf("text=%q, want the same-turn preamble", result.Text)
	}
	if result.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", result.FinishReason)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("tool calls=%#v, want 2", result.ToolCalls)
	}
	first, second := result.ToolCalls[0], result.ToolCalls[1]
	if first.ID != "call_a" || first.Name != "list_goals" || first.Arguments != `{"status":"active"}` {
		t.Fatalf("first call reassembly wrong: %#v", first)
	}
	if second.ID != "call_b" || second.Name != "get_daily_plan" || second.Arguments != "{}" {
		t.Fatalf("second call wrong: %#v", second)
	}
	// The sink saw the id and name on the first fragment of a call, which is what lets the proxy
	// create the part before the arguments are complete (doc/harness.md §4.4.1).
	if len(sink.deltas) == 0 || sink.deltas[0].ID != "call_a" || sink.deltas[0].Name != "list_goals" {
		t.Fatalf("first delta=%#v, want the call id and name", sink.deltas[0])
	}
}

// TestParseChatStreamForwardsReasoning covers doc/chat-features.md §3.1: qwen's
// reasoning_content arrives as its own delta kind and must not be mixed into the answer text.
func TestParseChatStreamForwardsReasoning(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先看看\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"目标\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你有一个目标。\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	if got := strings.Join(sink.reasoning, ""); got != "先看看目标" {
		t.Fatalf("reasoning=%q", got)
	}
	if got := strings.Join(sink.text, ""); got != "你有一个目标。" {
		t.Fatalf("answer text=%q, reasoning leaked into it", got)
	}
	if result.Reasoning != "先看看目标" || result.Text != "你有一个目标。" {
		t.Fatalf("result=%#v", result)
	}
}

// TestParseChatStreamHandlesInterleavedFragments covers the ordering the harness spike measured
// (doc/harness.md §16.3): fragments are interleaved, not nested, so a parser that assumes one
// part finishes before the next begins loses text.
func TestParseChatStreamHandlesInterleavedFragments(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"我先\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"list_goals\",\"arguments\":\"{}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"查一下\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"还有别的目标吗\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"get_daily_plan\",\"arguments\":\"{}\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	if result.Text != "我先查一下" {
		t.Fatalf("text=%q, want both fragments joined across the interleaved tool call", result.Text)
	}
	if result.Reasoning != "还有别的目标吗" {
		t.Fatalf("reasoning=%q", result.Reasoning)
	}
	if len(result.ToolCalls) != 2 || result.ToolCalls[1].ID != "call_2" {
		t.Fatalf("tool calls=%#v, want both calls in index order", result.ToolCalls)
	}
}

// TestParseChatStreamSurvivesSplitFrames feeds one frame in two pieces, which is what a real
// network read does. A parser that assumed frames arrive whole would drop the tail.
func TestParseChatStreamSurvivesSplitFrames(t *testing.T) {
	sink := &recorder{}
	parser := NewStreamParser(sink)
	ctx := context.Background()
	frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"分段到达\"}}]}\n\ndata: [DONE]\n\n"
	if err := parser.Feed(ctx, []byte(frame[:20])); err != nil {
		t.Fatal(err)
	}
	if err := parser.Feed(ctx, []byte(frame[20:])); err != nil {
		t.Fatal(err)
	}
	if err := parser.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := parser.Result().Text; got != "分段到达" {
		t.Fatalf("text=%q, want the frame reassembled across reads", got)
	}
}

// TestParseChatStreamIgnoresKeepAliveComments asserts SSE comments and blank lines are skipped
// rather than treated as data.
func TestParseChatStreamIgnoresKeepAliveComments(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		": ping\n\n",
		"\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	if result.Text != "ok" {
		t.Fatalf("text=%q", result.Text)
	}
}

// TestParseChatStreamRejectsEmptyStream asserts a stream that carried no data frame is an error
// rather than an empty success: an upstream that accepts a request and then says nothing must not
// look like a model that answered with nothing.
func TestParseChatStreamRejectsEmptyStream(t *testing.T) {
	parser := NewStreamParser(&recorder{})
	ctx := context.Background()
	if err := parser.Feed(ctx, []byte(": ping\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := parser.Close(ctx); err == nil {
		t.Fatal("an empty stream was accepted")
	}
}

// TestParseChatStreamToleratesMalformedFrame asserts one unparsable frame does not abort the turn;
// providers interleave keep-alives that are not valid JSON.
func TestParseChatStreamToleratesMalformedFrame(t *testing.T) {
	sink := &recorder{}
	result := parse(t, sink,
		"data: {not json}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"still here\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	if result.Text != "still here" {
		t.Fatalf("text=%q, a malformed frame aborted the stream", result.Text)
	}
}

// TestParseChatStreamDerivesFinishReason asserts a stream with no finish_reason still reports one,
// because callers branch on it.
func TestParseChatStreamDerivesFinishReason(t *testing.T) {
	withCalls := parse(t, &recorder{},
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"list_goals\",\"arguments\":\"{}\"}}]}}]}\n\n",
		"data: [DONE]\n\n",
	)
	if withCalls.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q, want tool_calls", withCalls.FinishReason)
	}
	withoutCalls := parse(t, &recorder{},
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	if withoutCalls.FinishReason != "stop" {
		t.Fatalf("finish=%q, want stop", withoutCalls.FinishReason)
	}
}

// TestParseChatStreamPropagatesSinkError asserts a sink failure aborts the parse: the proxy uses it
// to stop forwarding when writing the transcript fails.
func TestParseChatStreamPropagatesSinkError(t *testing.T) {
	sink := &recorder{failOn: 1}
	parser := NewStreamParser(sink)
	err := parser.Feed(context.Background(), []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
	if err == nil {
		t.Fatal("a sink error was swallowed")
	}
}

// TestWireToolsShape asserts the tool list the proxy injects is in the OpenAI function shape, with
// a schema even for a tool that declared none (§5.1.1).
func TestWireToolsShape(t *testing.T) {
	wire := WireTools([]ToolDefinition{
		{Name: "list_goals", Description: "list active goals", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "no_schema", Description: "d"},
	})
	if len(wire) != 2 {
		t.Fatalf("wire tools=%#v, want 2", wire)
	}
	for i, tool := range wire {
		if tool["type"] != "function" {
			t.Fatalf("tool[%d] type=%v", i, tool["type"])
		}
		fn, _ := tool["function"].(map[string]any)
		if fn["parameters"] == nil {
			t.Fatalf("tool[%d] has no parameters schema", i)
		}
	}
}

// TestLooksLikeNoToolSupport covers the classification that decides between "this model can never
// run the agent" and "this call failed" (§5.1.1).
func TestLooksLikeNoToolSupport(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"model does not support tools"}}`, true},
		{422, `{"error":{"message":"unknown parameter: tools"}}`, true},
		{501, `{"error":{"message":"function_call is unsupported"}}`, true},
		{400, `{"error":{"message":"invalid json in message content"}}`, false},
		{502, `{"error":{"message":"tools not supported"}}`, false},
		{400, `{"error":{"message":"rate limited"}}`, false},
	}
	for _, tc := range cases {
		if got := LooksLikeNoToolSupport(tc.status, tc.body); got != tc.want {
			t.Fatalf("LooksLikeNoToolSupport(%d, %q)=%v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
