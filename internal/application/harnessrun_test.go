package application

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The run-loop tests, ported from the in-process toolLoop to the harness
// (doc/harness.md §12: the loop driver moved to libfx, the limits and the error
// classification stayed server-side). Each test drives a run through the same surface a
// browser host uses — capability token, model proxy, tool execution, completion — against a
// scripted OpenAI-compatible upstream.

// --- shared helpers ---

// submitJob creates a server-driven run (no harness mode) and returns the AgentJob the Worker
// executes. This is the path a deployment without a model still uses.
func submitJob(t *testing.T, svc *AgentService, store *persistence.Store, userID, text string) persistence.AgentJob {
	t.Helper()
	submitted, err := svc.SubmitCommands(context.Background(), userID, CommandsRequest{
		Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: text}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var run persistence.AgentRun
	if err := store.DB.Where("id = ?", submitted.RunID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	var job persistence.AgentJob
	if err := store.DB.Where("id = ?", run.JobID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job
}

// assistantParts returns the persisted parts of the run's assistant message.
func assistantParts(t *testing.T, svc *AgentService, userID, runID string) []persistence.AgentMessagePart {
	t.Helper()
	ctx := context.Background()
	msgs, err := svc.Repository().ListRunMessages(ctx, userID, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		parts, err := svc.Repository().ListMessageParts(ctx, userID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		return parts
	}
	t.Fatal("no assistant message found")
	return nil
}

func runStatus(t *testing.T, svc *AgentService, userID, runID string) persistence.AgentRun {
	t.Helper()
	run, err := svc.Repository().GetRun(context.Background(), userID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return *run
}

func concatText(parts []persistence.AgentMessagePart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func partsOfType(parts []persistence.AgentMessagePart, kind string) []persistence.AgentMessagePart {
	out := make([]persistence.AgentMessagePart, 0, len(parts))
	for _, p := range parts {
		if p.Type == kind {
			out = append(out, p)
		}
	}
	return out
}

// testClock is an injectable clock, so heartbeat loss and cancellation grace can be tested
// without sleeping.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: persistence.Now()} }

func (c *testClock) At() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// --- the happy paths ---

// TestHarnessRunNoToolCallsSucceeds is the simplest turn: the model answers, the transcript is
// persisted, the run succeeds.
func TestHarnessRunNoToolCallsSucceeds(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "先打开 Overleaf，写下第一段。"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我推进论文")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	host.drive(4)

	run := host.runStatus()
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", run.Status)
	}
	parts := assistantParts(t, svc, f.user.ID, runID)
	if got := concatText(parts); got != "先打开 Overleaf，写下第一段。" {
		t.Fatalf("assistant text=%q", got)
	}
	if calls := upstream.callCount(); calls != 1 {
		t.Fatalf("model calls=%d, want 1", calls)
	}
	// The checkpoint the host reported at the end of its turn is stored on the thread (§6.1).
	thread, err := svc.Repository().GetThread(context.Background(), f.user.ID, run.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if string(thread.Checkpoint) != "checkpoint-bytes" || thread.LibfxVersion != LibfxVersion {
		t.Fatalf("checkpoint=%q version=%q, want the reported one", thread.Checkpoint, thread.LibfxVersion)
	}
}

// TestHarnessProxyReplacesSystemAndTools covers §4.3 and §14.4: whatever a host claims about
// the system prompt, the model and the tool list is discarded, and the server's own values are
// what the provider sees.
func TestHarnessProxyReplacesSystemAndTools(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "好。"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我推进论文")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	host.drive(2)

	request := upstream.request(t, 0)
	if model, _ := request["model"].(string); model != "qwen3.8-max" {
		t.Fatalf("model=%v, want the server's model, not the host's", model)
	}
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("the proxy forwarded no messages")
	}
	first, _ := messages[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("messages[0].role=%v, want system", role)
	}
	content, _ := first["content"].(string)
	if !strings.Contains(content, "最多三个") {
		t.Fatalf("system prompt is not the server's: %q", content)
	}
	if strings.Contains(content, "HOST INJECTED") {
		t.Fatal("a host-supplied system prompt reached the provider")
	}
	for _, message := range messages[1:] {
		entry, _ := message.(map[string]any)
		if role, _ := entry["role"].(string); role == "system" || role == "developer" {
			t.Fatalf("a second system message reached the provider: %#v", entry)
		}
	}
	tools, _ := request["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		entry, _ := item.(map[string]any)
		function, _ := entry["function"].(map[string]any)
		if name, ok := function["name"].(string); ok {
			names[name] = true
		}
	}
	if names["delete_everything"] {
		t.Fatal("a host-declared tool reached the provider")
	}
	if len(names) != 9 {
		t.Fatalf("advertised %d tools, want the registry's 9 (7 readonly + 2 proposal): %v", len(names), names)
	}
	if !names["propose_task_tree_patch"] || !names["propose_daily_plan"] {
		t.Fatal("proposal tools were not advertised")
	}
	if choice, _ := request["tool_choice"].(string); choice != "auto" {
		t.Fatalf("tool_choice=%v, want auto (the host must not force a call)", choice)
	}
	if _, present := request["api_key"]; present {
		t.Fatal("the host's api_key was forwarded upstream")
	}
	if _, present := request["base_url"]; present {
		t.Fatal("the host's base_url was forwarded upstream")
	}
}

// TestHarnessCredentialsNeverReachTheHost covers §4.5 and §14.17: the injected key and the
// upstream address appear in the request the provider receives and nowhere a host can read.
func TestHarnessCredentialsNeverReachTheHost(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "好。", reasoning: "想一想"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我推进论文")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	collector, err := host.modelCall("帮我推进论文")
	if err != nil {
		t.Fatalf("model call: %v", err)
	}
	creds := upstream.credentials()
	for _, secret := range []string{creds.APIKey, creds.BaseURL} {
		if strings.Contains(collector.forwarded, secret) {
			t.Fatalf("the proxied stream leaked %q to the host", secret)
		}
		if strings.Contains(host.grant.Instructions, secret) {
			t.Fatalf("the run grant leaked %q to the host", secret)
		}
	}
	// The grant carries the model id — a host needs it — but never the key.
	if host.grant.Model != creds.Model {
		t.Fatalf("grant model=%q, want %q", host.grant.Model, creds.Model)
	}
	if strings.Contains(host.token, creds.APIKey) {
		t.Fatal("the capability token embeds the provider key")
	}
	// The upstream saw the real key, which is the whole point of the proxy.
	upstream.mu.Lock()
	authorization := upstream.headers[0].Get("Authorization")
	upstream.mu.Unlock()
	if authorization != "Bearer "+creds.APIKey {
		t.Fatalf("upstream authorization=%q, want the injected provider key", authorization)
	}
	host.complete("stop")
}

// TestHarnessRunExecutesReadonlyTool covers §5: a readonly tool executes outside any
// transaction, its result goes back to the host (which feeds it to the model), and the call and
// its result are recorded on ONE part.
func TestHarnessRunExecutesReadonlyTool(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{text: "让我看看你的目标。", toolCalls: []agent.ToolCall{{ID: "call_1", Name: "list_goals", Arguments: `{"status":"active"}`}}},
		scriptedTurn{text: "你有一个活跃目标：Finish paper。"},
	)
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "我有哪些目标")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	host.drive(4)

	if calls := upstream.callCount(); calls != 2 {
		t.Fatalf("model calls=%d, want 2 (tool turn + answer turn)", calls)
	}
	parts := assistantParts(t, svc, f.user.ID, runID)
	toolParts := partsOfType(parts, "tool-call")
	if len(toolParts) != 1 {
		t.Fatalf("%d tool-call parts, want exactly 1: %#v", len(toolParts), toolParts)
	}
	if toolParts[0].ToolName != "list_goals" || derefString(toolParts[0].ToolCallID) != "call_1" {
		t.Fatalf("tool-call part=%#v", toolParts[0])
	}
	if !strings.Contains(toolParts[0].ResultJSON, "Finish paper") {
		t.Fatalf("tool result was not recorded on the part: %q", toolParts[0].ResultJSON)
	}
	if toolParts[0].IsError {
		t.Fatal("a successful tool call was recorded as an error")
	}
	if !strings.Contains(concatText(parts), "Finish paper") {
		t.Fatalf("final answer not persisted: %q", concatText(parts))
	}
	if host.runStatus().Status != persistence.RunSucceeded {
		t.Fatal("run did not succeed")
	}
	// A readonly tool must not create proposals or tasks (§5.2).
	var proposals, tasks int64
	f.store.DB.Model(&persistence.Proposal{}).Where("user_id = ?", f.user.ID).Count(&proposals)
	f.store.DB.Model(&persistence.Task{}).Where("user_id = ?", f.user.ID).Count(&tasks)
	if proposals != 0 {
		t.Fatalf("a readonly run created %d proposals", proposals)
	}
	if int(tasks) != len(f.tasks) {
		t.Fatalf("task count changed from %d to %d during a readonly run", len(f.tasks), tasks)
	}
}

// TestHarnessToolCallHistoryInRequestIsNeverPersisted covers §4.4.1 items 3 and 4 and §14.12:
// the history a host sends back on the next turn — the same tool call and its result — must not
// produce a second part.
func TestHarnessToolCallHistoryInRequestIsNeverPersisted(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "call_1", Name: "list_goals", Arguments: `{"status":"active"}`}}},
		scriptedTurn{text: "你有 1 个活跃目标。"},
	)
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "我有哪些目标")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	if !host.driveOneTurn() {
		t.Fatal("the first turn asked for no tool")
	}
	// The second request carries the whole history, as libfx does: the assistant's tool call and
	// the tool's result. None of it may be written (§4.4.1).
	history, _ := json.Marshal(map[string]any{
		"model": "qwen3.8-max", "stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "我有哪些目标"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "list_goals", "arguments": `{"status":"active"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "list_goals", "content": `{"goals":[{"title":"Finish paper"}]}`},
		},
	})
	if _, err := host.modelCallBody(history); err != nil {
		t.Fatalf("second model call: %v", err)
	}
	host.complete("stop")

	toolParts := partsOfType(assistantParts(t, svc, f.user.ID, runID), "tool-call")
	if len(toolParts) != 1 {
		t.Fatalf("%d tool-call parts after a replayed history, want 1: %#v", len(toolParts), toolParts)
	}
	var count int64
	f.store.DB.Model(&persistence.AgentMessagePart{}).Where("tool_call_id = ?", "call_1").Count(&count)
	if count != 1 {
		t.Fatalf("%d rows carry tool_call_id call_1, want 1", count)
	}
}

