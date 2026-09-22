package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// scriptedTurn is one model turn the fake provider returns.
type scriptedTurn struct {
	text      string
	toolCalls []agent.ToolCall
	err       error
}

// fakeChat is a scripted agent.ChatProvider. It records every request so tests
// can assert what history and tools the loop advertised, and what tool results
// were fed back.
type fakeChat struct {
	turns    []scriptedTurn
	calls    int
	requests []agent.ChatRequest
}

func (f *fakeChat) Name() string { return "fake-chat" }

func (f *fakeChat) Chat(ctx context.Context, req agent.ChatRequest, sink agent.ChatSink) (agent.ChatResult, error) {
	f.requests = append(f.requests, req)
	i := f.calls
	f.calls++
	if i >= len(f.turns) {
		return agent.ChatResult{FinishReason: "stop"}, nil
	}
	turn := f.turns[i]
	if turn.err != nil {
		return agent.ChatResult{}, turn.err
	}
	if turn.text != "" && sink != nil {
		for _, delta := range splitDeltas(turn.text, 8) {
			if err := sink.TextDelta(ctx, delta); err != nil {
				return agent.ChatResult{}, err
			}
		}
	}
	reason := "stop"
	if len(turn.toolCalls) > 0 {
		reason = "tool_calls"
	}
	return agent.ChatResult{FinishReason: reason, Text: turn.text, ToolCalls: turn.toolCalls}, nil
}

// loopFixture wires an AgentService with a scripted provider over the standard
// app fixture (one user, one goal "Finish paper", four tasks).
func loopFixture(t *testing.T, fake agent.ChatProvider, opts ...AgentOption) (fixture, *AgentService) {
	t.Helper()
	f := newFixture(t)
	base := []AgentOption{WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil })}
	base = append(base, opts...)
	return f, NewAgentService(f.app, base...)
}

// submitJob creates a run via SubmitCommands and returns the AgentJob the Worker
// would execute.
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

// TestToolLoopNoToolCallsSucceeds is the phase C happy path: the model answers
// directly, the run succeeds, and the streamed text is persisted.
func TestToolLoopNoToolCallsSucceeds(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{text: "先打开 Overleaf，写下第一段。"}}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "帮我推进论文")

	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", run.Status)
	}
	parts := assistantParts(t, svc, f.user.ID, job.SubjectID)
	if got := concatText(parts); got != "先打开 Overleaf，写下第一段。" {
		t.Fatalf("assistant text=%q", got)
	}
	if fake.calls != 1 {
		t.Fatalf("model calls=%d, want 1 (no tool loop)", fake.calls)
	}
	// The server-side system prompt is used, never a client-supplied one (§2.2).
	first := fake.requests[0]
	if first.Messages[0].Role != "system" || !strings.Contains(first.Messages[0].Content, "最多三个") {
		t.Fatalf("system prompt not server-owned: %#v", first.Messages[0])
	}
	// Readonly + proposal tools are advertised (§6). Phase D adds the task-tree
	// proposal tool and phase E adds the daily-plan proposal tool, so the loop
	// offers 7 readonly + 2 proposal = 9.
	if len(first.Tools) != 9 {
		t.Fatalf("advertised %d tools, want 9 (7 readonly + 2 proposal)", len(first.Tools))
	}
	advertised := map[string]bool{}
	for _, tool := range first.Tools {
		advertised[tool.Name] = true
	}
	if !advertised["propose_task_tree_patch"] || !advertised["propose_daily_plan"] {
		t.Fatal("proposal tools not advertised to the model")
	}
}

// TestToolLoopExecutesReadonlyToolAndFeedsBack covers §6: a readonly tool call is
// executed outside a transaction, its result re-enters the model context, and the
// loop continues to a final answer.
func TestToolLoopExecutesReadonlyToolAndFeedsBack(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "让我看看你的目标。", toolCalls: []agent.ToolCall{{ID: "call_1", Name: "list_goals", Arguments: `{"status":"active"}`}}},
		{text: "你有一个活跃目标：Finish paper。"},
	}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "我有哪些目标")

	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("model calls=%d, want 2 (tool turn + answer turn)", fake.calls)
	}
	// The second request must carry the tool result back to the model.
	second := fake.requests[1]
	var toolMsg *agent.ChatMessage
	for i := range second.Messages {
		if second.Messages[i].Role == "tool" {
			toolMsg = &second.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message fed back: %#v", second.Messages)
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Fatalf("tool message id=%q", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "Finish paper") {
		t.Fatalf("tool result did not reach the model: %q", toolMsg.Content)
	}
	// The assistant message records both a tool-call part and text.
	parts := assistantParts(t, svc, f.user.ID, job.SubjectID)
	var sawToolCall bool
	for _, p := range parts {
		if p.Type == "tool-call" && p.ToolName == "list_goals" {
			sawToolCall = true
			if p.ResultJSON == "" {
				t.Fatal("tool-call part has no persisted result")
			}
		}
	}
	if !sawToolCall {
		t.Fatalf("no tool-call part persisted: %#v", parts)
	}
	if !strings.Contains(concatText(parts), "Finish paper") {
		t.Fatalf("final answer not persisted: %q", concatText(parts))
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("run did not succeed")
	}
}

// TestToolLoopRejectsIdentityArgument covers §10: a model that smuggles user_id
// gets a structured error fed back, and the tool never executes with it.
func TestToolLoopRejectsIdentityArgument(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "list_goals", Arguments: `{"user_id":"victim"}`}}},
		{text: "抱歉，我不能那样做。"},
	}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "查看别人的目标")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	second := fake.requests[1]
	var toolContent string
	for _, m := range second.Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "forbidden") && !strings.Contains(toolContent, "identity") {
		t.Fatalf("identity argument was not rejected in the fed-back result: %q", toolContent)
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("run should still succeed after a rejected tool arg")
	}
}

