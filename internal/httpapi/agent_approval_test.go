package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The §7 approval flow over HTTP, driven by a harness host (doc/harness.md §7).
//
// The receipt path is unchanged — a decision is still an add-tool-result command, still applied
// synchronously, still 409 on a duplicate and 404 across users. What changed is the timing: the
// stream stays open, the same turn continues, and no job is created.

// toolResultBody builds an add-tool-result command body carrying an approval decision, matching the
// frontend's addToolResult payload (§7.1).
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

// proposalTurns is the two-turn script every approval test needs: propose a patch for the goal,
// then answer.
func proposalTurns(goalID, answer string) []stubTurn {
	patch := []map[string]any{{
		"op": "create", "type": "task", "client_ref": "n1",
		"title": "补充对照实验", "success_criteria": "得到可复现的对照结果", "minimum_action": "打开脚本跑一次基线",
	}}
	encoded, _ := json.Marshal(map[string]any{"goal_id": goalID, "instruction": "拆解下一步", "patch": patch})
	return []stubTurn{
		{text: "我建议新增一个任务：", toolCalls: []stubCall{{id: "call_http_propose", name: "propose_task_tree_patch", arguments: string(encoded)}}},
		{text: answer},
	}
}

// newApprovalAPI builds a test API with a model stub and a goal, and starts a harness run against
// it. The returned host holds a capability token for that run.
func newApprovalAPI(t *testing.T, answer string) (testAPI, persistence.Goal, *harnessClient, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	stub := newModelStub(t)
	api := newHarnessAPI(t, stub)
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-" + strconv.FormatInt(persistence.Now().UnixNano(), 10)})
	if goalResp.Code != http.StatusCreated {
		t.Fatalf("create goal=%d %s", goalResp.Code, goalResp.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	stub.setTurns(proposalTurns(goal.ID, answer)...)

	runID, responses := startHarnessRun(t, api, "帮我拆解论文下一步")
	return api, goal, newHarnessClient(t, api, runID), responses
}

// statusesSetAt collects every value the stream set at a path, in order. A harness stream stays open
// across an approval, so the intermediate states live in the chunk sequence rather than in the final
// folded state.
func statusesSetAt(t *testing.T, ops []protocol.Operation, path ...string) []any {
	t.Helper()
	var values []any
	for _, op := range ops {
		if op.Type != protocol.OpSet || strings.Join(op.Path, "/") != strings.Join(path, "/") {
			continue
		}
		values = append(values, op.Value)
	}
	return values
}

// TestAgentApprovalFlowEndToEnd drives the full §7 lifecycle over HTTP: a proposal parks the run with
// zero business writes, an add-tool-result approve receipt applies it synchronously, the waiting host
// is handed the decision, and the SAME run and turn continue to a completed stream.
func TestAgentApprovalFlowEndToEnd(t *testing.T) {
	api, goal, host, responses := newApprovalAPI(t, "提案已处理，继续推进。")
	tasksBefore := countGoalTasks(t, api, api.user.ID, goal.ID)

	// 1) The host executes the proposal tool; the run parks and nothing is written.
	outcomes := host.modelTurn()
	if len(outcomes) != 1 {
		t.Fatalf("outcomes=%#v, want one proposal call", outcomes)
	}
	status, _ := outcomes[0]["status"].(string)
	proposalID, _ := outcomes[0]["proposal_id"].(string)
	if status != "pending" || proposalID == "" {
		t.Fatalf("outcome=%#v, want a pending proposal", outcomes[0])
	}
	if got := host.runStatus().Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", got)
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore {
		t.Fatalf("tasks=%d while awaiting approval, want %d (no business writes)", got, tasksBefore)
	}
	var jobs int64
	api.store.DB.Model(&persistence.AgentJob{}).Where("subject_id = ?", host.runID).Count(&jobs)
	if jobs != 0 {
		t.Fatalf("a harness run created %d AgentJobs (§1.2)", jobs)
	}

	// 2) The user approves. The receipt applies the proposal synchronously and answers with an
	//    immediately terminated stream, because the open one carries the continuation (§7).
	approve := host.decideApproval("call_http_propose", "approve", "")
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approve.Code, approve.Body.String())
	}
	receipt := parseSSE(t, approve.Body.String())
	if !receipt.done {
		t.Fatal("the approval receipt did not end with [DONE]")
	}
	if len(receipt.ops) != 0 {
		t.Fatalf("the receipt streamed %d operations; the open stream owns the continuation", len(receipt.ops))
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore+1 {
		t.Fatalf("tasks=%d after approve, want %d", got, tasksBefore+1)
	}
	var applied int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'applied'", api.user.ID).Count(&applied)
	if applied != 1 {
		t.Fatalf("applied proposals=%d, want 1", applied)
	}
	var runs int64
	api.store.DB.Model(&persistence.AgentRun{}).Where("thread_id = ?", host.threadID()).Count(&runs)
	if runs != 1 {
		t.Fatalf("runs on the thread=%d, want 1 (approval must reuse the same run, §7.2)", runs)
	}
	if jobs := func() int64 {
		var n int64
		api.store.DB.Model(&persistence.AgentJob{}).Where("subject_id = ?", host.runID).Count(&n)
		return n
	}(); jobs != 0 {
		t.Fatalf("the approval created %d jobs; the same turn continues in the host (§7)", jobs)
	}

	// 3) The waiting host is handed the decision and continues the same turn.
	wait := host.poll(proposalID)
	if got, _ := wait["status"].(string); got != "approved" {
		t.Fatalf("approval poll=%#v, want approved", wait)
	}
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q after the continuation, want succeeded", got)
	}

	// 4) The stream that was open the whole time ends with [DONE], and its chunk sequence shows the
	//    message passing through requires-action before completing.
	stream := parseSSE(t, streamResponse(t, responses).Body.String())
	if !stream.done {
		t.Fatal("the command stream did not end with [DONE]")
	}
	state := applyOps(t, stream.ops)
	if running, _ := state["isRunning"].(bool); running {
		t.Fatal("isRunning left true after a completed run")
	}
	msgs, _ := state["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("stream rendered %d messages, want 2", len(msgs))
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["id"] != nil && last["role"] != "assistant" {
		t.Fatalf("the last message is not the assistant's: %#v", last)
	}
	statuses := statusesSetAt(t, stream.ops, "messages", "1", "status")
	var sawRequiresAction bool
	for _, value := range statuses {
		if entry, ok := value.(map[string]any); ok && entry["type"] == "requires-action" {
			sawRequiresAction = true
		}
	}
	if !sawRequiresAction {
		t.Fatalf("the stream never marked the message requires-action: %#v", statuses)
	}
	// The pending proposal is pushed as part of the whole fasttask namespace (§2.7), which is what
	// the approval card renders from.
	var sawPendingProposal bool
	for _, value := range statusesSetAt(t, stream.ops, "fasttask") {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if list, ok := entry["pendingProposals"].([]any); ok && len(list) > 0 {
			sawPendingProposal = true
		}
	}
	if !sawPendingProposal {
		t.Fatal("the stream never pushed the pending proposal the approval card renders")
	}
	if !strings.Contains(stream.body, "call_http_propose") {
		t.Fatal("the streamed transcript lost the proposal tool call")
	}
}

// TestAgentApprovalRejectOverHTTP asserts a reject receipt records the reason, writes no tasks, and
// hands the reason to the waiting host so the model can re-propose in the same turn (§7).
func TestAgentApprovalRejectOverHTTP(t *testing.T) {
	api, goal, host, responses := newApprovalAPI(t, "明白，我换个拆法。")
	tasksBefore := countGoalTasks(t, api, api.user.ID, goal.ID)

	outcomes := host.modelTurn()
	proposalID, _ := outcomes[0]["proposal_id"].(string)

	reject := host.decideApproval("call_http_propose", "reject", "拆得太粗")
	if reject.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%s", reject.Code, reject.Body.String())
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksBefore {
		t.Fatalf("tasks=%d after reject, want unchanged %d", got, tasksBefore)
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
	if !strings.Contains(string(encoded), "拆得太粗") {
		t.Fatalf("the reject reason did not reach the model: %s", encoded)
	}
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
	streamResponse(t, responses)
}

// TestAgentApprovalDuplicateReceipt409OverHTTP asserts a second receipt for the same toolCallId
// returns 409 and does not re-apply (§7.4).
func TestAgentApprovalDuplicateReceipt409OverHTTP(t *testing.T) {
	api, goal, host, responses := newApprovalAPI(t, "已应用。")
	host.modelTurn()

	if resp := host.decideApproval("call_http_propose", "approve", ""); resp.Code != http.StatusOK {
		t.Fatalf("first approve=%d %s", resp.Code, resp.Body.String())
	}
	tasksAfterFirst := countGoalTasks(t, api, api.user.ID, goal.ID)
	dup := host.decideApproval("call_http_propose", "approve", "")
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate receipt=%d, want 409 body=%s", dup.Code, dup.Body.String())
	}
	if got := countGoalTasks(t, api, api.user.ID, goal.ID); got != tasksAfterFirst {
		t.Fatalf("tasks=%d after a duplicate receipt, want unchanged %d", got, tasksAfterFirst)
	}
	host.drive(2)
	streamResponse(t, responses)
}

// TestAgentApprovalCrossUserToolCall404OverHTTP asserts a receipt is re-authenticated: another user's
// toolCallId is not-found (§7.4, arch.md §12).
func TestAgentApprovalCrossUserToolCall404OverHTTP(t *testing.T) {
	api, _, host, responses := newApprovalAPI(t, "不会到达")
	host.modelTurn()

	otherToken := agentSecondToken(t, api)
	resp := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody("call_http_propose", "approve", ""),
		map[string]string{"Authorization": "Bearer " + otherToken})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("cross-user approval=%d, want 404 body=%s", resp.Code, resp.Body.String())
	}
	var pending int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'pending'", api.user.ID).Count(&pending)
	if pending != 1 {
		t.Fatalf("pending proposals=%d, want 1 (a cross-user attempt must not resolve it)", pending)
	}
	if got := host.runStatus().Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want still awaiting_approval", got)
	}
	// Let the run finish so the background stream ends.
	host.decideApproval("call_http_propose", "reject", "收尾")
	host.drive(2)
	streamResponse(t, responses)
}

