package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// scriptedProvider is a test ChatProvider returning a fixed turn sequence. Turn 0
// proposes a task-tree patch for goalID; turn 1 (after approval resumes the run)
// returns a final text answer. goalID is set after the goal is created, before
// the first command, since the provider is shared by pointer with the resolver.
type scriptedProvider struct {
	goalID string
	calls  int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Chat(ctx context.Context, req agent.ChatRequest, sink agent.ChatSink) (agent.ChatResult, error) {
	i := p.calls
	p.calls++
	if i == 0 {
		if sink != nil {
			_ = sink.TextDelta(ctx, "我建议新增一个任务：")
		}
		patch := []map[string]any{{
			"op": "create", "type": "task", "client_ref": "n1",
			"title": "补充对照实验", "success_criteria": "得到可复现的对照结果", "minimum_action": "打开脚本跑一次基线",
		}}
		encoded, _ := json.Marshal(map[string]any{"goal_id": p.goalID, "instruction": "拆解下一步", "patch": patch})
		return agent.ChatResult{
			FinishReason: "tool_calls",
			Text:         "我建议新增一个任务：",
			ToolCalls:    []agent.ToolCall{{ID: "call_http_propose", Name: "propose_task_tree_patch", Arguments: string(encoded)}},
		}, nil
	}
	if sink != nil {
		_ = sink.TextDelta(ctx, "提案已处理，继续推进。")
	}
	return agent.ChatResult{FinishReason: "stop", Text: "提案已处理，继续推进。"}, nil
}

// newApprovalAPI builds a test API whose agent uses a scripted provider, plus a
// goal bound to that provider. It returns the api, the goal, and the provider.
func newApprovalAPI(t *testing.T, idemSuffix string) (testAPI, persistence.Goal, *scriptedProvider) {
	t.Helper()
	provider := &scriptedProvider{}
	api := newTestAPIWithAgent(t, application.WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return provider, nil }))
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-" + idemSuffix})
	if goalResp.Code != http.StatusCreated {
		t.Fatalf("create goal=%d %s", goalResp.Code, goalResp.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	provider.goalID = goal.ID
	return api, goal, provider
}

// toolResultBody builds an add-tool-result command body carrying an approval
// decision, matching the frontend's addToolResult payload (§7.1).
func toolResultBody(toolCallID, decision, reason string) map[string]any {
	result := map[string]any{"decision": decision}
	if reason != "" {
		result["reason"] = reason
	}
	encoded, _ := json.Marshal(result)
	return map[string]any{
		"commands": []any{map[string]any{
			"type":       "add-tool-result",
			"toolCallId": toolCallID,
			"toolName":   "propose_task_tree_patch",
			"result":     json.RawMessage(encoded),
		}},
	}
}

func countGoalTasks(t *testing.T, api testAPI, userID, goalID string) int64 {
	t.Helper()
	var n int64
	if err := api.store.DB.Model(&persistence.Task{}).Where("user_id = ? AND goal_id = ?", userID, goalID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// findProposalToolCallID extracts the propose_task_tree_patch tool-call id from
// reconstructed stream state.
func findProposalToolCallID(t *testing.T, state map[string]any) string {
	t.Helper()
	msgs, _ := state["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in state")
	}
	last := msgs[len(msgs)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	for _, partAny := range parts {
		part := partAny.(map[string]any)
		name, _ := part["toolName"].(string)
		if part["type"] == "tool-call" && strings.Contains(name, "propose") {
			id, _ := part["toolCallId"].(string)
			return id
		}
	}
	t.Fatalf("no proposal tool-call part in %#v", last)
	return ""
}

// TestAgentApprovalFlowEndToEnd drives the full §7 approval lifecycle over HTTP:
// a proposal pauses the run at awaiting_approval with zero business writes, then
// an add-tool-result approve receipt applies it synchronously, resumes the SAME
// run, and streams the continuation to a completed run.
func TestAgentApprovalFlowEndToEnd(t *testing.T) {
	api, goal, _ := newApprovalAPI(t, "approve")
	cancel := startAgentWorker(t, api)
	defer cancel()

	tasksBefore := countGoalTasks(t, api, api.user.ID, goal.ID)

	// 1) A message makes the model propose a task-tree patch.
	first := api.do(t, http.MethodPost, "/api/v1/agent/commands", commandsBody("帮我拆解论文下一步"), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("commands status=%d body=%s", first.Code, first.Body.String())
	}
	stream := parseSSE(t, first.Body.String())
	if !stream.done {
		t.Fatal("proposal stream did not end with [DONE]")
	}
	state := applyOps(t, stream.ops)

	if running, _ := state["isRunning"].(bool); running {
		t.Fatal("isRunning true while awaiting approval")
	}
	threadID, _ := getAt(state, []string{"fasttask", "threadId"})
	if threadID == nil {
		t.Fatal("missing fasttask.threadId")
	}
	pending, _ := getAt(state, []string{"fasttask", "pendingProposals"})
	if pendingList, _ := pending.([]any); len(pendingList) != 1 {
		t.Fatalf("pendingProposals=%v, want exactly 1", pending)
	}
	msgs, _ := state["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["status"].(map[string]any)["type"] != "requires-action" {
		t.Fatalf("assistant status=%v, want requires-action", last["status"])
	}
	var approvalStatus any
	toolCallID := findProposalToolCallID(t, state)
	for _, partAny := range last["parts"].([]any) {
		part := partAny.(map[string]any)
		if part["type"] == "tool-call" && part["toolCallId"] == toolCallID {
			if approval, ok := part["approval"].(map[string]any); ok {
				approvalStatus = approval["status"]
			}
		}
	}
	if approvalStatus != "pending" {
		t.Fatalf("tool-call approval=%v, want pending", approvalStatus)
	}
	// Zero business writes while awaiting approval (invariant 1, §10).
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore {
		t.Fatalf("tasks=%d while awaiting approval, want %d (no business writes)", got, tasksBefore)
	}

	// 2) Approve via add-tool-result: applies synchronously, resumes the same run.
	approve := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "approve", ""), nil)
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approve.Code, approve.Body.String())
	}
	if !parseSSE(t, approve.Body.String()).done {
		t.Fatal("approve stream did not end with [DONE]")
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore+1 {
		t.Fatalf("tasks=%d after approve, want %d", got, tasksBefore+1)
	}
	var applied int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'applied'", api.user.ID).Count(&applied)
	if applied != 1 {
		t.Fatalf("applied proposals=%d, want 1", applied)
	}
	// The resumed run reached a terminal state under the SAME run id (§7.2).
	var run persistence.AgentRun
	if err := api.store.DB.Where("thread_id = ?", threadID).Order("created_at DESC").First(&run).Error; err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q after approve+resume, want succeeded", run.Status)
	}
	var runs int64
	api.store.DB.Model(&persistence.AgentRun{}).Where("thread_id = ?", threadID).Count(&runs)
	if runs != 1 {
		t.Fatalf("runs on thread=%d, want 1 (approval must reuse the same run, §7.2)", runs)
	}
}

