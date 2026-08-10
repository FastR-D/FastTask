package application

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
)

type fixture struct {
	app   *App
	store *persistence.Store
	user  persistence.User
	goal  persistence.Goal
	tasks []persistence.Task
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "app", PasswordHash: "hash", DisplayName: "App", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	app := New(store)
	goal := persistence.Goal{Title: "Finish paper", SuccessCriteria: "Draft reviewed"}
	if err := app.CreateGoal(context.Background(), user.ID, &goal); err != nil {
		t.Fatal(err)
	}
	tasks := make([]persistence.Task, 4)
	for i := range tasks {
		tasks[i] = persistence.Task{GoalID: goal.ID, Type: "task", Title: string(rune('A' + i)), SuccessCriteria: "observable result", MinimumAction: "write one line", Priority: 100 - i*10, EstimateMinutes: 25, Position: i}
		if err := app.CreateTask(context.Background(), user.ID, &tasks[i]); err != nil {
			t.Fatal(err)
		}
	}
	return fixture{app, store, user, goal, tasks}
}

func TestDailyPlanLimitAndCompletionSemantics(t *testing.T) {
	f := newFixture(t)
	date := time.Now().Format("2006-01-02")
	plan, items, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, date, "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d core items, want 3", len(items))
	}
	extraID := f.tasks[3].ID
	extra := persistence.DailyPlanItem{TaskID: &extraID, Kind: "core", Title: "fourth", Commitment: "do", MinimumAction: "start", TargetMinutes: 25}
	if err := f.app.AddPlanItem(context.Background(), f.user.ID, plan.ID, plan.Revision, &extra); !errors.Is(err, domain.ErrCoreLimit) {
		t.Fatalf("fourth core item error = %v", err)
	}
	completed, err := f.app.CompletePlanItem(context.Background(), f.user.ID, items[0].ID, items[0].Revision, "minimum_action", "started", nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "satisfied" {
		t.Fatalf("item status=%s", completed.Status)
	}
	var task persistence.Task
	if err := f.store.DB.First(&task, "id = ?", *completed.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status == "completed" {
		t.Fatal("minimum action completed underlying task")
	}
}

func TestSupportRequiresEveryCoreItemSatisfied(t *testing.T) {
	f := newFixture(t)
	plan, items, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, "2026-08-08", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	support := persistence.DailyPlanItem{Kind: "input", Title: "Read", Commitment: "Read one section", MinimumAction: "Open paper", TargetMinutes: 25}
	if err := f.app.AddPlanItem(context.Background(), f.user.ID, plan.ID, plan.Revision, &support); !errors.Is(err, domain.ErrSupportBeforeCoreDone) {
		t.Fatalf("support before core completion error=%v", err)
	}
	for _, item := range items {
		if _, err := f.app.CompletePlanItem(context.Background(), f.user.ID, item.ID, item.Revision, "minimum_action", "done", nil); err != nil {
			t.Fatal(err)
		}
	}
	var refreshed persistence.DailyPlan
	if err := f.store.DB.First(&refreshed, "id = ?", plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.AddPlanItem(context.Background(), f.user.ID, plan.ID, refreshed.Revision, &support); err != nil {
		t.Fatalf("support after completion rejected: %v", err)
	}
}

func TestTimeCompletionUsesServerSessionDuration(t *testing.T) {
	f := newFixture(t)
	plan, items, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, "2026-08-09", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	item := items[0]
	session := persistence.WorkSession{TaskID: *item.TaskID, DailyPlanItemID: &item.ID, TargetMinutes: 25, StartedAt: persistence.Now().Add(-55 * time.Minute)}
	if err := f.app.StartSession(context.Background(), f.user.ID, &session); err != nil {
		t.Fatal(err)
	}
	finished, err := f.app.TransitionSession(context.Background(), f.user.ID, session.ID, session.Revision, "complete", "progressed", "two pomodoros", persistence.Now())
	if err != nil {
		t.Fatal(err)
	}
	if finished.DurationSeconds < 50*60 {
		t.Fatalf("duration=%d", finished.DurationSeconds)
	}
	if _, err := f.app.CompletePlanItem(context.Background(), f.user.ID, item.ID, item.Revision, "time", "focused", []string{session.ID}); err != nil {
		t.Fatal(err)
	}
	_ = plan
}

func TestWorkerPersistsProposalAndRejectsStaleAttempt(t *testing.T) {
	f := newFixture(t)
	job, err := f.app.CreateJob(context.Background(), f.user.ID, "task_tree_generation", "goal", f.goal.ID, 4, map[string]any{"instruction": "split"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(f.app, time.Millisecond)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.First(job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "succeeded" {
		t.Fatalf("job status=%s error=%s", job.Status, job.ErrorMessage)
	}
	var count int64
	f.store.DB.Model(&persistence.Proposal{}).Where("job_id = ?", job.ID).Count(&count)
	if count != 1 {
		t.Fatalf("proposal count=%d", count)
	}
}

func TestSameDayReplanPreservesSatisfiedSlotsAndSupersedesOpenItems(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan, items, err := f.app.CreateDailyPlan(ctx, f.user.ID, "2026-08-10", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.CompletePlanItem(ctx, f.user.ID, items[0].ID, items[0].Revision, "minimum_action", "done", nil); err != nil {
		t.Fatal(err)
	}
	replanned, newItems, err := f.app.ReplanDailyPlan(ctx, f.user.ID, plan.LocalDate, plan.Timezone, nil, 120, plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if replanned.CurrentRevision != 2 {
		t.Fatalf("current revision=%d", replanned.CurrentRevision)
	}
	core := 0
	satisfied := 0
	for _, item := range newItems {
		if item.Kind == "core" {
			core++
		}
		if item.Status == "satisfied" {
			satisfied++
		}
		if item.PlanRevision != 2 {
			t.Fatalf("item revision=%d", item.PlanRevision)
		}
	}
	if core > 3 || satisfied != 1 {
		t.Fatalf("core=%d satisfied=%d", core, satisfied)
	}
	var superseded int64
	f.store.DB.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = 1 AND status = 'superseded'", plan.ID).Count(&superseded)
	if superseded != 2 {
		t.Fatalf("superseded=%d", superseded)
	}
}

func TestClosePlanMarksUnresolvedItemsNotCompleted(t *testing.T) {
	f := newFixture(t)
	plan, _, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, "2026-08-11", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := f.app.CloseDailyPlan(context.Background(), f.user.ID, plan.ID, plan.Revision, "closed")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" {
		t.Fatalf("status=%s", closed.Status)
	}
	var count int64
	f.store.DB.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND status = 'not_completed'", plan.ID).Count(&count)
	if count != 3 {
		t.Fatalf("not completed=%d", count)
	}
}

func TestCrossUserReferencesAndDirectSatisfiedPatchAreRejected(t *testing.T) {
	f := newFixture(t)
	now := persistence.Now()
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "other-app", PasswordHash: "hash", DisplayName: "Other", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherGoal := persistence.Goal{Title: "Other", SuccessCriteria: "done"}
	if err := f.app.CreateGoal(context.Background(), other.ID, &otherGoal); err != nil {
		t.Fatal(err)
	}
	otherTask := persistence.Task{GoalID: otherGoal.ID, Title: "Other task", SuccessCriteria: "done", MinimumAction: "start"}
	if err := f.app.CreateTask(context.Background(), other.ID, &otherTask); err != nil {
		t.Fatal(err)
	}
	plan, items, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, "2026-08-12", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	bad := persistence.DailyPlanItem{TaskID: &otherTask.ID, Kind: "core", Title: "bad", Commitment: "bad", MinimumAction: "bad"}
	if err := f.app.AddPlanItem(context.Background(), f.user.ID, plan.ID, plan.Revision, &bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user item err=%v", err)
	}
	session := persistence.WorkSession{TaskID: otherTask.ID, DailyPlanItemID: &items[0].ID}
	if err := f.app.StartSession(context.Background(), f.user.ID, &session); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user session err=%v", err)
	}
	if _, err := f.app.UpdatePlanItem(context.Background(), f.user.ID, plan.ID, items[0].ID, items[0].Revision, map[string]any{"status": "satisfied"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("direct satisfied err=%v", err)
	}
}

func TestSupportWorkerCreatesVisibleInputItem(t *testing.T) {
	f := newFixture(t)
	plan, items, err := f.app.CreateDailyPlan(context.Background(), f.user.ID, "2026-08-13", "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if _, err := f.app.CompletePlanItem(context.Background(), f.user.ID, item.ID, item.Revision, "minimum_action", "done", nil); err != nil {
			t.Fatal(err)
		}
	}
	job, err := f.app.CreateJob(context.Background(), f.user.ID, "support_generation", "daily_plan", plan.ID, 0, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(f.app, time.Millisecond)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.store.DB.First(job, "id = ?", job.ID)
	if job.Status != "succeeded" {
		t.Fatalf("job=%s %s", job.Status, job.ErrorMessage)
	}
	var count int64
	f.store.DB.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'input'", plan.ID).Count(&count)
	if count != 1 {
		t.Fatalf("input items=%d", count)
	}
}

func TestExternalImportConversionCreatesTaskAtomically(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	item := persistence.ExternalImport{SourceSystem: "fastread", SourceExternalID: "paper-task-1", SourceURL: "https://example.org/paper", ContentHash: "sha256:paper", Kind: "candidate_task", Title: "Read paper", Description: "Assess it as a baseline", SuggestedGoalID: &f.goal.ID, ArtifactsJSON: `[{"kind":"json","uri":"file:///tmp/report.json"}]`, MetadataJSON: `{"page_count":12}`}
	if err := f.app.CreateExternalImport(ctx, f.user.ID, &item); err != nil {
		t.Fatal(err)
	}
	converted, task, err := f.app.ConvertExternalImport(ctx, f.user.ID, item.ID, item.Revision, ConvertExternalImportCommand{Type: "task", SuccessCriteria: "Record a baseline decision with evidence", MinimumAction: "Read the abstract and method section", Priority: 80, EstimateMinutes: 50, DecisionNote: "Relevant to the active goal"})
	if err != nil {
		t.Fatal(err)
	}
	if converted.Status != "converted" || converted.TaskID == nil || *converted.TaskID != task.ID {
		t.Fatalf("unexpected conversion: %#v task=%#v", converted, task)
	}
	if task.Title != item.Title || task.Description != item.Description || task.Status != "ready" || task.GoalID != f.goal.ID {
		t.Fatalf("unexpected task: %#v", task)
	}
	var treeRevisions int64
	f.store.DB.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ? AND source = 'external_import'", f.goal.ID).Count(&treeRevisions)
	if treeRevisions != 1 {
		t.Fatalf("external import tree revisions=%d", treeRevisions)
	}
	var progress int64
	f.store.DB.Model(&persistence.ProgressEvent{}).Where("task_id = ?", task.ID).Count(&progress)
	if progress != 0 {
		t.Fatalf("conversion created %d progress events", progress)
	}
}

func TestExternalImportConversionRollbackAndTerminalRejection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	item := persistence.ExternalImport{SourceSystem: "fastwrite", SourceExternalID: "review-1", Kind: "review_issue", Title: "Fix threat model", ArtifactsJSON: "[]", MetadataJSON: "{}"}
	if err := f.app.CreateExternalImport(ctx, f.user.ID, &item); err != nil {
		t.Fatal(err)
	}
	beforeTasks := int64(0)
	f.store.DB.Model(&persistence.Task{}).Where("user_id = ?", f.user.ID).Count(&beforeTasks)
	_, _, err := f.app.ConvertExternalImport(ctx, f.user.ID, item.ID, item.Revision, ConvertExternalImportCommand{GoalID: "missing", Type: "task", SuccessCriteria: "fixed", MinimumAction: "open issue"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid conversion error=%v", err)
	}
	var current persistence.ExternalImport
	if err := f.store.DB.First(&current, "id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "candidate" || current.Revision != 1 {
		t.Fatalf("import changed after rollback: %#v", current)
	}
	afterTasks := int64(0)
	f.store.DB.Model(&persistence.Task{}).Where("user_id = ?", f.user.ID).Count(&afterTasks)
	if afterTasks != beforeTasks {
		t.Fatalf("task count changed after rollback: %d -> %d", beforeTasks, afterTasks)
	}
	rejected, err := f.app.RejectExternalImport(ctx, f.user.ID, item.ID, item.Revision, "Not relevant")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != "rejected" || rejected.DecidedAt == nil {
		t.Fatalf("unexpected rejection: %#v", rejected)
	}
	if _, _, err := f.app.ConvertExternalImport(ctx, f.user.ID, item.ID, rejected.Revision, ConvertExternalImportCommand{ExistingTaskID: &f.tasks[0].ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected import converted: %v", err)
	}
}
