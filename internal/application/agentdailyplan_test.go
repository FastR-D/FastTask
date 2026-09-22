package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The daily-plan proposal tool under the harness (doc/agent-impl.md §11, doc/harness.md §7).
//
// What these tests guard did not change with the harness: the program — not the model — owns the
// top-3 cap and the ordering, a proposal writes no business table, and an invalid candidate is a
// tool error the model can correct rather than a run failure.

// dailyPlanArgsFor renders a propose_daily_plan argument JSON listing the given tasks (in the
// given order) as candidates, each with a concrete minimum action and a reason. The agent's order
// is deliberately controllable so tests can prove the program — not the model — owns the final
// ordering and the top-3 cap.
func dailyPlanArgsFor(tasks []persistence.Task, available int) string {
	candidates := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		candidates = append(candidates, map[string]any{
			"task_id":        task.ID,
			"minimum_action": "先做 " + task.Title + " 的第一步",
			"reason":         "因为 " + task.Title + " 最紧急",
		})
	}
	encoded, _ := json.Marshal(map[string]any{"candidates": candidates, "available_minutes": available})
	return string(encoded)
}

// planForUser loads the user's most recent daily plan and its live items (ordered by position).
// found is false when no plan exists yet.
func planForUser(t *testing.T, store *persistence.Store, userID string) (persistence.DailyPlan, []persistence.DailyPlanItem, bool) {
	t.Helper()
	var plan persistence.DailyPlan
	err := store.DB.Where("user_id = ?", userID).Order("created_at desc, id desc").First(&plan).Error
	if err != nil {
		if persistence.IsNotFound(err) {
			return plan, nil, false
		}
		t.Fatal(err)
	}
	var items []persistence.DailyPlanItem
	if err := store.DB.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position").Find(&items).Error; err != nil {
		t.Fatal(err)
	}
	return plan, items, true
}

// planFixture drives a run to a parked propose_daily_plan call. The arguments are built from the
// fixture because they name its tasks.
func planFixture(t *testing.T, argsFor func(fixture) string, answer string) (fixture, *AgentService, *testHost, string) {
	t.Helper()
	f := newFixture(t)
	upstream := newFakeUpstream(t,
		scriptedTurn{text: "今天的计划建议：", toolCalls: []agent.ToolCall{{ID: "call_plan", Name: "propose_daily_plan", Arguments: argsFor(f)}}},
		scriptedTurn{text: answer},
	)
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "帮我安排今天")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)
	if outcomes := host.modelTurn(); len(outcomes) != 1 || outcomes[0].Status != "pending" {
		t.Fatalf("outcomes=%#v, want one parked daily-plan proposal", outcomes)
	}
	return f, svc, host, runID
}

// TestDailyPlanProposalDeterministicTop3NotBypassed is the acceptance test for the deterministic
// top-3 (agent-impl.md §11: "确定性取三不被绕过"). The agent proposes all four fixture tasks in
// REVERSE priority order; after approval the program must still filter, re-sort by priority and cap
// at three core items (A, B, C), dropping D. The agent's candidate order and count cannot bypass
// domain.SelectDailyCandidates.
func TestDailyPlanProposalDeterministicTop3NotBypassed(t *testing.T) {
	f, svc, host, runID := planFixture(t, func(f fixture) string {
		// D, C, B, A: the agent proposes the lowest priority first.
		return dailyPlanArgsFor([]persistence.Task{f.tasks[3], f.tasks[2], f.tasks[1], f.tasks[0]}, 120)
	}, "已生成今日计划。")
	ctx := context.Background()

	if got := host.runStatus().Status; got != persistence.RunAwaitingApproval {
		t.Fatalf("run status=%q, want awaiting_approval", got)
	}
	// Invariant 1: proposing stages a pending proposal but writes NO plan yet.
	if _, _, found := planForUser(t, f.store, f.user.ID); found {
		t.Fatal("a daily plan existed before approval; propose must not write business tables")
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 1 {
		t.Fatalf("pending proposals=%d, want 1", got)
	}

	result, err := svc.ResolveApproval(ctx, f.user.ID, approveCommand("call_plan", "approve", ""))
	if err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("approval resolved run=%q, want the same run %q (§7.2)", result.RunID, runID)
	}

	plan, items, found := planForUser(t, f.store, f.user.ID)
	if !found {
		t.Fatal("no daily plan after approval")
	}
	if len(items) != 3 {
		t.Fatalf("core items=%d, want 3 (the deterministic top-3 must not be bypassed)", len(items))
	}
	// The program re-sorted by priority: A(100), B(90), C(80) at positions 1..3; D(70) is dropped
	// despite being proposed first by the agent.
	wantOrder := []string{f.tasks[0].ID, f.tasks[1].ID, f.tasks[2].ID}
	for i, want := range wantOrder {
		if items[i].TaskID == nil || *items[i].TaskID != want {
			t.Fatalf("item[%d] task=%v, want %q (priority order, not agent order)", i, items[i].TaskID, want)
		}
		if items[i].Position != i+1 {
			t.Fatalf("item[%d] position=%d, want %d", i, items[i].Position, i+1)
		}
	}
	for _, item := range items {
		if item.TaskID != nil && *item.TaskID == f.tasks[3].ID {
			t.Fatal("lowest-priority task D landed; the top-3 cap was bypassed")
		}
	}
	if items[0].MinimumAction != "先做 A 的第一步" {
		t.Fatalf("item minimum_action=%q, want the agent-proposed action", items[0].MinimumAction)
	}

	// arch.md §9.2: the revision input snapshot folds in the agent's candidates, reasons and
	// schema version, and is covered by the hash.
	var rev persistence.DailyPlanRevision
	if err := f.store.DB.Where("plan_id = ?", plan.ID).Order("revision desc").First(&rev).Error; err != nil {
		t.Fatalf("load revision: %v", err)
	}
	if !strings.Contains(rev.InputSnapshot, dailyPlanAgentSchema) {
		t.Fatalf("input snapshot missing schema version: %s", rev.InputSnapshot)
	}
	if !strings.Contains(rev.InputSnapshot, "最紧急") {
		t.Fatalf("input snapshot missing agent reasons: %s", rev.InputSnapshot)
	}
	if rev.InputHash == "" || rev.InputHash != persistence.Hash(rev.InputSnapshot) {
		t.Fatal("input hash does not cover the snapshot")
	}

	// The same turn continues to its answer (§7).
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
}