// TestAgentApprovalStreamStaysOpenDuringWait covers the §7 change that is easiest to get wrong: for a
// harness run, awaiting_approval must NOT end the stream, or the client would stop rendering a
// conversation the server is still writing.
func TestAgentApprovalStreamStaysOpenDuringWait(t *testing.T) {
	_, _, host, responses := newApprovalAPI(t, "继续。")
	host.modelTurn()
	if got := host.runStatus().Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", got)
	}
	select {
	case response := <-responses:
		t.Fatalf("the stream ended while the run was awaiting approval: %s", response.Body.String())
	default:
	}
	host.decideApproval("call_http_propose", "approve", "")
	host.drive(2)
	stream := parseSSE(t, streamResponse(t, responses).Body.String())
	if !stream.done {
		t.Fatal("the stream never terminated after the run finished")
	}
	if agentStreamTerminal(persistence.RunAwaitingApproval, true) {
		t.Fatal("awaiting_approval must not terminate a harness stream")
	}
	if !agentStreamTerminal(persistence.RunAwaitingApproval, false) {
		t.Fatal("awaiting_approval must still terminate a job-driven stream (agent-impl.md §4)")
	}
}

// TestAgentApprovalWaitDoesNotCountAgainstWallClock covers §7 and §14.6: a wait longer than the run's
// wall clock must not fail the run, because time spent waiting for a human is not the model's budget.
func TestAgentApprovalWaitDoesNotCountAgainstWallClock(t *testing.T) {
	stub := newModelStub(t)
	api := newHarnessAPI(t, stub, application.WithLoopLimits(application.LoopLimits{
		MaxTurns: 4, WallClock: 50 * time.Millisecond, ToolTimeout: time.Second,
	}))
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-wallclock"})
	if goalResp.Code != http.StatusCreated {
		t.Fatalf("create goal=%d %s", goalResp.Code, goalResp.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	stub.setTurns(proposalTurns(goal.ID, "好。")...)

	runID, responses := startHarnessRun(t, api, "拆解")
	host := newHarnessClient(t, api, runID)
	outcomes := host.modelTurn()
	proposalID, _ := outcomes[0]["proposal_id"].(string)

	// The host is already waiting when the user decides, so the wait is real and gets recorded. The
	// decision happens on another goroutine because the poll holds this one open.
	codes := make(chan int, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		response := api.do(t, http.MethodPost, "/api/v1/agent/commands", toolResultBody("call_http_propose", "approve", ""), nil)
		codes <- response.Code
	}()
	wait := host.poll(proposalID)
	if code := <-codes; code != http.StatusOK {
		t.Fatalf("approve status=%d, want 200", code)
	}
	if got, _ := wait["status"].(string); got != "approved" {
		t.Fatalf("approval poll=%#v, want approved", wait)
	}
	// The wait was excluded, so the next model call is still allowed.
	host.drive(2)
	if got := host.runStatus(); got.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q code=%q, want succeeded: approval waiting must not count against the wall clock", got.Status, got.ErrorCode)
	}
	if got := host.runStatus().ApprovalWaitMs; got <= 0 {
		t.Fatalf("approval_wait_ms=%d, want the wait to have been recorded", got)
	}
	streamResponse(t, responses)
}