// TestToolLoopFeedsBackInvalidArgs covers §10: illegal tool arguments do not crash
// the run; a structured error returns to the model so it can self-correct.
func TestToolLoopFeedsBackInvalidArgs(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "get_task_tree", Arguments: `{}`}}}, // missing goal_id
		{text: "我需要 goal_id。"},
	}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "看任务树")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	var toolContent string
	for _, m := range fake.requests[1].Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "goal_id") {
		t.Fatalf("invalid-arg error not fed back: %q", toolContent)
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("run should survive invalid tool args")
	}
}

// TestToolLoopUnknownToolFedBack asserts an hallucinated tool name yields a
// structured error, not a crash.
func TestToolLoopUnknownToolFedBack(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "delete_everything", Arguments: `{}`}}},
		{text: "我没有那个工具。"},
	}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "删库")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	var toolContent string
	for _, m := range fake.requests[1].Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "unknown tool") {
		t.Fatalf("unknown-tool error not fed back: %q", toolContent)
	}
}

// TestToolLoopTurnLimitEndsRun covers §6: exceeding the turn budget ends the run
// and tells the user, rather than looping forever or failing.
func TestToolLoopTurnLimitEndsRun(t *testing.T) {
	always := []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "list_goals", Arguments: `{}`}}},
		{toolCalls: []agent.ToolCall{{ID: "c2", Name: "list_goals", Arguments: `{}`}}},
		{toolCalls: []agent.ToolCall{{ID: "c3", Name: "list_goals", Arguments: `{}`}}},
	}
	fake := &fakeChat{turns: always}
	f, svc := loopFixture(t, fake, WithLoopLimits(LoopLimits{MaxTurns: 2, WallClock: time.Minute, ToolTimeout: time.Second}))
	job := submitJob(t, svc, f.store, f.user.ID, "无限循环")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("model calls=%d, want exactly MaxTurns=2", fake.calls)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("turn-limit run status=%q, want succeeded (graceful end)", run.Status)
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, job.SubjectID)), "轮次上限") {
		t.Fatal("user was not told the turn limit was reached")
	}
}

// TestToolLoopWallClockTimeout covers §6: exceeding the wall clock fails the run
// with RUN_TIMEOUT.
func TestToolLoopWallClockTimeout(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{text: "不会到达"}}}
	f, svc := loopFixture(t, fake, WithLoopLimits(LoopLimits{MaxTurns: 8, WallClock: time.Nanosecond, ToolTimeout: time.Second}))
	job := submitJob(t, svc, f.store, f.user.ID, "超时")
	_, err := svc.ExecuteRun(context.Background(), job)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunFailed || run.ErrorCode != "RUN_TIMEOUT" {
		t.Fatalf("run=%q code=%q, want failed/RUN_TIMEOUT", run.Status, run.ErrorCode)
	}
	if fake.calls != 0 {
		t.Fatalf("model was called %d times after the deadline", fake.calls)
	}
}

// TestToolLoopNoToolSupportFails covers §5.1.1: a model that cannot do tool
// calling fails loudly with PROVIDER_NO_TOOL_SUPPORT instead of degrading.
func TestToolLoopNoToolSupportFails(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{err: agent.ErrNoToolSupport}}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "你好")
	if _, err := svc.ExecuteRun(context.Background(), job); err == nil {
		t.Fatal("expected an error")
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunFailed || run.ErrorCode != "PROVIDER_NO_TOOL_SUPPORT" {
		t.Fatalf("run=%q code=%q, want failed/PROVIDER_NO_TOOL_SUPPORT", run.Status, run.ErrorCode)
	}
}