// --- structured tool errors (§5 item 6, §14) ---

// TestHarnessRunRejectsIdentityArgument covers agent.md §4 invariant 5: a model that smuggles an
// identity field gets a structured error and the tool never runs with it.
func TestHarnessRunRejectsIdentityArgument(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c1", Name: "list_goals", Arguments: `{"user_id":"victim"}`}}},
		scriptedTurn{text: "抱歉，我不能那样做。"},
	)
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "查看别人的目标")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	host.driveOneTurn()
	host.complete("stop")

	parts := partsOfType(assistantParts(t, svc, f.user.ID, runID), "tool-call")
	if len(parts) != 1 || !parts[0].IsError {
		t.Fatalf("identity argument was not recorded as an error: %#v", parts)
	}
	if !strings.Contains(parts[0].ResultJSON, "forbidden") && !strings.Contains(parts[0].ResultJSON, "identity") {
		t.Fatalf("the model was not told why: %q", parts[0].ResultJSON)
	}
	if host.runStatus().Status != persistence.RunSucceeded {
		t.Fatal("a rejected tool argument must not fail the run")
	}
}

// TestHarnessRunFeedsBackInvalidArgs covers §5.2: illegal arguments return a readable error the
// model can correct itself on.
func TestHarnessRunFeedsBackInvalidArgs(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c1", Name: "get_task_tree", Arguments: `{}`}}},
		scriptedTurn{text: "我需要 goal_id。"},
	)
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "看任务树")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	outcomes := host.executeCalls([]agent.ToolCall{{ID: "c1", Name: "get_task_tree", Arguments: `{}`}})
	if len(outcomes) != 1 || outcomes[0].Status != "error" {
		t.Fatalf("outcomes=%#v, want one structured error", outcomes)
	}
	if !strings.Contains(outcomes[0].Error, "goal_id") {
		t.Fatalf("the error does not name the missing argument: %q", outcomes[0].Error)
	}
	if host.runStatus().Status != persistence.RunSucceeded && host.runStatus().Status != persistence.RunRunning {
		t.Fatal("invalid tool arguments must not fail the run")
	}
}

