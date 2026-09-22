package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The approval flow under the harness (doc/harness.md §7).
//
// What changed against the in-process loop is the timing, not the semantics: a proposal parks
// the run, the host waits on a long poll instead of the stream ending, and the SAME turn
// continues after the decision — no new run and no new job. What did not change is that a
// proposal writes no business table until a user approves it, and that the decision arrives as
// an add-tool-result receipt (ADR-0002 §3.1).

// proposalArgs builds a valid propose_task_tree_patch argument JSON string that creates one task
// under the fixture goal.
func proposalArgs(goalID string) string {
	patch := []map[string]any{{
		"op": "create", "type": "task", "client_ref": "n1",
		"title": "补充实验", "success_criteria": "得到可复现结果", "minimum_action": "打开脚本跑一次",
	}}
	encoded, _ := json.Marshal(map[string]any{"goal_id": goalID, "instruction": "拆解下一步", "patch": patch})
	return string(encoded)
}

// proposalScript is the two-turn model script every approval test needs: propose, then answer.
func proposalScript(goalID, answer string) []scriptedTurn {
	return []scriptedTurn{
		{text: "我建议这样拆：", toolCalls: []agent.ToolCall{{
			ID: "call_propose", Name: "propose_task_tree_patch", Arguments: proposalArgs(goalID),
		}}},
		{text: answer},
	}
}

// proposeFixture drives a run to awaiting_approval and returns everything a test needs to decide
// it. The proposal tool call has already been executed by the host at that point, exactly as it
// would be in a browser.
func proposeFixture(t *testing.T, answer string) (fixture, *AgentService, *testHost, string) {
	t.Helper()
	f := newFixture(t)
	upstream := newFakeUpstream(t, proposalScript(f.goal.ID, answer)...)
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我拆解论文下一步")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	if outcomes := host.modelTurn(); len(outcomes) != 1 || outcomes[0].Status != "pending" {
		t.Fatalf("outcomes=%#v, want one parked proposal", outcomes)
	}
	if status := host.runStatus().Status; status != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval (§7)", status)
	}
	return f, svc, host, runID
}

