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

// proposalArgs builds a valid propose_task_tree_patch argument JSON string that
// creates one task under the fixture goal.
func proposalArgs(goalID string) string {
	patch := []map[string]any{{
		"op": "create", "type": "task", "client_ref": "n1",
		"title": "补充实验", "success_criteria": "得到可复现结果", "minimum_action": "打开脚本跑一次",
	}}
	encoded, _ := json.Marshal(map[string]any{"goal_id": goalID, "instruction": "拆解下一步", "patch": patch})
	return string(encoded)
}

// proposeFixture runs a run to awaiting_approval and returns the fixture, service,
// run id and the proposal tool-call id.
func proposeFixture(t *testing.T, fake *fakeChat) (fixture, *AgentService, string, string) {
	t.Helper()
	f, svc := loopFixture(t, fake)
	job := submitJob(t, svc, f.store, f.user.ID, "帮我拆解论文下一步")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", run.Status)
	}
	return f, svc, run.ID, "call_propose"
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

func resumeJob(t *testing.T, svc *AgentService, store *persistence.Store, userID, runID string) persistence.AgentJob {
	t.Helper()
	run := runStatus(t, svc, userID, runID)
	var job persistence.AgentJob
	if err := store.DB.Where("id = ?", run.JobID).First(&job).Error; err != nil {
		t.Fatalf("load resume job: %v", err)
	}
	return job
}

func approveCommand(toolCallID, decision, reason string) Command {
	payload := map[string]any{"decision": decision}
	if reason != "" {
		payload["reason"] = reason
	}
	encoded, _ := json.Marshal(payload)
	return Command{Type: "add-tool-result", ToolCallID: toolCallID, Result: encoded}
}

// TestProposalToolWritesNoBusinessTables covers agent-impl.md §10 ("proposal 级
// 工具执行后业务表零写入") and invariant 1: proposing stages a Proposal and
// changes nothing in goals/tasks until the user approves.
func TestProposalToolWritesNoBusinessTables(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "我建议这样拆：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: ""}}},
		{text: "已提交，等你确认。"},
	}}
	// The Arguments are goal-dependent; fill them after the fixture goal is known.
	f := newFixture(t)
	fake.turns[0].toolCalls[0].Arguments = proposalArgs(f.goal.ID)
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))

	job := submitJob(t, svc, f.store, f.user.ID, "帮我拆解论文下一步")
	if _, err := svc.ExecuteRun(context.Background(), job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", run.Status)
	}
	// Zero business writes: still exactly the four fixture tasks.
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)) {
		t.Fatalf("tasks=%d after proposing, want %d (proposal must not write business tables)", got, len(f.tasks))
	}
	// Exactly one pending proposal was staged.
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1", got)
	}
	// The tool-call part is persisted with approval pending and the proposal link.
	parts := assistantParts(t, svc, f.user.ID, run.ID)
	var found bool
	for _, p := range parts {
		if p.Type == "tool-call" && p.ToolName == "propose_task_tree_patch" {
			found = true
			if p.ApprovalStatus != "pending" || p.ProposalID == nil {
				t.Fatalf("proposal part approval=%q proposalID=%v", p.ApprovalStatus, p.ProposalID)
			}
		}
	}
	if !found {
		t.Fatal("no proposal tool-call part persisted")
	}
}

// TestApprovalApproveAppliesAndResumes covers §7.2/§7.3: approving applies the
// proposal synchronously (tasks created), records the decision, re-queues the
// SAME run, and the resumed loop feeds the approval result back to the model.
func TestApprovalApproveAppliesAndResumes(t *testing.T) {
	f := newFixture(t)
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: proposalArgs(f.goal.ID)}}},
		{text: "已应用，任务已创建。"},
	}}
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()

	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	runID := job.SubjectID

	result, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("approval resumed run=%q, want same run %q (§7.2)", result.RunID, runID)
	}
	// ApplyProposal ran synchronously: one task created (4 -> 5).
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)+1) {
		t.Fatalf("tasks=%d after approve, want %d", got, len(f.tasks)+1)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "applied"); got != 1 {
		t.Fatalf("applied proposals=%d, want 1", got)
	}
	// FromSeq is the awaiting checkpoint so the client streams only the continuation.
	run := runStatus(t, svc, f.user.ID, runID)
	if result.FromSeq != run.CheckpointSeq {
		// CheckpointSeq advances on resume; FromSeq captured the pre-resume value.
		if result.FromSeq <= 0 {
			t.Fatalf("FromSeq=%d, want the awaiting checkpoint > 0", result.FromSeq)
		}
	}

	// Resume the same run under the new job; the loop continues and succeeds.
	rjob := resumeJob(t, svc, f.store, f.user.ID, runID)
	if rjob.ID == job.ID {
		t.Fatal("resume did not create a new AgentJob (§7.2)")
	}
	if _, err := svc.ExecuteRun(ctx, rjob); err != nil {
		t.Fatalf("resume ExecuteRun: %v", err)
	}
	if got := runStatus(t, svc, f.user.ID, runID).Status; got != persistence.RunSucceeded {
		t.Fatalf("resumed run status=%q, want succeeded", got)
	}
	// The resumed model turn saw the approval result fed back (§7).
	if fake.calls < 2 {
		t.Fatalf("model calls=%d, want the resumed continuation", fake.calls)
	}
	resumed := fake.requests[1]
	var toolContent string
	for _, m := range resumed.Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "approve") {
		t.Fatalf("approval result not fed back to the model: %q", toolContent)
	}
	if !strings.Contains(concatText(assistantParts(t, svc, f.user.ID, runID)), "已应用") {
		t.Fatal("resumed assistant text not persisted")
	}
}

