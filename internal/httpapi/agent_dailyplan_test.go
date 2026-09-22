package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// dailyPlanProvider is a test ChatProvider that proposes a daily plan. Turn 0
// proposes ALL taskIDs as candidates in REVERSE order (so the test can prove the
// program — not the agent — owns the final ordering and the top-3 cap); turn 1
// returns a final answer after approval resumes the run.
type dailyPlanProvider struct {
	taskIDs []string
	calls   int
}

func (p *dailyPlanProvider) Name() string { return "daily-plan-scripted" }

func (p *dailyPlanProvider) Chat(ctx context.Context, req agent.ChatRequest, sink agent.ChatSink) (agent.ChatResult, error) {
	i := p.calls
	p.calls++
	if i == 0 {
		if sink != nil {
			_ = sink.TextDelta(ctx, "今天建议这样安排：")
		}
		candidates := make([]map[string]any, 0, len(p.taskIDs))
		for idx := len(p.taskIDs) - 1; idx >= 0; idx-- { // reverse order on purpose
			candidates = append(candidates, map[string]any{
				"task_id":        p.taskIDs[idx],
				"minimum_action": "先做 " + p.taskIDs[idx] + " 的第一步",
				"reason":         "推进论文",
			})
		}
		encoded, _ := json.Marshal(map[string]any{"candidates": candidates, "available_minutes": 120})
		return agent.ChatResult{
			FinishReason: "tool_calls",
			Text:         "今天建议这样安排：",
			ToolCalls:    []agent.ToolCall{{ID: "call_http_plan", Name: "propose_daily_plan", Arguments: string(encoded)}},
		}, nil
	}
	if sink != nil {
		_ = sink.TextDelta(ctx, "计划已生成。")
	}
	return agent.ChatResult{FinishReason: "stop", Text: "计划已生成。"}, nil
}

// createPlanTask creates one task under the goal and returns its id.
func createPlanTask(t *testing.T, api testAPI, goalID string, priority, position int, key string) string {
	t.Helper()
	resp := api.do(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"goal_id": goalID, "type": "task", "title": fmt.Sprintf("任务 %d", position),
		"success_criteria": "可验证结果", "minimum_action": "写下第一步",
		"estimate_minutes": 25, "priority": priority, "position": position,
	}, map[string]string{"Idempotency-Key": key})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create task=%d %s", resp.Code, resp.Body.String())
	}
	var task persistence.Task
	decode(t, resp, &task)
	return task.ID
}

// planItemsFor returns the live core items of the user's most recent daily plan.
func planItemsFor(t *testing.T, api testAPI, userID string) []persistence.DailyPlanItem {
	t.Helper()
	var plan persistence.DailyPlan
	if err := api.store.DB.Where("user_id = ?", userID).Order("created_at desc, id desc").First(&plan).Error; err != nil {
		if persistence.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	var items []persistence.DailyPlanItem
	if err := api.store.DB.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position").Find(&items).Error; err != nil {
		t.Fatal(err)
	}
	return items
}

// TestAgentDailyPlanApprovalEndToEnd drives propose_daily_plan over HTTP and
// asserts the phase E invariant survives the full transport: the agent proposes
// four candidates in reverse priority order, but after approval the deterministic
// rule lands exactly the top three by priority and drops the fourth (§6, §11
// "确定性取三不被绕过"). Proposing stages no plan; approval applies synchronously
// and resumes the same run.
func TestAgentDailyPlanApprovalEndToEnd(t *testing.T) {
	provider := &dailyPlanProvider{}
	api := newTestAPIWithAgent(t, application.WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return provider, nil }))

	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-dp"})
	if goalResp.Code != http.StatusCreated {
		t.Fatalf("create goal=%d %s", goalResp.Code, goalResp.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResp, &goal)

	// Four tasks with descending priority; the deterministic rule must keep the
	// top three (priorities 100/90/80) and drop the fourth (70).
	ids := []string{
		createPlanTask(t, api, goal.ID, 100, 0, "dp-0"),
		createPlanTask(t, api, goal.ID, 90, 1, "dp-1"),
		createPlanTask(t, api, goal.ID, 80, 2, "dp-2"),
		createPlanTask(t, api, goal.ID, 70, 3, "dp-3"),
	}
	provider.taskIDs = ids

	cancel := startAgentWorker(t, api)
	defer cancel()

	// 1) A message makes the model propose a daily plan; the run pauses.
	first := api.do(t, http.MethodPost, "/api/v1/agent/commands", commandsBody("帮我安排今天"), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("commands status=%d body=%s", first.Code, first.Body.String())
	}
	stream := parseSSE(t, first.Body.String())
	if !stream.done {
		t.Fatal("proposal stream did not end with [DONE]")
	}
	state := applyOps(t, stream.ops)
	pending, _ := getAt(state, []string{"fasttask", "pendingProposals"})
	if list, _ := pending.([]any); len(list) != 1 {
		t.Fatalf("pendingProposals=%v, want exactly 1", pending)
	}
	// Invariant 1: proposing writes NO plan.
	if got := len(planItemsFor(t, api, api.user.ID)); got != 0 {
		t.Fatalf("plan items=%d before approval, want 0 (no business writes)", got)
	}
	toolCallID := findProposalToolCallID(t, state)

	// 2) Approve: the deterministic top-3 lands synchronously and the run resumes.
	approve := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "approve", ""), nil)
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approve.Code, approve.Body.String())
	}
	if !parseSSE(t, approve.Body.String()).done {
		t.Fatal("approve stream did not end with [DONE]")
	}

	items := planItemsFor(t, api, api.user.ID)
	if len(items) != 3 {
		t.Fatalf("core items=%d, want 3 (deterministic top-3 must not be bypassed over HTTP)", len(items))
	}
	present := map[string]bool{}
	for _, item := range items {
		if item.TaskID != nil {
			present[*item.TaskID] = true
		}
	}
	// The three highest-priority tasks land; the lowest-priority one is dropped
	// even though the agent proposed it first.
	for _, want := range ids[:3] {
		if !present[want] {
			t.Fatalf("top-3 task %s missing from the plan", want)
		}
	}
	if present[ids[3]] {
		t.Fatal("lowest-priority task landed; the top-3 cap was bypassed")
	}
	// The proposal is applied and the run reached a terminal state on the same thread.
	var applied int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'applied'", api.user.ID).Count(&applied)
	if applied != 1 {
		t.Fatalf("applied proposals=%d, want 1", applied)
	}
}