func countTasks(t *testing.T, store *persistence.Store, userID, goalID string) int64 {
	t.Helper()
	var n int64
	if err := store.DB.Model(&persistence.Task{}).Where("user_id = ? AND goal_id = ?", userID, goalID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func countProposalsByStatus(t *testing.T, store *persistence.Store, userID, status string) int64 {
	t.Helper()
	var n int64
	q := store.DB.Model(&persistence.Proposal{}).Where("user_id = ?", userID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// countRunJobs is the assertion §1.2 and §7 both hinge on: a harness run has no job, and an
// approval must not create one.
func countRunJobs(t *testing.T, store *persistence.Store, runID string) int64 {
	t.Helper()
	var n int64
	if err := store.DB.Model(&persistence.AgentJob{}).Where("subject_id = ?", runID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func approveCommand(toolCallID, decision, reason string) Command {
	payload := map[string]any{"decision": decision}
	if reason != "" {
		payload["reason"] = reason
	}
	encoded, _ := json.Marshal(payload)
	return Command{Type: "add-tool-result", ToolCallID: toolCallID, Result: encoded}
}

// waitApproval reads the decision back off the long poll the host is holding open (§7).
func waitApproval(t *testing.T, svc *AgentService, host *testHost, proposalID string) ApprovalWait {
	t.Helper()
	wait, err := svc.WaitForApproval(context.Background(), host.principal, proposalID, 0)
	if err != nil {
		t.Fatalf("WaitForApproval: %v", err)
	}
	return wait
}

// pendingProposalID returns the proposal the parked run is waiting on.
func pendingProposalID(t *testing.T, store *persistence.Store, userID string) string {
	t.Helper()
	var proposal persistence.Proposal
	if err := store.DB.Where("user_id = ? AND status = 'pending'", userID).First(&proposal).Error; err != nil {
		t.Fatalf("no pending proposal: %v", err)
	}
	return proposal.ID
}

// TestProposalToolWritesNoBusinessTables covers agent.md §4 invariant 1 and agent-impl.md §10:
// proposing stages a Proposal and changes nothing in goals/tasks until the user approves.
func TestProposalToolWritesNoBusinessTables(t *testing.T) {
	f, svc, host, runID := proposeFixture(t, "已提交，等你确认。")

	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)) {
		t.Fatalf("tasks=%d after proposing, want %d (a proposal must not write business tables)", got, len(f.tasks))
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1", got)
	}
	var found bool
	for _, p := range assistantParts(t, svc, f.user.ID, runID) {
		if p.Type == "tool-call" && p.ToolName == "propose_task_tree_patch" {
			found = true
			if p.ApprovalStatus != "pending" || p.ProposalID == nil {
				t.Fatalf("proposal part approval=%q proposalID=%v", p.ApprovalStatus, p.ProposalID)
			}
			if p.ResultJSON == "" {
				t.Fatal("the proposal part carries no structured diff for the approval card")
			}
		}
	}
	if !found {
		t.Fatal("no proposal tool-call part persisted")
	}
	// The run is parked, not finished, and holds no job (§1.2).
	if countRunJobs(t, f.store, runID) != 0 {
		t.Fatal("a harness run created an AgentJob")
	}
	if host.beats == 0 {
		t.Fatal("the host never beat; a parked run would be reaped mid-wait (§10.4)")
	}
}

// TestApprovalApproveContinuesSameTurn covers §7.2, §7.3 and §5.1: approving applies the
// proposal synchronously, records the decision, hands it to the waiting host, and the SAME run and
// turn continue — no new run, no new job, no rebuilt context.
func TestApprovalApproveContinuesSameTurn(t *testing.T) {
	f, svc, host, runID := proposeFixture(t, "已应用，任务已创建。")
	ctx := context.Background()
	proposalID := pendingProposalID(t, f.store, f.user.ID)

	result, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("approval resolved run=%q, want the same run %q (§7.2)", result.RunID, runID)
	}
	if !result.NoStream {
		t.Fatal("an approval receipt opened a second stream; the open one carries the continuation (§7)")
	}
	// ApplyProposal ran synchronously: one task created (4 -> 5).
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)+1) {
		t.Fatalf("tasks=%d after approve, want %d", got, len(f.tasks)+1)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "applied"); got != 1 {
		t.Fatalf("applied proposals=%d, want 1", got)
	}
	if got := countRunJobs(t, f.store, runID); got != 0 {
		t.Fatalf("the approval created %d AgentJobs; the same turn continues in the host (§7)", got)
	}
	// The run is live again, and the host's wait returns the decision.
	if status := host.runStatus().Status; status != persistence.RunRunning {
		t.Fatalf("run status=%q after approval, want running", status)
	}
	wait := waitApproval(t, svc, host, proposalID)
	if wait.Status != "approved" {
		t.Fatalf("long poll returned %q, want approved", wait.Status)
	}
	encoded, _ := json.Marshal(wait.Result)
	if !strings.Contains(string(encoded), "applied") {
		t.Fatalf("the decision the model sees carries no apply result: %s", encoded)
	}

	// The same turn continues: one more model call, then the host reports completion.
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, runID)), "已应用") {
		t.Fatal("the continuation's text was not persisted")
	}
}

// TestApprovalRejectFeedsReasonToModel covers §7: rejecting records the reason as the tool result,
// writes no task, and lets the model re-propose in the same turn.
func TestApprovalRejectFeedsReasonToModel(t *testing.T) {
	f, svc, host, _ := proposeFixture(t, "明白，我换个拆法。")
	ctx := context.Background()
	proposalID := pendingProposalID(t, f.store, f.user.ID)
	host.decide = func(string, string) approvalDecision {
		return approvalDecision{Decision: "reject", Reason: "第二步和第三步重复了"}
	}

	if _, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "reject", "第二步和第三步重复了")); err != nil {
		t.Fatalf("ResolveApproval reject: %v", err)
	}
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)) {
		t.Fatalf("tasks=%d after reject, want unchanged %d", got, len(f.tasks))
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "rejected"); got != 1 {
		t.Fatalf("rejected proposals=%d, want 1", got)
	}
	wait := waitApproval(t, svc, host, proposalID)
	if wait.Status != "rejected" {
		t.Fatalf("long poll returned %q, want rejected", wait.Status)
	}
	encoded, _ := json.Marshal(wait.Result)
	if !strings.Contains(string(encoded), "第二步和第三步重复了") {
		t.Fatalf("the reject reason did not reach the model: %s", encoded)
	}

	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q after a rejection, want succeeded", got)
	}
}

