package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// propose_daily_plan over HTTP, driven by a harness host (doc/agent-impl.md §6, §11).
//
// The invariant these tests guard did not change with the harness: the agent may propose four
// candidates in any order, and the program still lands exactly the top three by priority. What
// changed is that proposing parks the run and the same turn continues after the decision.

// dailyPlanTurns proposes ALL taskIDs as candidates in REVERSE order, so the test can prove the
// program — not the agent — owns the final ordering and the top-3 cap.
func dailyPlanTurns(taskIDs []string, answer string) []stubTurn {
	candidates := make([]map[string]any, 0, len(taskIDs))
	for idx := len(taskIDs) - 1; idx >= 0; idx-- {
		candidates = append(candidates, map[string]any{
			"task_id":        taskIDs[idx],
			"minimum_action": "先做 " + taskIDs[idx] + " 的第一步",
			"reason":         "推进论文",
		})
	}
	encoded, _ := json.Marshal(map[string]any{"candidates": candidates, "available_minutes": 120})
	return []stubTurn{
		{text: "今天建议这样安排：", toolCalls: []stubCall{{id: "call_http_plan", name: "propose_daily_plan", arguments: string(encoded)}}},
		{text: answer},
	}
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

// TestAgentDailyPlanApprovalEndToEnd drives propose_daily_plan over HTTP and asserts the invariant
// survives the full transport: four candidates proposed in reverse priority order, and after
// approval the deterministic rule lands exactly the top three and drops the fourth (§6, §11
// "确定性取三不被绕过"). Proposing stages no plan; approval applies synchronously and the same run
// continues.
func TestAgentDailyPlanApprovalEndToEnd(t *testing.T) {
	stub := newModelStub(t)
	api := newHarnessAPI(t, stub)
	suffix := strconv.FormatInt(persistence.Now().UnixNano(), 10)

	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-dp-" + suffix})
	if goalResp.Code != http.StatusCreated {
		t.Fatalf("create goal=%d %s", goalResp.Code, goalResp.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResp, &goal)

	// Four tasks with descending priority; the deterministic rule must keep the top three
	// (100/90/80) and drop the fourth (70).
	ids := []string{
		createPlanTask(t, api, goal.ID, 100, 0, "dp-0-"+suffix),
		createPlanTask(t, api, goal.ID, 90, 1, "dp-1-"+suffix),
		createPlanTask(t, api, goal.ID, 80, 2, "dp-2-"+suffix),
		createPlanTask(t, api, goal.ID, 70, 3, "dp-3-"+suffix),
	}
	stub.setTurns(dailyPlanTurns(ids, "计划已生成。")...)

	runID, responses := startHarnessRun(t, api, "帮我安排今天")
	host := newHarnessClient(t, api, runID)

	// 1) The host executes the proposal tool; the run parks and no plan exists yet.
	outcomes := host.modelTurn()
	if len(outcomes) != 1 {
		t.Fatalf("outcomes=%#v, want one daily-plan proposal", outcomes)
	}
	if got := len(planItemsFor(t, api, api.user.ID)); got != 0 {
		t.Fatalf("plan items=%d before approval, want 0 (invariant 1: no business writes)", got)
	}
	if got := host.runStatus().Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", got)
	}

	// 2) Approve: the deterministic top-3 lands synchronously.
	if response := host.decideApproval("call_http_plan", "approve", ""); response.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", response.Code, response.Body.String())
	}
	items := planItemsFor(t, api, api.user.ID)
	if len(items) != 3 {
		t.Fatalf("core items=%d, want 3 (the deterministic top-3 must not be bypassed over HTTP)", len(items))
	}
	present := map[string]bool{}
	for _, item := range items {
		if item.TaskID != nil {
			present[*item.TaskID] = true
		}
	}
	for _, want := range ids[:3] {
		if !present[want] {
			t.Fatalf("top-3 task %s missing from the plan", want)
		}
	}
	if present[ids[3]] {
		t.Fatal("the lowest-priority task landed; the top-3 cap was bypassed")
	}
	var applied int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'applied'", api.user.ID).Count(&applied)
	if applied != 1 {
		t.Fatalf("applied proposals=%d, want 1", applied)
	}

	// 3) The same turn continues to its answer and the stream ends.
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
	var runs int64
	api.store.DB.Model(&persistence.AgentRun{}).Where("thread_id = ?", host.threadID()).Count(&runs)
	if runs != 1 {
		t.Fatalf("runs on the thread=%d, want 1 (§7.2)", runs)
	}
	if stream := parseSSE(t, streamResponse(t, responses).Body.String()); !stream.done {
		t.Fatal("the command stream did not end with [DONE]")
	}
}

// TestAgentDailyPlanRejectWritesNoPlan covers the rejection half over HTTP: no plan is written, the
// proposal is rejected, and the reason reaches the waiting host (§7).
func TestAgentDailyPlanRejectWritesNoPlan(t *testing.T) {
	stub := newModelStub(t)
	api := newHarnessAPI(t, stub)
	suffix := strconv.FormatInt(persistence.Now().UnixNano(), 10)
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-dpr-" + suffix})
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	ids := []string{
		createPlanTask(t, api, goal.ID, 100, 0, "dpr-0-"+suffix),
		createPlanTask(t, api, goal.ID, 90, 1, "dpr-1-"+suffix),
	}
	stub.setTurns(dailyPlanTurns(ids, "明白，我重新安排。")...)

	runID, responses := startHarnessRun(t, api, "帮我安排今天")
	host := newHarnessClient(t, api, runID)
	outcomes := host.modelTurn()
	proposalID, _ := outcomes[0]["proposal_id"].(string)

	if response := host.decideApproval("call_http_plan", "reject", "今天没空"); response.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%s", response.Code, response.Body.String())
	}
	if got := len(planItemsFor(t, api, api.user.ID)); got != 0 {
		t.Fatalf("plan items=%d after a rejection, want 0", got)
	}
	var rejected int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'rejected'", api.user.ID).Count(&rejected)
	if rejected != 1 {
		t.Fatalf("rejected proposals=%d, want 1", rejected)
	}
	wait := host.poll(proposalID)
	if got, _ := wait["status"].(string); got != "rejected" {
		t.Fatalf("approval poll=%#v, want rejected", wait)
	}
	encoded, _ := json.Marshal(wait["result"])
	if got := string(encoded); !strings.Contains(got, "今天没空") {
		t.Fatalf("the reject reason did not reach the model: %s", got)
	}
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
	streamResponse(t, responses)
}
