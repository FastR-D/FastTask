package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

func reviewTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return at
}
func reviewPointer(value string) *string { return &value }

func reviewFixture(t *testing.T) (*LensService, string, string, persistence.Goal, []persistence.Task) {
	t.Helper()
	store, owner, other := panelTestStore(t)
	at := reviewTime("2026-11-10T08:00:00Z")
	goal := persistence.Goal{ID: "review-goal", UserID: owner, Title: "论文推进", Status: "active", SuccessCriteria: "可核验成果", Revision: 1, CreatedAt: at, UpdatedAt: at}
	if err := store.DB.Create(&goal).Error; err != nil {
		t.Fatal(err)
	}
	tasks := []persistence.Task{}
	for _, id := range []string{"joint", "retracted", "reopened", "focus", "next"} {
		task := persistence.Task{ID: id, UserID: owner, GoalID: goal.ID, Type: "task", Title: "任务 " + id, Status: "ready", DependsOn: []string{}, SuccessCriteria: "实际产出", MinimumAction: "小步", Revision: 1, CreatedAt: at, UpdatedAt: at}
		if err := store.DB.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	plan := persistence.DailyPlan{ID: "base-plan", UserID: owner, LocalDate: "2026-11-11", Timezone: "Asia/Shanghai", Status: "active", CurrentRevision: 1, Revision: 1, CreatedAt: at, UpdatedAt: at}
	if err := store.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"item", "item2"} {
		item := persistence.DailyPlanItem{ID: id, LogicalID: id, UserID: owner, PlanID: plan.ID, TaskID: &tasks[i].ID, Kind: "core", Title: "原安排", Status: "planned", PlanRevision: 1, TargetMinutes: 40, Revision: 1, CreatedAt: at, UpdatedAt: at}
		if err := store.DB.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	return NewLensService(store), owner, other, goal, tasks
}

func reviewInsert(t *testing.T, a *LensService, value any) {
	t.Helper()
	if err := a.Store.DB.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}

func TestReviewActivityJointCompletionReversalAndOwnerIsolation(t *testing.T) {
	a, owner, other, _, tasks := reviewFixture(t)
	at := reviewTime("2026-11-11T08:00:00Z")
	events := []persistence.ProgressEvent{
		{ID: "daily", UserID: owner, TaskID: &tasks[0].ID, PlanItemID: reviewPointer("item"), Type: "result", Summary: "同一次确认", EvidenceJSON: `{"logical_id":"logical-1"}`, OccurredAt: at, CreatedAt: at},
		{ID: "whole", UserID: owner, TaskID: &tasks[0].ID, Type: "result", Summary: "同一次确认", EvidenceJSON: `{"daily_completion_event_id":"daily"}`, OccurredAt: at, CreatedAt: at},
		{ID: "reversed-step", UserID: owner, TaskID: &tasks[1].ID, PlanItemID: reviewPointer("item2"), Type: "step", Summary: "误记步骤", EvidenceJSON: `{}`, OccurredAt: at, CreatedAt: at},
		{ID: "old-completion", UserID: owner, TaskID: &tasks[2].ID, Type: "result", Summary: "旧结果", EvidenceJSON: `[]`, OccurredAt: at, CreatedAt: at},
		{ID: "other-secret", UserID: other, Type: "result", Summary: "另一用户的秘密", EvidenceJSON: `{}`, OccurredAt: at, CreatedAt: at},
	}
	for i := range events {
		reviewInsert(t, a, &events[i])
	}
	// Corrections made after the selected week still invalidate the old fact.
	later := reviewTime("2026-11-18T08:00:00Z")
	for _, e := range []persistence.ProgressEvent{
		{ID: "undo-daily", UserID: owner, TaskID: &tasks[0].ID, Type: "daily_item_reopened", EvidenceJSON: `{"reverses_event_id":"daily"}`, OccurredAt: later, CreatedAt: later},
		{ID: "undo-step", UserID: owner, TaskID: &tasks[1].ID, Type: "daily_item_reopened", EvidenceJSON: `{"reverses_event_id":"reversed-step"}`, OccurredAt: later, CreatedAt: later},
		{ID: "reopen-task", UserID: owner, TaskID: &tasks[2].ID, Type: "task_reopened", EvidenceJSON: `[]`, OccurredAt: later, CreatedAt: later},
	} {
		reviewInsert(t, a, &e)
	}
	session := persistence.WorkSession{ID: "focus-session", UserID: owner, TaskID: tasks[3].ID, SessionType: "pomodoro", Status: "stopped", DurationSeconds: 2400, Note: "真实投入", StartedAt: at.Add(-40 * time.Minute), EndedAt: &at, Revision: 1, CreatedAt: at, UpdatedAt: at}
	reviewInsert(t, a, &session)
	result, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 4 || result.Counts["result"] != 1 || result.Counts["step"] != 0 || result.FocusMinutes != 40 {
		t.Fatalf("wrong facts: %+v", result)
	}
	byID := map[string]ReviewActivityItem{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	if _, ok := byID["daily"]; ok {
		t.Fatal("joint completion counted twice")
	}
	if byID["whole"].Status != "completed" || byID["reversed-step"].Status != "retracted" || byID["old-completion"].Status != "reopened" {
		t.Fatalf("lost correction semantics: %+v", byID)
	}
	if len(byID["whole"].RelatedEventIDs) != 1 || byID["whole"].GoalTitle != "论文推进" {
		t.Fatal("lost provenance")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "另一用户") {
		t.Fatal("leaked other user")
	}
	var count int64
	a.Store.DB.Model(&persistence.ProgressEvent{}).Count(&count)
	if count != 8 {
		t.Fatal("reading review wrote events")
	}
	a.Store.DB.Model(&persistence.DailyPlan{}).Count(&count)
	if count != 1 {
		t.Fatal("reading review scheduled new work")
	}
}

func TestReviewActivityWeekBoundaryPaginationAndFocusRounding(t *testing.T) {
	a, owner, _, _, tasks := reviewFixture(t)
	for i, stamp := range []string{"2026-11-08T15:59:59Z", "2026-11-08T16:00:00Z", "2026-11-15T15:59:59Z", "2026-11-15T16:00:00Z"} {
		at := reviewTime(stamp)
		event := persistence.ProgressEvent{ID: []string{"before", "begin", "end", "after"}[i], UserID: owner, TaskID: &tasks[0].ID, PlanItemID: reviewPointer("item"), Type: "step", Summary: "已保存步骤", EvidenceJSON: `{}`, OccurredAt: at, CreatedAt: at}
		reviewInsert(t, a, &event)
	}
	at := reviewTime("2026-11-10T09:00:00Z")
	for _, id := range []string{"seconds-a", "seconds-b"} {
		session := persistence.WorkSession{ID: id, UserID: owner, TaskID: tasks[1].ID, SessionType: "pomodoro", Status: "completed", DurationSeconds: 59, StartedAt: at.Add(-59 * time.Second), EndedAt: &at, CreatedAt: at, UpdatedAt: at, Revision: 1}
		reviewInsert(t, a, &session)
	}
	first, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Timezone != "Asia/Shanghai" || first.StartDate != "2026-11-09" || first.EndDate != "2026-11-15" || first.Total != 4 || first.FocusMinutes != 1 || !first.HasMore || first.NextOffset != 1 || first.Items[0].ID != "end" {
		t.Fatalf("wrong boundary/page: %+v", first)
	}
	second, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 1, first.NextOffset)
	if err != nil || second.Items[0].ID == first.Items[0].ID {
		t.Fatal("pagination repeated first item")
	}
	all, err := a.ReviewActivity(context.Background(), owner, "all", "", 200, 0)
	if err != nil || all.Total != 6 || all.StartDate != "" {
		t.Fatalf("all history: %+v %v", all, err)
	}
	if _, err := a.ReviewActivity(context.Background(), owner, "invalid", "", 100, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("accepted bad week: %v", err)
	}
	if _, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 201, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("accepted unbounded page: %v", err)
	}
}

