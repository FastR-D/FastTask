package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestAgentGanttNewGoalStagesAndAppliesDates(t *testing.T) {
	svc, principal := providerHarness(t, func(http.ResponseWriter, *http.Request) {}, LoopLimits{})
	ctx := context.Background()
	args := map[string]any{
		"title": "Reproduce a research result", "success_criteria": "Three source notes are archived", "target_date": "2001-01-01",
		"shortfall_reason": "Reading the method requires locating the source, so only two independent tasks can start now.",
		"nodes": []any{
			map[string]any{"type": "task", "title": "Find the source", "success_criteria": "Source is located", "minimum_action": "Open the index", "client_ref": "source", "estimate_minutes": 250},
			map[string]any{"type": "task", "title": "Read the method", "success_criteria": "Method is summarized", "minimum_action": "Read the first section", "depends_on_refs": []any{"source"}},
			map[string]any{"type": "task", "title": "Compare the outcome", "success_criteria": "A comparison is filed", "minimum_action": "Draft the comparison columns", "leave_unscheduled_reason": "用户要求稍后决定日期。"},
		},
	}
	tool := newGoalWithTasksTool(svc.app)
	result, err := tool.Execute(ctx, ToolContext{UserID: principal.UserID}, args)
	if err != nil || result.Proposal == nil || result.Proposal.Schedule == nil {
		t.Fatalf("stage new goal: %+v %v", result, err)
	}
	schedule := result.Proposal.Schedule
	if len(schedule.Changes) != 2 || schedule.Changes[0].End <= schedule.Changes[0].Start || schedule.Changes[1].Start < schedule.Changes[0].End ||
		!strings.Contains(schedule.Assumptions, "研究预算") || !strings.Contains(strings.Join(schedule.Warnings, " "), "超过目标截止日") {
		t.Fatalf("unexpected schedule: %+v", schedule)
	}
	var proposal persistence.Proposal
	if err := svc.store.DB.First(&proposal, "id = ?", result.Proposal.ProposalID).Error; err != nil {
		t.Fatal(err)
	}
	var unchanged int64
	if err := svc.store.DB.Model(&persistence.Goal{}).Where("title = ?", args["title"]).Count(&unchanged).Error; err != nil || unchanged != 0 {
		t.Fatalf("goal written before approval: %d %v", unchanged, err)
	}
	goal, count, err := svc.app.ApplyGoalWithTasks(ctx, principal.UserID, &proposal, principal.ThreadID)
	if err != nil || count != 3 || derefString(goal.TargetDate) != "2001-01-01" {
		t.Fatalf("apply new goal: %+v %d %v", goal, count, err)
	}
	var tasks []persistence.Task
	if err := svc.store.DB.Where("goal_id = ?", goal.ID).Order("position").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 || derefString(tasks[0].PlannedStart) != schedule.Changes[0].Start || derefString(tasks[1].PlannedEnd) != schedule.Changes[1].End || tasks[2].PlannedStart != nil {
		t.Fatalf("stored dates do not match the approved preview: %+v", tasks)
	}
}