// TestApprovalBaseRevisionConflict412 covers §7.4: if the tree moved after the proposal was
// staged, approving returns ErrRevision (→412), marks the proposal conflict, does NOT apply the
// patch — and, under a host, releases the run so the model can see the conflict instead of waiting
// forever.
func TestApprovalBaseRevisionConflict412(t *testing.T) {
	f, svc, host, _ := proposeFixture(t, "不会到达")
	ctx := context.Background()
	proposalID := pendingProposalID(t, f.store, f.user.ID)

	moved := persistence.Task{GoalID: f.goal.ID, Type: "task", Title: "插入的变更", SuccessCriteria: "sc", MinimumAction: "ma", Priority: 50, EstimateMinutes: 25}
	if err := f.app.CreateTask(ctx, f.user.ID, &moved); err != nil {
		t.Fatal(err)
	}
	tasksAfterMove := countTasks(t, f.store, f.user.ID, f.goal.ID)

	_, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if !errors.Is(err, ErrRevision) {
		t.Fatalf("approve after the tree moved err=%v, want ErrRevision (412)", err)
	}
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != tasksAfterMove {
		t.Fatalf("tasks=%d after 412, want %d (a stale patch must not apply)", got, tasksAfterMove)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "conflict"); got != 1 {
		t.Fatalf("conflict proposals=%d, want 1", got)
	}
	// The run is released rather than left parked: the decision happened, it just failed.
	if got := host.runStatus().Status; got != persistence.RunRunning {
		t.Fatalf("run status=%q after 412, want running so the turn can continue", got)
	}
	wait := waitApproval(t, svc, host, proposalID)
	if wait.Status != "conflict" || !wait.IsError {
		t.Fatalf("long poll returned %q (isError=%v), want a conflict error the model can act on", wait.Status, wait.IsError)
	}
}

// TestApprovalDuplicateReceipt409 covers §7.4: a second receipt for the same toolCallId is a
// duplicate and does not re-apply.
func TestApprovalDuplicateReceipt409(t *testing.T) {
	f, svc, _, runID := proposeFixture(t, "已应用。")
	ctx := context.Background()

	if _, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", "")); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	tasksAfterFirst := countTasks(t, f.store, f.user.ID, f.goal.ID)
	_, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if !errors.Is(err, ErrApprovalDuplicate) {
		t.Fatalf("second receipt err=%v, want ErrApprovalDuplicate (409)", err)
	}
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != tasksAfterFirst {
		t.Fatalf("tasks=%d after a duplicate receipt, want unchanged %d", got, tasksAfterFirst)
	}
	if got := countRunJobs(t, f.store, runID); got != 0 {
		t.Fatalf("a duplicate receipt created %d jobs", got)
	}
}

// TestApprovalCrossUserToolCall404 covers §7.4 and agent.md §4 invariant 5: a receipt is
// re-authenticated, and a toolCallId owned by another user is not-found (404).
func TestApprovalCrossUserToolCall404(t *testing.T) {
	f, svc, _, _ := proposeFixture(t, "不会到达")
	ctx := context.Background()

	other := persistence.User{ID: persistence.NewID("user"), Identifier: "approver-other", PasswordHash: "h", DisplayName: "O", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.ResolveApproval(ctx, other.ID, approveCommand("call_propose", "approve", ""))
	if !persistence.IsNotFound(err) {
		t.Fatalf("cross-user approval err=%v, want not-found (404)", err)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1 (a cross-user attempt must not resolve it)", got)
	}
}

// TestApprovalInvalidDecision asserts a receipt without a valid decision is rejected before
// touching the proposal.
func TestApprovalInvalidDecision(t *testing.T) {
	f, svc, host, _ := proposeFixture(t, "不会到达")
	ctx := context.Background()

	bad := Command{Type: "add-tool-result", ToolCallID: "call_propose", Result: json.RawMessage(`{"decision":"maybe"}`)}
	if _, err := svc.ResolveApproval(ctx, f.user.ID, bad); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("invalid decision err=%v, want ErrEmptyCommand", err)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1", got)
	}
	// Still parked: an invalid decision is not a decision.
	if status := host.runStatus().Status; status != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", status)
	}
}
