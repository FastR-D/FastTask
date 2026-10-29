package application

import (
	"context"
	"encoding/json"
	"github.com/FastR-D/FastTask/internal/persistence"
	"strings"
	"testing"
)

func TestPlanningContextPagesAndObservedProposalBasis(t *testing.T) {
	app, owner, _, _, _ := deletionFixture(t, 4)
	ctx := context.Background()
	var goal persistence.Goal
	if err := app.Store.DB.Where("user_id = ?", owner).First(&goal).Error; err != nil {
		t.Fatal(err)
	}
	tc := ToolContext{UserID: owner}
	result, err := app.toolPlanningContext(ctx, tc, map[string]any{"goal_id": goal.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result.Result.(map[string]any)
	if snapshot["goal_id"] != goal.ID || snapshot["plan"] == nil {
		t.Fatal("missing coherent context")
	}
	page, err := app.toolGetTaskTree(ctx, tc, map[string]any{"goal_id": goal.ID, "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	v := page.Result.(map[string]any)
	if v["has_more"] != true || v["tree"] != nil || len(v["tasks"].([]map[string]any)) != 1 {
		t.Fatalf("unbounded duplicate representation: %+v", v)
	}
	svc := NewAgentService(app)
	submitted, err := svc.SubmitCommands(ctx, owner, CommandsRequest{Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "edit"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	tc.RunID = submitted.RunID
	tc.ThreadID = submitted.ThreadID
	var task persistence.Task
	app.Store.DB.Where("goal_id = ?", goal.ID).First(&task)
	args := map[string]any{"goal_id": goal.ID, "patch": []any{map[string]any{"op": "update", "target_id": task.ID, "title": "Updated title"}}}
	build := buildTaskTreePatchProposal(app)
	if _, _, _, err = build(ctx, app, tc, args); err == nil {
		t.Fatal("unobserved version accepted")
	}
	message := &persistence.AgentMessage{UserID: owner, ThreadID: submitted.ThreadID, RunID: submitted.RunID, Role: "assistant"}
	if err = svc.repo.CreateMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(snapshot)
	call := "observed-context"
	if err = svc.repo.CreatePart(ctx, &persistence.AgentMessagePart{UserID: owner, MessageID: message.ID, Type: "tool-call", ToolCallID: &call, ToolName: "get_planning_context", ArgsJSON: `{}`, ResultJSON: string(b)}); err != nil {
		t.Fatal(err)
	}
	ref, _, _, err := build(ctx, app, tc, args)
	if err != nil {
		t.Fatal(err)
	}
	if ref.BaseRevision != snapshot["tree_revision"] {
		t.Fatal("observed revision not used")
	}
	args["base_revision"] = float64(0)
	if ref.BaseRevision != 0 {
		if _, _, _, err = build(ctx, app, tc, args); err == nil {
			t.Fatal("explicit zero treated as omitted")
		}
	}
}
func TestContextBudgetRetainsConstraintsAndSignalsTruncation(t *testing.T) {
	messages := []any{map[string]any{"role": "system", "content": "rules"}, map[string]any{"role": "user", "content": "保留原方向，不增加字数指标"}, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}}, map[string]any{"role": "tool", "content": strings.Repeat("旧材料", 10000)}, map[string]any{"role": "user", "content": "current"}, map[string]any{"role": "assistant", "content": "recent"}}
	bounded, err := boundModelContext(messages, 20000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(bounded)
	if !strings.Contains(string(b), "保留原方向") || !strings.Contains(string(b), "truncated") {
		t.Fatal("constraint or truncation lost")
	}
	_, err = boundModelContext([]any{map[string]any{"role": "user", "content": strings.Repeat("constraint", 1000)}}, 20)
	if err == nil {
		t.Fatal("oversized constraints silently dropped")
	}
}