// TestToolLoopProviderErrorFails covers §6 error classification: a generic
// provider error fails the run with PROVIDER_ERROR.
func TestToolLoopProviderErrorFails(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{err: errors.New("upstream 502")}}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "你好")
	if _, err := svc.ExecuteRun(context.Background(), job); err == nil {
		t.Fatal("expected an error")
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunFailed || run.ErrorCode != "PROVIDER_ERROR" {
		t.Fatalf("run=%q code=%q, want failed/PROVIDER_ERROR", run.Status, run.ErrorCode)
	}
}

// TestToolLoopCancellation covers §6: cancel_requested is checked before each
// turn and ends the run as cancelled.
func TestToolLoopCancellation(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{text: "不会到达"}}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "取消我")
	// Set cancel_requested on the job row before executing.
	if err := f.store.DB.Model(&persistence.AgentJob{}).Where("id = ?", job.ID).
		Update("cancel_requested", true).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunCancelled {
		t.Fatalf("run status=%q, want cancelled", run.Status)
	}
	if fake.calls != 0 {
		t.Fatalf("model was called %d times after cancellation", fake.calls)
	}
}

// TestToolLoopToolTimeoutFedBack covers §6: a tool exceeding its timeout returns
// an error to the model and the loop continues (not a run failure).
func TestToolLoopToolTimeoutFedBack(t *testing.T) {
	f := newFixture(t)
	slow := slowTool{delay: 200 * time.Millisecond}
	registry, err := NewToolRegistry(append(NewReadonlyTools(f.app), slow)...)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeChat{turns: []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "slow_tool", Arguments: `{}`}}},
		{text: "工具超时了，我换个办法。"},
	}}
	svc := NewAgentService(f.app,
		WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }),
		WithToolRegistry(registry),
		WithLoopLimits(LoopLimits{MaxTurns: 4, WallClock: time.Minute, ToolTimeout: 10 * time.Millisecond}),
	)
	job := submitJob(t, svc, f.store, f.user.ID, "慢工具")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	var toolContent string
	for _, m := range fake.requests[1].Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "timed out") {
		t.Fatalf("tool timeout not fed back: %q", toolContent)
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("a tool timeout must not fail the whole run")
	}
}

// slowTool blocks longer than the configured tool timeout.
type slowTool struct{ delay time.Duration }

func (slowTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "slow_tool", Description: "slow", Parameters: objectSchema(nil)}
}
func (slowTool) Level() ToolLevel { return ToolReadonly }
func (s slowTool) Execute(ctx context.Context, _ ToolContext, _ map[string]any) (ToolResult, error) {
	select {
	case <-time.After(s.delay):
		return ToolResult{Result: map[string]any{"ok": true}}, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

// TestExecuteRunViaWorker proves the Worker→agent_run→loop wiring end to end: a
// queued job claimed by the Worker runs the loop and materializes no business
// writes (readonly tools only).
func TestExecuteRunViaWorker(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{toolCalls: []agent.ToolCall{{ID: "c1", Name: "list_goals", Arguments: `{}`}}},
		{text: "你有 1 个活跃目标。"},
	}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "目标概况")

	worker := NewWorker(f.app, time.Millisecond).WithAgentRunner(svc)
	// Run the worker loop until the job leaves the queue.
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
	// Readonly tools must not create proposals or tasks (§5.2, §10).
	var proposals, tasksAfter int64
	f.store.DB.Model(&persistence.Proposal{}).Where("user_id = ?", f.user.ID).Count(&proposals)
	f.store.DB.Model(&persistence.Task{}).Where("user_id = ?", f.user.ID).Count(&tasksAfter)
	if proposals != 0 {
		t.Fatalf("readonly run created %d proposals", proposals)
	}
	if int(tasksAfter) != len(f.tasks) {
		t.Fatalf("task count changed from %d to %d during a readonly run", len(f.tasks), tasksAfter)
	}
}

// TestDeterministicFallbackWhenNoProvider asserts that with no chat resolver the
// run still produces a labelled reply (README: deterministic local provider when
// no LLM is configured), keeping the product usable without a model.
func TestDeterministicFallbackWhenNoProvider(t *testing.T) {
	f := newFixture(t)
	svc := NewAgentService(f.app) // no chat resolver
	job := submitJob(t, svc, f.store, f.user.ID, "没有模型也要能用")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if runStatus(t, svc, f.user.ID, job.SubjectID).Status != persistence.RunSucceeded {
		t.Fatal("deterministic run did not succeed")
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, job.SubjectID)), "未配置模型") {
		t.Fatal("deterministic reply not produced")
	}
}

// TestToolLoopStateCheckpointIsValidJSON asserts the saved resume snapshot is
// well-formed and reflects the rendered messages (agent-impl.md §2.8, §3.1).
func TestToolLoopStateCheckpointIsValidJSON(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{{text: "答案"}}}
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "问题")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
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