// TestDailyPlanProposalNoPaddingWhenFewerThanThree covers agent.md §6 constraint 2 ("候选不足时不补
// 占位任务"): proposing two candidates yields exactly two core items, never padded up to three.
func TestDailyPlanProposalNoPaddingWhenFewerThanThree(t *testing.T) {
	f, svc, _, _ := planFixture(t, func(f fixture) string {
		return dailyPlanArgsFor([]persistence.Task{f.tasks[1], f.tasks[2]}, 120) // B, C
	}, "已生成。")

	if _, err := svc.ResolveApproval(context.Background(), f.user.ID, approveCommand("call_plan", "approve", "")); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	_, items, found := planForUser(t, f.store, f.user.ID)
	if !found {
		t.Fatal("no daily plan after approval")
	}
	if len(items) != 2 {
		t.Fatalf("core items=%d, want 2 (no placeholder padding)", len(items))
	}
}

// TestDailyPlanCandidateMissingMinimumActionRejected covers agent.md §6 constraint 3 ("最小行动…
// 缺失则该候选无效"): a candidate without a concrete minimum action is rejected by the tool, staging
// no proposal and writing no plan.
func TestDailyPlanCandidateMissingMinimumActionRejected(t *testing.T) {
	f := newFixture(t)
	badArgs := `{"candidates":[{"task_id":"` + f.tasks[0].ID + `","reason":"缺少最小行动"}],"available_minutes":60}`
	upstream := newFakeUpstream(t,
		scriptedTurn{text: "建议：", toolCalls: []agent.ToolCall{{ID: "call_plan", Name: "propose_daily_plan", Arguments: badArgs}}},
		scriptedTurn{text: "好的，我补上最小行动。"},
	)
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "安排今天")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)

	outcomes := host.executeCalls([]agent.ToolCall{{ID: "call_plan", Name: "propose_daily_plan", Arguments: badArgs}})
	if len(outcomes) != 1 || outcomes[0].Status != "error" {
		t.Fatalf("outcomes=%#v, want a structured tool error", outcomes)
	}
	if !strings.Contains(outcomes[0].Error, "minimum_action") {
		t.Fatalf("the error does not name the missing field: %q", outcomes[0].Error)
	}
	// The invalid candidate is a tool error, not an approval pause: no proposal, no plan, and the
	// run keeps going so the model can correct itself.
	if got := host.runStatus().Status; got == persistence.RunAwaitingApproval {
		t.Fatal("the run paused for approval on an invalid candidate; want a tool error fed back")
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "pending"); got != 0 {
		t.Fatalf("pending proposals=%d, want 0 (an invalid candidate must not stage a proposal)", got)
	}
	if _, _, found := planForUser(t, f.store, f.user.ID); found {
		t.Fatal("a daily plan was written for an invalid candidate")
	}
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded after the model corrected itself", got)
	}
}

// TestDailyPlanProposalRejectWritesNoPlan covers §7: rejecting a daily-plan proposal records the
// reason, writes no plan, and the same turn continues.
func TestDailyPlanProposalRejectWritesNoPlan(t *testing.T) {
	f, svc, host, runID := planFixture(t, func(f fixture) string {
		return dailyPlanArgsFor([]persistence.Task{f.tasks[0], f.tasks[1], f.tasks[2]}, 120)
	}, "明白，我重新安排。")
	proposalID := pendingProposalID(t, f.store, f.user.ID)

	if _, err := svc.ResolveApproval(context.Background(), f.user.ID, approveCommand("call_plan", "reject", "今天没空")); err != nil {
		t.Fatalf("ResolveApproval reject: %v", err)
	}
	if _, _, found := planForUser(t, f.store, f.user.ID); found {
		t.Fatal("a daily plan was written despite rejection")
	}
	if got := countProposalsByStatus(t, f.store, f.user.ID, "rejected"); got != 1 {
		t.Fatalf("rejected proposals=%d, want 1", got)
	}
	// The reject reason is what the waiting host hands back to the model (§7).
	wait := waitApproval(t, svc, host, proposalID)
	if wait.Status != "rejected" {
		t.Fatalf("long poll returned %q, want rejected", wait.Status)
	}
	encoded, _ := json.Marshal(wait.Result)
	if !strings.Contains(string(encoded), "今天没空") {
		t.Fatalf("the reject reason did not reach the model: %s", encoded)
	}
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run %s status=%q, want succeeded", runID, got)
	}
}