// TestHarnessRunUnknownToolFedBack covers a hallucinated tool name: a structured error, not a
// crash and not a failed run.
func TestHarnessRunUnknownToolFedBack(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "我没有那个工具。"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "删库")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	outcomes := host.executeCalls([]agent.ToolCall{{ID: "c1", Name: "delete_everything", Arguments: `{}`}})
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Error, "unknown tool") {
		t.Fatalf("outcomes=%#v, want an unknown-tool error", outcomes)
	}
}

// TestHarnessRunToolTimeoutFedBack covers §6: a tool that exceeds its budget returns an error to
// the model and the run keeps going.
func TestHarnessRunToolTimeoutFedBack(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c1", Name: "slow_tool", Arguments: `{}`}}},
		scriptedTurn{text: "工具超时了，我换个办法。"},
	)
	f := newFixture(t)
	registry, err := NewToolRegistry(append(NewReadonlyTools(f.app), slowTool{delay: 200 * time.Millisecond})...)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewAgentService(f.app,
		WithCredentialsResolver(upstream.resolver()),
		WithToolRegistry(registry),
		WithLoopLimits(LoopLimits{MaxTurns: 4, WallClock: time.Minute, ToolTimeout: 10 * time.Millisecond}),
	)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "慢工具")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	outcomes := host.executeCalls([]agent.ToolCall{{ID: "c1", Name: "slow_tool", Arguments: `{}`}})
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Error, "timed out") {
		t.Fatalf("outcomes=%#v, want a timeout error", outcomes)
	}
	host.drive(2)
	if host.runStatus().Status != persistence.RunSucceeded {
		t.Fatal("a tool timeout must not fail the whole run")
	}
}