func TestAgentGanttExistingScheduleChecksGoalAndPreservesDates(t *testing.T) {
	svc, principal := providerHarness(t, func(http.ResponseWriter, *http.Request) {}, LoopLimits{})
	ctx := context.Background()
	tc := ToolContext{UserID: principal.UserID, RunID: principal.RunID}
	goal := persistence.Goal{Title: "Study the method", SuccessCriteria: "Find and document source material"}
	if err := svc.app.CreateGoal(ctx, principal.UserID, &goal); err != nil {
		t.Fatal(err)
	}
	start, end := "2027-01-05", "2027-01-06"
	first := persistence.Task{GoalID: goal.ID, Type: "task", Title: "Already scheduled", SuccessCriteria: "Notes are filed", MinimumAction: "Open the notes", PlannedStart: &start, PlannedEnd: &end}
	second := persistence.Task{GoalID: goal.ID, Type: "task", Title: "Follow up", SuccessCriteria: "Open questions are recorded", MinimumAction: "Open the list"}
	for _, task := range []*persistence.Task{&first, &second} {
		if err := svc.app.CreateTask(ctx, principal.UserID, task); err != nil {
			t.Fatal(err)
		}
	}
	run, err := svc.repo.GetRun(ctx, principal.UserID, principal.RunID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.harnessLifecycle.loadRunSession(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	observe := func() {
		t.Helper()
		state, err := svc.app.toolGetTaskTree(ctx, tc, map[string]any{"goal_id": goal.ID})
		if err != nil {
			t.Fatal(err)
		}
		record, _ := json.Marshal(state.Result)
		if !strings.Contains(string(record), `"planned_start":"2027-01-05"`) {
			t.Fatalf("task-tree observation omitted planned dates: %s", record)
		}
		if err := svc.repo.CreatePart(ctx, &persistence.AgentMessagePart{UserID: principal.UserID, MessageID: session.assistantID, Type: "tool-call", ToolName: "get_task_tree", ResultJSON: string(record)}); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	stage := func(taskID string, reschedule bool) (ToolResult, error) {
		tool, _ := svc.tools.Get("propose_task_tree_patch")
		return tool.Execute(ctx, tc, map[string]any{
			"goal_id": goal.ID, "reschedule_existing": reschedule,
			"patch": []any{map[string]any{"op": "update", "target_id": taskID, "planned_start": "2027-01-08", "planned_end": "2027-01-09"}},
		})
	}
	if result, err := stage(first.ID, false); err != nil || !result.IsError {
		t.Fatalf("default scheduling overwrote an existing date: %+v %v", result, err)
	}
	if result, err := stage(first.ID, true); err != nil || result.Proposal == nil || result.Proposal.Schedule.Changes[0].BeforeStart != start {
		t.Fatalf("explicit reschedule omitted original dates: %+v %v", result, err)
	}
	staged, err := stage(second.ID, false)
	if err != nil || staged.Proposal == nil || !staged.Proposal.Schedule.DateOnly {
		t.Fatalf("stage date-only schedule: %+v %v", staged, err)
	}
	db := svc.store.DB
	var before, after int64
	if err := db.Model(&persistence.DailyPlan{}).Where("user_id = ?", principal.UserID).Select("COALESCE(SUM(current_revision),0)").Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	var proposal persistence.Proposal
	if err := db.First(&proposal, "id = ?", staged.Proposal.ProposalID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.app.UpdateGoal(ctx, principal.UserID, goal.ID, goal.Revision, map[string]any{"target_date": "2027-01-07"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.app.ApplyProposal(ctx, principal.UserID, &proposal, nil, proposal.BaseRevision); !errors.Is(err, ErrRevision) {
		t.Fatalf("goal deadline change did not invalidate schedule: %v", err)
	}
	observe()
	staged, err = stage(second.ID, false)
	if err != nil || staged.Proposal == nil || !strings.Contains(strings.Join(staged.Proposal.Schedule.Warnings, " "), "2 天") {
		t.Fatalf("new schedule should disclose the delay: %+v %v", staged, err)
	}
	proposal = persistence.Proposal{}
	if err := db.First(&proposal, "id = ?", staged.Proposal.ProposalID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.app.ApplyProposal(ctx, principal.UserID, &proposal, nil, proposal.BaseRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.DailyPlan{}).Where("user_id = ?", principal.UserID).Select("COALESCE(SUM(current_revision),0)").Scan(&after).Error; err != nil || before != after {
		t.Fatalf("date-only edit changed daily plans: %d -> %d %v", before, after, err)
	}
	var got persistence.Task
	if err := db.First(&got, "id = ?", second.ID).Error; err != nil || derefString(got.PlannedStart) != "2027-01-08" || derefString(got.PlannedEnd) != "2027-01-09" {
		t.Fatalf("approved task dates missing: %+v %v", got, err)
	}
}