func TestReviewActivityReadsOnlyCurrentAlreadyScheduledNextWeek(t *testing.T) {
	a, owner, other, _, tasks := reviewFixture(t)
	at := reviewTime("2026-11-10T09:00:00Z")
	plan := persistence.DailyPlan{ID: "next-plan", UserID: owner, LocalDate: "2026-11-16", Timezone: "Asia/Shanghai", Status: "active", CurrentRevision: 2, Revision: 2, CreatedAt: at, UpdatedAt: at}
	reviewInsert(t, a, &plan)
	for i, id := range []string{"obsolete-item", "current-item"} {
		item := persistence.DailyPlanItem{ID: id, LogicalID: "same-logical", UserID: owner, PlanID: plan.ID, TaskID: &tasks[4].ID, Kind: "core", Title: "未来安排", Status: "planned", PlanRevision: i + 1, TargetMinutes: 40, Revision: 1, CreatedAt: at, UpdatedAt: at}
		reviewInsert(t, a, &item)
	}
	otherPlan := persistence.DailyPlan{ID: "other-plan", UserID: other, LocalDate: "2026-11-16", Timezone: "Asia/Shanghai", Status: "active", CurrentRevision: 1, Revision: 1, CreatedAt: at, UpdatedAt: at}
	reviewInsert(t, a, &otherPlan)
	result, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Upcoming) != 1 || result.Upcoming[0].Date != "2026-11-16" || result.Upcoming[0].TaskID != tasks[4].ID {
		t.Fatalf("wrong next week: %+v", result.Upcoming)
	}
	var count int64
	a.Store.DB.Model(&persistence.DailyPlan{}).Count(&count)
	if count != 3 {
		t.Fatal("review created schedules")
	}
}