// --- limits and error classification (§6, §14) ---

// TestHarnessRunTurnLimitEndsRun covers §6: the turn budget is still enforced server-side now
// that the loop runs in a host, and running out of turns is a graceful end, not an error.
func TestHarnessRunTurnLimitEndsRun(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c1", Name: "list_goals", Arguments: `{}`}}},
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c2", Name: "list_goals", Arguments: `{}`}}},
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "c3", Name: "list_goals", Arguments: `{}`}}},
	)
	f, svc := harnessFixture(t, upstream, WithLoopLimits(LoopLimits{MaxTurns: 2, WallClock: time.Minute, ToolTimeout: time.Second}))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "无限循环")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	for turn := 0; turn < 2; turn++ {
		if !host.driveOneTurn() {
			t.Fatalf("turn %d asked for no tool", turn)
		}
	}
	_, err := host.modelCall("再一次")
	proxyErr, ok := err.(*ProxyError)
	if !ok {
		t.Fatalf("third model call error=%v, want a MAX_TURNS proxy error", err)
	}
	if proxyErr.Code != "MAX_TURNS" {
		t.Fatalf("code=%q, want MAX_TURNS", proxyErr.Code)
	}
	if calls := upstream.callCount(); calls != 2 {
		t.Fatalf("upstream calls=%d, want exactly MaxTurns=2", calls)
	}
	run := host.runStatus()
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("turn-limit run status=%q, want succeeded (a budget is not an error)", run.Status)
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, runID)), "轮次上限") {
		t.Fatal("the user was not told the turn limit was reached")
	}
}

// TestHarnessRunWallClockTimeout covers §6: the wall clock is enforced at the proxy, so a host
// that keeps asking for turns cannot run forever.
func TestHarnessRunWallClockTimeout(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f, svc := harnessFixture(t, upstream, WithLoopLimits(LoopLimits{MaxTurns: 8, WallClock: time.Nanosecond, ToolTimeout: time.Second}))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "超时")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	_, err := host.modelCall("超时")
	proxyErr, ok := err.(*ProxyError)
	if !ok || proxyErr.Code != "RUN_TIMEOUT" {
		t.Fatalf("error=%v, want a RUN_TIMEOUT proxy error", err)
	}
	run := host.runStatus()
	if run.Status != persistence.RunFailed || run.ErrorCode != "RUN_TIMEOUT" {
		t.Fatalf("run=%q code=%q, want failed/RUN_TIMEOUT", run.Status, run.ErrorCode)
	}
	if calls := upstream.callCount(); calls != 0 {
		t.Fatalf("the model was called %d times after the deadline", calls)
	}
}

