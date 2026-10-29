package application

import (
	"context"
	"encoding/json"
	"github.com/FastR-D/FastTask/internal/persistence"
	"strings"
	"testing"
)

func TestDeepSeekRebuildPreservesReasoningAndParallelCalls(t *testing.T) {
	store, owner, _ := panelTestStore(t)
	svc := NewAgentService(New(store))
	ctx := context.Background()
	thread, _ := svc.repo.CreateThread(ctx, owner, nil, "thinking")
	run := &persistence.AgentRun{UserID: owner, ThreadID: thread.ID, Status: persistence.RunRunning, HarnessMode: HarnessModeWASM}
	if err := svc.repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	user := &persistence.AgentMessage{UserID: owner, ThreadID: thread.ID, RunID: run.ID, Role: "user"}
	svc.repo.CreateMessage(ctx, user)
	svc.repo.CreatePart(ctx, &persistence.AgentMessagePart{UserID: owner, MessageID: user.ID, Type: "text", Text: "查看当前安排"})
	run.ParentMessageID = &user.ID
	message := &persistence.AgentMessage{UserID: owner, ThreadID: thread.ID, RunID: run.ID, Role: "assistant"}
	svc.repo.CreateMessage(ctx, message)
	parts := []persistence.AgentMessagePart{{Type: "reasoning", Text: "provider exact reasoning"}, {Type: "text", Text: "读取两项"}}
	for _, name := range []string{"list_goals", "get_daily_plan"} {
		id := name + "-id"
		parts = append(parts, persistence.AgentMessagePart{Type: "tool-call", ToolName: name, ToolCallID: &id, ToolModelTurn: 1, ArgsJSON: `{}`, ResultJSON: `{"ok":true}`, ToolArgsReady: true})
	}
	for _, part := range parts {
		part.UserID = owner
		part.MessageID = message.ID
		if err := svc.repo.CreatePart(ctx, &part); err != nil {
			t.Fatal(err)
		}
	}
	decode := func(base, model string) map[string]any {
		t.Helper()
		body, err := svc.rewriteRequest(ctx, []byte(`{}`), &ModelCredentials{BaseURL: base, Model: model}, run)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		json.Unmarshal(body, &payload)
		return payload
	}
	payload := decode("https://api.deepseek.com/v1", "deepseek-flash")
	messages := payload["messages"].([]any)
	found := false
	for _, raw := range messages {
		m := raw.(map[string]any)
		if calls, ok := m["tool_calls"].([]any); ok {
			found = true
			if len(calls) != 2 || m["reasoning_content"] != "provider exact reasoning" || m["content"] != "读取两项" {
				t.Fatalf("response split or reasoning lost: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("no reconstructed tool response")
	}
	generic := decode("https://provider.invalid/v1", "generic-model")
	b, _ := json.Marshal(generic)
	if strings.Contains(string(b), "reasoning_content") {
		t.Fatal("DeepSeek field leaked into another provider")
	}
	svc.reasoningPersisted = false
	payload = decode("https://api.deepseek.com/v1", "deepseek-flash")
	if payload["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatal("nonpersisted thinking cannot be resumed")
	}
}
func TestContextBudgetDropsWholeParallelToolGroup(t *testing.T) {
	calls := []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}
	messages := []any{map[string]any{"role": "system", "content": "rules"}, map[string]any{"role": "assistant", "content": strings.Repeat("old", 5000), "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": "a", "content": "{}"}, map[string]any{"role": "tool", "tool_call_id": "b", "content": "{}"}, map[string]any{"role": "user", "content": "constraint"}, map[string]any{"role": "assistant", "content": "recent"}, map[string]any{"role": "user", "content": "current"}}
	bounded, err := boundModelContext(messages, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range bounded {
		if raw.(map[string]any)["role"] == "tool" {
			t.Fatal("orphaned result after compaction")
		}
	}
}