func TestReviewActivityExcludesImportedMaterialAndKeepsLocalTaskProgress(t *testing.T) {
	a, owner, other, _, tasks := reviewFixture(t)
	at := reviewTime("2026-11-12T08:00:00Z")
	for _, record := range []persistence.ExternalImport{
		{ID: "material", UserID: owner, SourceSystem: "fastread", SourceExternalID: "source-secret", SourceURL: "https://read.example/papers/1", ContentHash: "hash-secret", Kind: "research_material", Title: "已有论文笔记", Description: "原文摘要", Status: "candidate", MetadataJSON: `{"target_task_id":"joint"}`, ArtifactsJSON: `[]`, Revision: 1, CreatedAt: at, UpdatedAt: at},
		{ID: "detached", UserID: owner, SourceSystem: "fastwrite", SourceURL: "javascript:alert(1)", Kind: "generated_report", Title: "已解除关联资料", Status: "candidate", TaskID: &tasks[0].ID, MetadataJSON: `{"task_material_detached_from":"joint"}`, ArtifactsJSON: `[]`, Revision: 1, CreatedAt: at, UpdatedAt: at},
		{ID: "rejected", UserID: owner, Kind: "research_material", Title: "拒绝资料", Status: "rejected", MetadataJSON: `{}`, ArtifactsJSON: `[]`, Revision: 1, CreatedAt: at, UpdatedAt: at},
		{ID: "private-import", UserID: other, Kind: "research_material", Title: "别人的资料", Status: "candidate", MetadataJSON: `{}`, ArtifactsJSON: `[]`, Revision: 1, CreatedAt: at, UpdatedAt: at},
		{ID: "empty-source-import", UserID: owner, SourceExternalID: "unlabeled-evidence", Kind: "progress_evidence", Title: "没有来源标记的导入证据", Status: "candidate", TaskID: &tasks[0].ID, MetadataJSON: `{}`, ArtifactsJSON: `[]`, Revision: 1, CreatedAt: at, UpdatedAt: at},
	} {
		reviewInsert(t, a, &record)
	}
	progress := persistence.ProgressEvent{ID: "local-step", UserID: owner, TaskID: &tasks[0].ID, PlanItemID: reviewPointer("item"), Type: "step", Summary: "FastTask 中记录的进展", EvidenceJSON: `{}`, OccurredAt: at, CreatedAt: at}
	reviewInsert(t, a, &progress)
	result, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Counts["material"] != 0 || result.Counts["step"] != 1 || result.Items[0].ID != progress.ID || result.Items[0].Source != "fasttask" || result.Items[0].TaskID != tasks[0].ID {
		t.Fatalf("review mixed imported material with task progress: %+v", result)
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"source-secret", "hash-secret", "别人的资料"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("exposed %s", private)
		}
	}
	var count int64
	if err := a.Store.DB.Model(&persistence.ExternalImport{}).Count(&count).Error; err != nil || count != 5 {
		t.Fatalf("reading task review changed imported records: %d %v", count, err)
	}
}

func TestReviewActivityFocusEvidenceKeepsOneCardAndActualDuration(t *testing.T) {
	a, owner, _, _, tasks := reviewFixture(t)
	at := reviewTime("2026-11-12T08:00:00Z")
	for _, id := range []string{"focus-one", "focus-two"} {
		session := persistence.WorkSession{ID: id, UserID: owner, TaskID: tasks[0].ID, SessionType: "pomodoro", Status: "stopped", DurationSeconds: 59, StartedAt: at.Add(-59 * time.Second), EndedAt: &at, CreatedAt: at, UpdatedAt: at, Revision: 1}
		reviewInsert(t, a, &session)
	}
	event := persistence.ProgressEvent{ID: "time-confirmed", UserID: owner, TaskID: &tasks[0].ID, PlanItemID: reviewPointer("item"), Type: "time", Summary: "有效投入", EvidenceJSON: `{"work_session_ids":["focus-one","focus-two"]}`, OccurredAt: at, CreatedAt: at}
	reviewInsert(t, a, &event)
	result, err := a.ReviewActivity(context.Background(), owner, "2026-W46", "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.FocusMinutes != 1 || result.Items[0].DurationMinutes != 1 || result.Items[0].Summary != "有效投入" {
		t.Fatalf("duplicate or rounded evidence: %+v", result)
	}
}

func TestReviewActivityReadSnapshotDoesNotBlockProgressWrites(t *testing.T) {
	a, owner, _, _, tasks := reviewFixture(t)
	err := reviewReadSnapshot(a.Store.DB, func(tx *gorm.DB) error {
		var before int64
		if err := tx.Model(&persistence.ProgressEvent{}).Count(&before).Error; err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			at := reviewTime("2026-11-12T08:00:00Z")
			event := persistence.ProgressEvent{ID: "concurrent-step", UserID: owner, TaskID: &tasks[0].ID, Type: "step", Summary: "并行保存", EvidenceJSON: `{}`, OccurredAt: at, CreatedAt: at}
			done <- a.Store.DB.WithContext(ctx).Create(&event).Error
		}()
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		var after int64
		if err := tx.Model(&persistence.ProgressEvent{}).Count(&after).Error; err != nil {
			return err
		}
		if before != after {
			t.Fatalf("review mixed snapshot versions: %d -> %d", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("review blocked a progress save: %v", err)
	}
	var count int64
	if err := a.Store.DB.Model(&persistence.ProgressEvent{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("concurrent write missing: %d %v", count, err)
	}
}