// TestApprovalRejectFeedsReasonToModel covers §7: rejecting records the reason as
// the tool result and resumes the run so the model can re-propose. No tasks are
// created on reject.
func TestApprovalRejectFeedsReasonToModel(t *testing.T) {
	f := newFixture(t)
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: proposalArgs(f.goal.ID)}}},
		{text: "明白，我换个拆法。"},
	}}
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()

	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	runID := job.SubjectID

	if _, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "reject", "第二步和第三步重复了")); err != nil {
		t.Fatalf("ResolveApproval reject: %v", err)
	}
	// Reject writes no tasks.
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != int64(len(f.tasks)) {
		t.Fatalf("tasks=%d after reject, want unchanged %d", got, len(f.tasks))
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "rejected"); got != 1 {
		t.Fatalf("rejected proposals=%d, want 1", got)
	}
	rjob := resumeJob(t, svc, f.store, f.user.ID, runID)
	if _, err := svc.ExecuteRun(ctx, rjob); err != nil {
		t.Fatalf("resume ExecuteRun: %v", err)
	}
	// The reject reason reached the model (§7: 拒绝理由要回灌模型).
	resumed := fake.requests[1]
	var toolContent string
	for _, m := range resumed.Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "第二步和第三步重复了") {
		t.Fatalf("reject reason not fed back to the model: %q", toolContent)
	}
}

// TestApprovalBaseRevisionConflict412 covers §7.4/§10: if the tree moved after the
// proposal was staged, approve returns ErrRevision (→412), marks the proposal
// conflict, and does NOT apply the patch.
func TestApprovalBaseRevisionConflict412(t *testing.T) {
	f := newFixture(t)
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: proposalArgs(f.goal.ID)}}},
		{text: "不会到达"},
	}}
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()

	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	runID := job.SubjectID

	// Advance the tree revision behind the proposal's back.
	moved := persistence.Task{GoalID: f.goal.ID, Type: "task", Title: "插入的变更", SuccessCriteria: "sc", MinimumAction: "ma", Priority: 50, EstimateMinutes: 25}
	if err := f.app.CreateTask(ctx, f.user.ID, &moved); err != nil {
		t.Fatal(err)
	}
	tasksAfterMove := countTasks(t, f.store, f.user.ID, f.goal.ID)

	_, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if !errors.Is(err, ErrRevision) {
		t.Fatalf("approve after tree moved err=%v, want ErrRevision (412)", err)
	}
	// The stale patch was NOT applied: task count is just the moved one, no proposal task.
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != tasksAfterMove {
		t.Fatalf("tasks=%d after 412, want %d (stale patch must not apply)", got, tasksAfterMove)
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "conflict"); got != 1 {
		t.Fatalf("conflict proposals=%d, want 1", got)
	}
	// The run was NOT resumed on apply failure (§7.3).
	if got := runStatus(t, svc, f.user.ID, runID).Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q after 412, want still awaiting_approval (not resumed)", got)
	}
}

// TestApprovalDuplicateReceipt409 covers §7.4: a second receipt for the same
// toolCallId is rejected as a duplicate and does not re-apply.
func TestApprovalDuplicateReceipt409(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: ""}}},
		{text: "已应用。"},
	}}
	f := newFixture(t)
	fake.turns[0].toolCalls[0].Arguments = proposalArgs(f.goal.ID)
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()

	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if _, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", "")); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	tasksAfterFirst := countTasks(t, f.store, f.user.ID, f.goal.ID)
	// Second receipt for the same toolCallId → duplicate (409), no re-apply.
	_, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_propose", "approve", ""))
	if !errors.Is(err, ErrApprovalDuplicate) {
		t.Fatalf("second receipt err=%v, want ErrApprovalDuplicate (409)", err)
	}
	if got := countTasks(t, f.store, f.user.ID, f.goal.ID); got != tasksAfterFirst {
		t.Fatalf("tasks=%d after duplicate, want unchanged %d", got, tasksAfterFirst)
	}
}

// TestApprovalCrossUserToolCall404 covers §7.4/§10: an approval receipt is
// re-authenticated, and a toolCallId owned by another user is not-found (404).
func TestApprovalCrossUserToolCall404(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: ""}}},
	}}
	f := newFixture(t)
	fake.turns[0].toolCalls[0].Arguments = proposalArgs(f.goal.ID)
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()

	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	// A different user cannot resolve the owner's approval.
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "approver-other", PasswordHash: "h", DisplayName: "O", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.ResolveApproval(ctx, other.ID, approveCommand("call_propose", "approve", ""))
	if !persistence.IsNotFound(err) {
		t.Fatalf("cross-user approval err=%v, want not-found (404)", err)
	}
	// The owner's proposal is untouched.
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1 (cross-user attempt must not resolve it)", got)
	}
}

// TestApprovalInvalidDecision asserts a receipt without a valid decision is
// rejected before touching the proposal.
func TestApprovalInvalidDecision(t *testing.T) {
	fake := &fakeChat{turns: []scriptedTurn{
		{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_propose", Name: "propose_task_tree_patch", Arguments: ""}}},
	}}
	f := newFixture(t)
	fake.turns[0].toolCalls[0].Arguments = proposalArgs(f.goal.ID)
	svc := NewAgentService(f.app, WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return fake, nil }))
	ctx := context.Background()
	job := submitJob(t, svc, f.store, f.user.ID, "拆解论文")
	if _, err := svc.ExecuteRun(ctx, job); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	bad := Command{Type: "add-tool-result", ToolCallID: "call_propose", Result: json.RawMessage(`{"decision":"maybe"}`)}
	if _, err := svc.ResolveApproval(ctx, f.user.ID, bad); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("invalid decision err=%v, want ErrEmptyCommand", err)
	}
	// Still pending; nothing applied.
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1", got)
	}
}