// TestHarnessRunNoToolSupportFails covers §5.1.1: a model that cannot do tool calling fails
// loudly instead of degrading to a single-turn reply.
func TestHarnessRunNoToolSupportFails(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"model does not support tools","type":"invalid_request_error"}}`,
	})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "你好")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	_, err := host.modelCall("你好")
	proxyErr, ok := err.(*ProxyError)
	if !ok || proxyErr.Code != "PROVIDER_NO_TOOL_SUPPORT" {
		t.Fatalf("error=%v, want PROVIDER_NO_TOOL_SUPPORT", err)
	}
	run := host.runStatus()
	if run.Status != persistence.RunFailed || run.ErrorCode != "PROVIDER_NO_TOOL_SUPPORT" {
		t.Fatalf("run=%q code=%q, want failed/PROVIDER_NO_TOOL_SUPPORT", run.Status, run.ErrorCode)
	}
}

// TestHarnessRunProviderErrorIsReportedByHost covers §3.6 and §6: a transient upstream failure
// is handed back to the host, which may retry; when it gives up it reports the turn as failed and
// the run ends PROVIDER_ERROR.
func TestHarnessRunProviderErrorIsReportedByHost(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{status: 502, body: `{"error":{"message":"upstream exploded"}}`})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "你好")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	_, err := host.modelCall("你好")
	proxyErr, ok := err.(*ProxyError)
	if !ok || proxyErr.Code != "PROVIDER_ERROR" {
		t.Fatalf("error=%v, want PROVIDER_ERROR", err)
	}
	if proxyErr.Fatal {
		t.Fatal("a transient provider error must not end the run: libfx retries once (§3.6)")
	}
	if status := host.runStatus().Status; status != persistence.RunRunning {
		t.Fatalf("run status=%q after a transient error, want running", status)
	}
	// The host gives up and reports the failed turn (§5.1).
	if err := svc.CompleteRun(context.Background(), host.principal, Completion{
		StopReason: "error", ErrorMessage: "upstream exploded",
	}); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
	run := host.runStatus()
	if run.Status != persistence.RunFailed || run.ErrorCode != "PROVIDER_ERROR" {
		t.Fatalf("run=%q code=%q, want failed/PROVIDER_ERROR", run.Status, run.ErrorCode)
	}
}

// TestHarnessRunCancellation covers §5.2: a cancellation reaches a run through three channels,
// and the run ends cancelled whichever one arrives first.
func TestHarnessRunCancellation(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f, svc := harnessFixture(t, upstream)
	clock := newTestClock()
	svc = NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()), WithClock(clock.At))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "取消我")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	if err := svc.RequestCancellation(context.Background(), f.user.ID, runID); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	if status := host.runStatus().Status; status != persistence.RunCancelling {
		t.Fatalf("run status=%q, want cancelling", status)
	}
	// Channel 1: the heartbeat tells an idle host.
	beat, err := svc.HarnessHeartbeat(context.Background(), host.principal)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !beat.CancelRequested {
		t.Fatal("the heartbeat did not carry the cancellation")
	}
	// Channel 3: a model call is refused, so no new tokens are spent.
	if _, err := host.modelCall("取消我"); err == nil {
		t.Fatal("a cancelled run still proxied a model call")
	}
	if calls := upstream.callCount(); calls != 0 {
		t.Fatalf("the model was called %d times after cancellation", calls)
	}
	// The host confirms by reporting its turn as cancelled.
	if err := svc.CompleteRun(context.Background(), host.principal, Completion{StopReason: "cancelled"}); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
	if status := host.runStatus().Status; status != persistence.RunCancelled {
		t.Fatalf("run status=%q, want cancelled", status)
	}
}

// TestHarnessCancellationIsFinishedByTheReaper covers the other half of §5.2: a host that never
// confirms a cancellation must not leave the run in cancelling forever.
func TestHarnessCancellationIsFinishedByTheReaper(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f := newFixture(t)
	clock := newTestClock()
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()), WithClock(clock.At))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "取消我")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	if err := svc.RequestCancellation(context.Background(), f.user.ID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReapHarnessRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := host.runStatus().Status; status != persistence.RunCancelling {
		t.Fatalf("status=%q immediately after cancelling, want cancelling (inside the grace period)", status)
	}
	clock.Advance(HarnessCancelGrace * 2)
	if _, err := svc.ReapHarnessRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := host.runStatus().Status; status != persistence.RunCancelled {
		t.Fatalf("status=%q after the grace period, want cancelled", status)
	}
}

// --- the run's own state ---

// TestHarnessRunStateCheckpointIsValidJSON asserts the saved resume snapshot is well-formed and
// reflects the rendered messages (agent-impl.md §2.8, §3.1).
func TestHarnessRunStateCheckpointIsValidJSON(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "答案"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "问题")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	host.drive(2)

	run := host.runStatus()
	var state map[string]any
	if err := json.Unmarshal([]byte(run.StateJSON), &state); err != nil {
		t.Fatalf("state_json is not valid JSON: %v", err)
	}
	if running, _ := state["isRunning"].(bool); running {
		t.Fatal("isRunning left true in the saved state")
	}
	msgs, _ := state["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("saved state has %d messages, want 2", len(msgs))
	}
	if run.CheckpointSeq <= 0 {
		t.Fatalf("checkpoint_seq=%d, want > 0", run.CheckpointSeq)
	}
}

// TestWasmRunIsNotExecutedByTheWorker covers §1.2's central hazard: a browser-driven run must
// never become a job, or the Worker would execute the same run a second time.
func TestWasmRunIsNotExecutedByTheWorker(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "浏览器在跑"})
	f, svc := harnessFixture(t, upstream)
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我推进论文")

	worker := NewWorker(f.app, time.Millisecond).WithAgentRunner(svc)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if calls := upstream.callCount(); calls != 0 {
		t.Fatalf("the worker drove a wasm run: %d upstream calls", calls)
	}
	if status := runStatus(t, svc, f.user.ID, runID).Status; status != persistence.RunQueued {
		t.Fatalf("run status=%q, want queued until a host picks it up", status)
	}
}

// TestServerDrivenRunStillExecutesViaWorker covers the fallback that keeps the product usable
// without a model: POST /agent/commands with no configured provider creates a job, and the Worker
// produces the labelled deterministic reply (README: "LLM 未配置时使用确定性本地 Provider").
func TestServerDrivenRunStillExecutesViaWorker(t *testing.T) {
	f := newFixture(t)
	svc := NewAgentService(f.app)
	// A host asks for wasm, but with no model there is nothing to drive, so the server keeps the
	// run (§1.2).
	submitted, err := svc.SubmitCommands(context.Background(), f.user.ID, CommandsRequest{
		HarnessMode: HarnessModeWASM,
		Commands:    []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "没有模型也要能用"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.HarnessMode != "" {
		t.Fatalf("harness mode=%q, want the server-driven path when no model is configured", submitted.HarnessMode)
	}
	var job persistence.AgentJob
	if err := f.store.DB.Where("subject_id = ?", submitted.RunID).First(&job).Error; err != nil {
		t.Fatalf("a server-driven run must create the job the Worker executes: %v", err)
	}

	worker := NewWorker(f.app, time.Millisecond).WithAgentRunner(svc)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		f.store.DB.Model(&persistence.AgentJob{}).Where("id = ?", job.ID).Pluck("status", &status)
		if status == "succeeded" || status == "failed" {
			break
		}
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	var finalJob persistence.AgentJob
	if err := f.store.DB.Where("id = ?", job.ID).First(&finalJob).Error; err != nil {
		t.Fatal(err)
	}
	if finalJob.Status != "succeeded" {
		t.Fatalf("job status=%q error=%q, want succeeded", finalJob.Status, finalJob.ErrorMessage)
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("run did not succeed via worker")
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, job.SubjectID)), "未配置模型") {
		t.Fatal("the deterministic reply was not produced")
	}
}

// TestSidecarModeWithoutSidecarIsRefused covers §1.2: asking for a sidecar that is not deployed
// must fail at submission rather than queue a job nobody will ever run.
func TestSidecarModeWithoutSidecarIsRefused(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f, svc := harnessFixture(t, upstream)
	_, err := svc.SubmitCommands(context.Background(), f.user.ID, CommandsRequest{
		HarnessMode: HarnessModeSidecar,
		Commands:    []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "你好"}}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "harness is unavailable") {
		t.Fatalf("err=%v, want HARNESS_UNAVAILABLE", err)
	}
}

// TestUnknownHarnessModeIsRejected covers §1.2: the mode decides who drives, and an unknown value
// is a client error rather than a silent fallback.
func TestUnknownHarnessModeIsRejected(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f, svc := harnessFixture(t, upstream)
	_, err := svc.SubmitCommands(context.Background(), f.user.ID, CommandsRequest{
		HarnessMode: "carrier-pigeon",
		Commands:    []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "你好"}}}}},
	})
	if err == nil {
		t.Fatal("an unknown harness_mode was accepted")
	}
}