// TestAgentApprovalRejectOverHTTP asserts a reject receipt records the reason,
// writes no tasks, and resumes the run (§7).
func TestAgentApprovalRejectOverHTTP(t *testing.T) {
	api, goal, _ := newApprovalAPI(t, "reject")
	cancel := startAgentWorker(t, api)
	defer cancel()

	tasksBefore := countGoalTasks(t, api, api.user.ID, goal.ID)
	first := api.do(t, http.MethodPost, "/api/v1/agent/commands", commandsBody("拆解论文"), nil)
	state := applyOps(t, parseSSE(t, first.Body.String()).ops)
	toolCallID := findProposalToolCallID(t, state)

	reject := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "reject", "拆得太粗"), nil)
	if reject.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%s", reject.Code, reject.Body.String())
	}
	if !parseSSE(t, reject.Body.String()).done {
		t.Fatal("reject stream did not end with [DONE]")
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore {
		t.Fatalf("tasks=%d after reject, want unchanged %d", got, tasksBefore)
	}
	var rejected int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'rejected'", api.user.ID).Count(&rejected)
	if rejected != 1 {
		t.Fatalf("rejected proposals=%d, want 1", rejected)
	}
}

// TestAgentApprovalDuplicateReceipt409OverHTTP asserts a second receipt for the
// same toolCallId returns 409 and does not re-apply (§7.4).
func TestAgentApprovalDuplicateReceipt409OverHTTP(t *testing.T) {
	api, goal, _ := newApprovalAPI(t, "dup")
	cancel := startAgentWorker(t, api)
	defer cancel()

	first := api.do(t, http.MethodPost, "/api/v1/agent/commands", commandsBody("拆解论文"), nil)
	state := applyOps(t, parseSSE(t, first.Body.String()).ops)
	toolCallID := findProposalToolCallID(t, state)

	if resp := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "approve", ""), nil); resp.Code != http.StatusOK {
		t.Fatalf("first approve=%d %s", resp.Code, resp.Body.String())
	}
	tasksAfterFirst := countGoalTasks(t, api, api.user.ID, goal.ID)
	dup := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "approve", ""), nil)
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate receipt=%d, want 409 body=%s", dup.Code, dup.Body.String())
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksAfterFirst {
		t.Fatalf("tasks=%d after duplicate, want unchanged %d", got, tasksAfterFirst)
	}
}

// TestAgentApprovalCrossUserToolCall404OverHTTP asserts an approval receipt is
// re-authenticated: another user's toolCallId is not-found (§7.4, §10).
func TestAgentApprovalCrossUserToolCall404OverHTTP(t *testing.T) {
	api, _, _ := newApprovalAPI(t, "crossuser")
	cancel := startAgentWorker(t, api)
	defer cancel()

	first := api.do(t, http.MethodPost, "/api/v1/agent/commands", commandsBody("拆解论文"), nil)
	state := applyOps(t, parseSSE(t, first.Body.String()).ops)
	toolCallID := findProposalToolCallID(t, state)

	otherToken := agentSecondToken(t, api)
	resp := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, "approve", ""), map[string]string{"Authorization": "Bearer " + otherToken})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("cross-user approval=%d, want 404 body=%s", resp.Code, resp.Body.String())
	}
	// The owner's proposal is still pending (the cross-user attempt resolved nothing).
	var pending int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'pending'", api.user.ID).Count(&pending)
	if pending != 1 {
		t.Fatalf("pending proposals=%d, want 1", pending)
	}
}
