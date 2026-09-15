package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func coordFor(t *testing.T, f fixture, taskID string) persistence.TaskCoord {
	t.Helper()
	var coord persistence.TaskCoord
	if err := f.store.DB.Where("task_id = ? AND lens = ?", taskID, domain.LensResearchRisk).First(&coord).Error; err != nil {
		t.Fatalf("load coord: %v", err)
	}
	return coord
}

func addSession(t *testing.T, f fixture, taskID, status string, minutes int, endedAt time.Time) persistence.WorkSession {
	t.Helper()
	now := persistence.Now()
	session := persistence.WorkSession{
		ID: persistence.NewID("work"), UserID: f.user.ID, TaskID: taskID, SessionType: "pomodoro",
		Status: status, TargetMinutes: minutes, DurationSeconds: minutes * 60,
		Revision: 1, StartedAt: endedAt.Add(-time.Duration(minutes) * time.Minute), EndedAt: &endedAt,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.store.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func addProgressEvent(t *testing.T, f fixture, taskID, eventType string, occurredAt time.Time) {
	t.Helper()
	event := persistence.ProgressEvent{
		ID: persistence.NewID("progress"), UserID: f.user.ID, TaskID: &taskID, Type: eventType,
		Summary: eventType + " evidence", EvidenceJSON: "[]", OccurredAt: occurredAt, CreatedAt: occurredAt,
	}
	if err := f.store.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
}

func pendingProposal(t *testing.T, f fixture, patches []map[string]any) *persistence.Proposal {
	t.Helper()
	encoded, err := json.Marshal(patches)
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	proposal := persistence.Proposal{
		ID: persistence.NewID("proposal"), UserID: f.user.ID, GoalID: f.goal.ID, Status: "pending",
		Instruction: "lens test", PatchJSON: string(encoded), BaseRevision: 0, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.store.DB.Create(&proposal).Error; err != nil {
		t.Fatal(err)
	}
	return &proposal
}

func quadrantMinutes(review *WeeklyReview, key string) ReviewQuadrant {
	for _, quadrant := range review.Focus.Quadrants {
		if quadrant.Key == key {
			return quadrant
		}
	}
	panic("missing quadrant " + key)
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func TestSetTaskCoordCreatesThenRequiresIfMatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "方法未定，直接决定验收", -1)
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || !created.Pinned || created.Source != domain.CoordSourceUser {
		t.Fatalf("unexpected coord: %#v", created)
	}
	if created.X != 70 || created.Y != 85 || created.Lens != domain.LensResearchRisk || created.UserID != f.user.ID {
		t.Fatalf("unexpected coord: %#v", created)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 10, 20, "", -1); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second write without If-Match err=%v", err)
	}
	updated, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 10, 20, "", created.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.X != 10 || updated.Y != 20 || !updated.Pinned {
		t.Fatalf("unexpected update: %#v", updated)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, "unknown_lens", 10, 20, "", -1); !errors.Is(err, domain.ErrLens) {
		t.Fatalf("unknown lens err=%v", err)
	}
}

func TestSetTaskCoordRevisionMismatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[1].ID, domain.LensResearchRisk, 30, 40, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[1].ID, domain.LensResearchRisk, 60, 60, "", created.Revision+5); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale revision err=%v", err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[2].ID, domain.LensResearchRisk, 60, 60, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("If-Match without existing coord err=%v", err)
	}
	unchanged := coordFor(t, f, f.tasks[1].ID)
	if unchanged.Revision != 1 || unchanged.X != 30 || unchanged.Y != 40 {
		t.Fatalf("coord changed after rejected write: %#v", unchanged)
	}
}

func TestSetTaskCoordRejectsOtherUsersTask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "other-lens", PasswordHash: "hash", DisplayName: "Other", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, other.ID, f.tasks[0].ID, domain.LensResearchRisk, 10, 10, "", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user coord err=%v", err)
	}
	var count int64
	f.store.DB.Model(&persistence.TaskCoord{}).Count(&count)
	if count != 0 {
		t.Fatalf("coords written for foreign task: %d", count)
	}
}

func TestSetTaskCoordRejectsOutOfRange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, pair := range [][2]int{{101, 50}, {50, 101}, {-1, 50}, {50, -1}} {
		if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, pair[0], pair[1], "", -1); !errors.Is(err, domain.ErrCoord) {
			t.Fatalf("(%d,%d) err=%v", pair[0], pair[1], err)
		}
	}
	var count int64
	f.store.DB.Model(&persistence.TaskCoord{}).Count(&count)
	if count != 0 {
		t.Fatalf("out of range coords persisted: %d", count)
	}
}

func TestApplyProposalPersistsCoords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	patches := []map[string]any{{
		"op": "create", "client_ref": "n1", "type": "task", "title": "跑通基线实验",
		"success_criteria": "得到可复现的基线数字", "minimum_action": "打开实验脚本并运行一次",
		"priority": 80, "estimate_minutes": 50,
		"uncertainty": 70, "contribution": 85, "coord_rationale": "方法未定，直接决定验收",
	}}
	proposal := pendingProposal(t, f, patches)
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, proposal, patches); err != nil {
		t.Fatal(err)
	}
	var coords []persistence.TaskCoord
	if err := f.store.DB.Where("user_id = ?", f.user.ID).Find(&coords).Error; err != nil {
		t.Fatal(err)
	}
	if len(coords) != 1 {
		t.Fatalf("coords=%d", len(coords))
	}
	coord := coords[0]
	if coord.Source != domain.CoordSourceAgent || coord.Pinned || coord.X != 70 || coord.Y != 85 {
		t.Fatalf("unexpected coord: %#v", coord)
	}
	if coord.Rationale != "方法未定，直接决定验收" || coord.Lens != domain.LensResearchRisk || coord.Revision != 1 {
		t.Fatalf("unexpected coord: %#v", coord)
	}
}

func TestApplyProposalToleratesMissingCoords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	patches := []map[string]any{
		{"op": "create", "client_ref": "n1", "type": "task", "title": "无坐标节点", "success_criteria": "可验证结果", "minimum_action": "写下第一步", "priority": 70, "estimate_minutes": 25},
		{"op": "create", "client_ref": "n2", "type": "task", "title": "坐标越界节点", "success_criteria": "可验证结果", "minimum_action": "写下第一步", "priority": 70, "estimate_minutes": 25, "uncertainty": 700, "contribution": 40},
		{"op": "create", "client_ref": "n3", "type": "task", "title": "缺少一个坐标节点", "success_criteria": "可验证结果", "minimum_action": "写下第一步", "priority": 70, "estimate_minutes": 25, "contribution": 90},
	}
	proposal := pendingProposal(t, f, patches)
	created, err := f.app.ApplyProposal(ctx, f.user.ID, proposal, patches)
	if err != nil {
		t.Fatal(err)
	}
	if created != 3 {
		t.Fatalf("created=%d", created)
	}
	var tasks []persistence.Task
	titles := []string{"无坐标节点", "坐标越界节点", "缺少一个坐标节点"}
	if err := f.store.DB.Where("user_id = ? AND title IN ?", f.user.ID, titles).Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks=%d", len(tasks))
	}
	byTitle := map[string]persistence.Task{}
	for _, task := range tasks {
		byTitle[task.Title] = task
	}
	var count int64
	f.store.DB.Model(&persistence.TaskCoord{}).Where("task_id = ?", byTitle["无坐标节点"].ID).Count(&count)
	if count != 0 {
		t.Fatalf("missing coords produced %d rows", count)
	}
	outOfRange := coordFor(t, f, byTitle["坐标越界节点"].ID)
	if outOfRange.X != domain.CoordThreshold || outOfRange.Y != 40 {
		t.Fatalf("out of range coord not defaulted: %#v", outOfRange)
	}
	partial := coordFor(t, f, byTitle["缺少一个坐标节点"].ID)
	if partial.X != domain.CoordThreshold || partial.Y != 90 {
		t.Fatalf("partial coord not defaulted: %#v", partial)
	}
}

func TestApplyProposalDoesNotOverwritePinnedCoord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pinned, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 10, 90, "用户判断", -1)
	if err != nil {
		t.Fatal(err)
	}
	patches := []map[string]any{{
		"op": "update", "target_id": f.tasks[0].ID, "title": "换个标题",
		"uncertainty": 80, "contribution": 20, "coord_rationale": "Agent 重新打分",
	}}
	proposal := pendingProposal(t, f, patches)
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, proposal, patches); err != nil {
		t.Fatal(err)
	}
	current := coordFor(t, f, f.tasks[0].ID)
	if current.X != 10 || current.Y != 90 || current.Revision != pinned.Revision {
		t.Fatalf("pinned coord overwritten: %#v", current)
	}
	if current.Source != domain.CoordSourceUser || !current.Pinned || current.Rationale != "用户判断" {
		t.Fatalf("pinned coord metadata changed: %#v", current)
	}
	var task persistence.Task
	if err := f.store.DB.First(&task, "id = ?", f.tasks[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.Title != "换个标题" {
		t.Fatalf("structural update was skipped: %#v", task)
	}
}

func TestApplyProposalUpdatesUnpinnedCoord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	create := []map[string]any{{
		"op": "create", "client_ref": "n1", "type": "task", "title": "需要打分的任务",
		"success_criteria": "可验证结果", "minimum_action": "写下第一步", "priority": 70,
		"estimate_minutes": 25, "uncertainty": 20, "contribution": 30,
	}}
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, pendingProposal(t, f, create), create); err != nil {
		t.Fatal(err)
	}
	var task persistence.Task
	if err := f.store.DB.Where("user_id = ? AND title = ?", f.user.ID, "需要打分的任务").First(&task).Error; err != nil {
		t.Fatal(err)
	}
	update := []map[string]any{{"op": "update", "target_id": task.ID, "uncertainty": 75, "contribution": 88, "coord_rationale": "重新评估"}}
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, pendingProposal(t, f, update), update); err != nil {
		t.Fatal(err)
	}
	coord := coordFor(t, f, task.ID)
	if coord.X != 75 || coord.Y != 88 || coord.Revision != 2 || coord.Source != domain.CoordSourceAgent || coord.Pinned {
		t.Fatalf("unexpected coord after agent update: %#v", coord)
	}
	move := []map[string]any{{"op": "move", "target_id": task.ID, "parent_id": nil, "uncertainty": 5, "contribution": 5}}
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, pendingProposal(t, f, move), move); err != nil {
		t.Fatal(err)
	}
	unchanged := coordFor(t, f, task.ID)
	if unchanged.X != 75 || unchanged.Y != 88 || unchanged.Revision != 2 {
		t.Fatalf("move changed coords: %#v", unchanged)
	}
}

func TestWorkerFallbackPatchCarriesCoords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job, err := f.app.CreateJob(ctx, f.user.ID, "task_tree_generation", "goal", f.goal.ID, 0, map[string]any{"instruction": "拆解目标"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(f.app, time.Millisecond)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.First(job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "succeeded" {
		t.Fatalf("job status=%s error=%s", job.Status, job.ErrorMessage)
	}
	var proposal persistence.Proposal
	if err := f.store.DB.Where("job_id = ?", job.ID).First(&proposal).Error; err != nil {
		t.Fatal(err)
	}
	var patches []map[string]any
	if err := json.Unmarshal([]byte(proposal.PatchJSON), &patches); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.ApplyProposal(ctx, f.user.ID, &proposal, patches); err != nil {
		t.Fatal(err)
	}
	var coords []persistence.TaskCoord
	if err := f.store.DB.Where("user_id = ? AND source = ?", f.user.ID, domain.CoordSourceAgent).Find(&coords).Error; err != nil {
		t.Fatal(err)
	}
	if len(coords) != len(patches) {
		t.Fatalf("coords=%d patches=%d", len(coords), len(patches))
	}
	for _, coord := range coords {
		if err := domain.ValidateCoord(coord.X, coord.Y); err != nil {
			t.Fatalf("coord %#v invalid: %v", coord, err)
		}
		if coord.Pinned {
			t.Fatalf("agent coord pinned: %#v", coord)
		}
	}
}

func TestWeeklyReviewAggregatesByQuadrant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[1].ID, domain.LensResearchRisk, 20, 80, "", -1); err != nil {
		t.Fatal(err)
	}
	addSession(t, f, f.tasks[0].ID, "completed", 30, now)
	addSession(t, f, f.tasks[1].ID, "completed", 45, now)
	addSession(t, f, f.tasks[0].ID, "completed", 50, now.AddDate(0, 0, -7))

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if review.Focus.TotalMinutes != 75 {
		t.Fatalf("total minutes=%d", review.Focus.TotalMinutes)
	}
	if review.Focus.UnplottedMinutes != 0 {
		t.Fatalf("unplotted minutes=%d", review.Focus.UnplottedMinutes)
	}
	critical, main := quadrantMinutes(review, "A"), quadrantMinutes(review, "B")
	if critical.Minutes != 30 || critical.PreviousMinutes != 50 || critical.DeltaMinutes != -20 {
		t.Fatalf("quadrant A = %#v", critical)
	}
	if critical.Share != 0.4 || main.Minutes != 45 || main.Share != 0.6 {
		t.Fatalf("shares A=%v B=%v", critical.Share, main.Share)
	}
	if critical.Label != domain.QuadrantLabel("A") || main.Label != domain.QuadrantLabel("B") {
		t.Fatalf("labels = %q %q", critical.Label, main.Label)
	}
	if len(review.Focus.Quadrants) != 4 || quadrantMinutes(review, "C").Minutes != 0 || quadrantMinutes(review, "D").Minutes != 0 {
		t.Fatalf("quadrants = %#v", review.Focus.Quadrants)
	}
	if review.Week != domain.CurrentISOWeek(now, mustLocation(t, review.Timezone)) {
		t.Fatalf("week=%s", review.Week)
	}
	if review.StartDate == "" || review.EndDate == "" {
		t.Fatalf("dates = %s..%s", review.StartDate, review.EndDate)
	}
	if review.Summary.Source != "template" || review.Summary.Rule == "" || review.Summary.Text == "" {
		t.Fatalf("summary = %#v", review.Summary)
	}
	if review.LLMNote != "" {
		t.Fatalf("llm_note must stay empty in L2: %q", review.LLMNote)
	}
}

func TestWeeklyReviewExcludesInvalidatedSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "", -1); err != nil {
		t.Fatal(err)
	}
	addSession(t, f, f.tasks[0].ID, "invalidated", 90, now)
	addSession(t, f, f.tasks[0].ID, "running", 120, now)

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if review.Focus.TotalMinutes != 0 || quadrantMinutes(review, "A").Minutes != 0 {
		t.Fatalf("invalidated or running session counted: %#v", review.Focus)
	}
	if review.Summary.Rule != "empty" {
		t.Fatalf("rule=%s", review.Summary.Rule)
	}
}

func TestWeeklyReviewIncludesStoppedSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "", -1); err != nil {
		t.Fatal(err)
	}
	addSession(t, f, f.tasks[0].ID, "stopped", 15, now)
	addSession(t, f, f.tasks[1].ID, "completed", 44, now)

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if quadrantMinutes(review, "A").Minutes != 15 {
		t.Fatalf("stopped session not counted: %#v", quadrantMinutes(review, "A"))
	}
	if review.Focus.TotalMinutes != 59 || review.Focus.UnplottedMinutes != 44 {
		t.Fatalf("focus = %#v", review.Focus)
	}
}

func TestWeeklyReviewUnplottedNotInQuadrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "", -1); err != nil {
		t.Fatal(err)
	}
	addSession(t, f, f.tasks[0].ID, "completed", 20, now)
	addSession(t, f, f.tasks[1].ID, "completed", 10, now)

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if review.Focus.TotalMinutes != 30 || review.Focus.UnplottedMinutes != 10 {
		t.Fatalf("focus = %#v", review.Focus)
	}
	quadrants := map[string]int{}
	for _, quadrant := range review.Focus.Quadrants {
		quadrants[quadrant.Key] = quadrant.Minutes
	}
	if quadrants["A"] != 20 || quadrants["B"]+quadrants["C"]+quadrants["D"] != 0 {
		t.Fatalf("unplotted minutes leaked into quadrants: %v", quadrants)
	}
	if quadrantMinutes(review, "A").Share != 0.667 {
		t.Fatalf("share = %v", quadrantMinutes(review, "A").Share)
	}
}

func TestWeeklyReviewEmptyWeekIsValid(t *testing.T) {
	f := newFixture(t)
	review, err := f.app.WeeklyReview(context.Background(), f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Focus.Quadrants) != 4 {
		t.Fatalf("quadrants = %#v", review.Focus.Quadrants)
	}
	for _, quadrant := range review.Focus.Quadrants {
		if quadrant.Minutes != 0 || quadrant.Share != 0 || quadrant.PreviousMinutes != 0 || quadrant.DeltaMinutes != 0 {
			t.Fatalf("non zero quadrant in empty week: %#v", quadrant)
		}
	}
	if review.Focus.TotalMinutes != 0 || review.Evidence.Total != 0 || review.Evidence.MinimumActionShare != 0 {
		t.Fatalf("review = %#v", review)
	}
	if review.Summary.Rule != "empty" || review.Summary.Text != "本周没有记录到有效专注时间。" {
		t.Fatalf("summary = %#v", review.Summary)
	}
	if len(review.Stalled) != 0 {
		t.Fatalf("stalled = %#v", review.Stalled)
	}
	if review.LLMNote != "" {
		t.Fatalf("llm_note = %q", review.LLMNote)
	}
}

func TestWeeklyReviewEvidenceComposition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	localDate := persistence.Now().In(mustLocation(t, "Asia/Shanghai")).Format("2006-01-02")
	plan, items, err := f.app.CreateDailyPlan(ctx, f.user.ID, localDate, "Asia/Shanghai", nil, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("core items=%d", len(items))
	}
	for index, completionType := range []string{"result", "step", "minimum_action"} {
		if _, err := f.app.CompletePlanItem(ctx, f.user.ID, items[index].ID, items[index].Revision, completionType, "推进证据", nil); err != nil {
			t.Fatal(err)
		}
	}
	now := persistence.Now()
	timeItem := persistence.DailyPlanItem{
		ID: persistence.NewID("dpi"), UserID: f.user.ID, PlanID: plan.ID, PlanRevision: 1,
		Kind: "core", Title: "时间型推进", Commitment: "专注 50 分钟", MinimumAction: "开始",
		AllowedTypes: "result,step,time,minimum_action", Status: "satisfied", CompletionType: "time",
		CompletionSummary: "两个番茄", TargetMinutes: 50, Position: 4, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.store.DB.Create(&timeItem).Error; err != nil {
		t.Fatal(err)
	}

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if review.Evidence.Result != 1 || review.Evidence.Step != 1 || review.Evidence.MinimumAction != 1 || review.Evidence.Time != 1 {
		t.Fatalf("evidence = %#v", review.Evidence)
	}
	if review.Evidence.Total != 4 || review.Evidence.MinimumActionShare != 0.25 {
		t.Fatalf("evidence = %#v", review.Evidence)
	}
}

func TestWeeklyReviewRejectsUnknownWeekAndResolvesTimezone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.app.WeeklyReview(ctx, f.user.ID, "2026-W99", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad week err=%v", err)
	}
	review, err := f.app.WeeklyReview(ctx, f.user.ID, "2026-W37", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if review.Week != "2026-W37" || review.Timezone != "Europe/Berlin" {
		t.Fatalf("review = %s %s", review.Week, review.Timezone)
	}
	if review.StartDate != "2026-09-07" || review.EndDate != "2026-09-13" {
		t.Fatalf("dates = %s..%s", review.StartDate, review.EndDate)
	}
	fallback, err := f.app.WeeklyReview(ctx, f.user.ID, "", "Not/AZone")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone fallback = %s", fallback.Timezone)
	}
	if _, err := f.app.WeeklyReview(ctx, persistence.NewID("user"), "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user err=%v", err)
	}
}

func TestWeeklyReviewTimeEvidenceIsNotProgress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	stalled, moving := f.tasks[0], f.tasks[1]
	created := now.AddDate(0, 0, -20)
	for _, task := range []persistence.Task{stalled, moving} {
		if err := f.store.DB.Model(&persistence.Task{}).Where("id = ?", task.ID).Update("created_at", created).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, stalled.ID, domain.LensResearchRisk, 80, 90, "", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, moving.ID, domain.LensResearchRisk, 75, 80, "", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[2].ID, domain.LensResearchRisk, 20, 20, "", -1); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.Model(&persistence.Task{}).Where("id = ?", f.tasks[2].ID).Update("created_at", created).Error; err != nil {
		t.Fatal(err)
	}
	addProgressEvent(t, f, stalled.ID, "time", now.AddDate(0, 0, -2))
	addProgressEvent(t, f, stalled.ID, "minimum_action", now.AddDate(0, 0, -1))
	addProgressEvent(t, f, moving.ID, "step", now.AddDate(0, 0, -1))

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Stalled) != 1 {
		t.Fatalf("stalled = %#v", review.Stalled)
	}
	entry := review.Stalled[0]
	if entry.TaskID != stalled.ID || entry.Quadrant != "A" || entry.Title != stalled.Title || entry.GoalID != f.goal.ID {
		t.Fatalf("stalled entry = %#v", entry)
	}
	if entry.DaysSinceProgress != 20 {
		t.Fatalf("days since progress = %d, want 20", entry.DaysSinceProgress)
	}
	if review.Summary.Rule != "stalled_risk" || review.Summary.Text == "" {
		t.Fatalf("summary = %#v", review.Summary)
	}
}

func TestWeeklyReviewStalledExcludesClosedTasks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	created := now.AddDate(0, 0, -30)
	for index, status := range []string{"ready", "in_progress", "completed", "cancelled"} {
		task := f.tasks[index]
		if err := f.store.DB.Model(&persistence.Task{}).Where("id = ?", task.ID).Updates(map[string]any{"created_at": created, "status": status}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.SetTaskCoord(ctx, f.user.ID, task.ID, domain.LensResearchRisk, 80, 90, "", -1); err != nil {
			t.Fatal(err)
		}
	}
	extra := persistence.Task{GoalID: f.goal.ID, Type: "task", Title: "第五个风险任务", SuccessCriteria: "可验证结果", MinimumAction: "写下第一步", Priority: 60, EstimateMinutes: 25, Position: 9}
	if err := f.app.CreateTask(ctx, f.user.ID, &extra); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.Model(&persistence.Task{}).Where("id = ?", extra.ID).Update("created_at", created).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, extra.ID, domain.LensResearchRisk, 90, 95, "", -1); err != nil {
		t.Fatal(err)
	}

	review, err := f.app.WeeklyReview(ctx, f.user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Stalled) != 3 {
		t.Fatalf("stalled = %#v", review.Stalled)
	}
	for _, entry := range review.Stalled {
		if entry.TaskID == f.tasks[2].ID || entry.TaskID == f.tasks[3].ID {
			t.Fatalf("closed task reported as stalled: %#v", entry)
		}
	}
	if review.Stalled[0].DaysSinceProgress < review.Stalled[len(review.Stalled)-1].DaysSinceProgress {
		t.Fatalf("stalled not sorted by days: %#v", review.Stalled)
	}
}

func TestGoalMapReturnsLeavesWithDefaults(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	parent := persistence.Task{GoalID: f.goal.ID, Type: "milestone", Title: "里程碑", SuccessCriteria: "阶段成果", MinimumAction: "列出成果", Priority: 90, EstimateMinutes: 50, Position: 10}
	if err := f.app.CreateTask(ctx, f.user.ID, &parent); err != nil {
		t.Fatal(err)
	}
	child := persistence.Task{GoalID: f.goal.ID, ParentID: &parent.ID, Type: "action", Title: "子行动", SuccessCriteria: "可验证结果", MinimumAction: "写下第一步", Priority: 70, EstimateMinutes: 25, Position: 11}
	if err := f.app.CreateTask(ctx, f.user.ID, &child); err != nil {
		t.Fatal(err)
	}
	cancelled := f.tasks[3]
	if err := f.store.DB.Model(&persistence.Task{}).Where("id = ?", cancelled.ID).Update("status", "cancelled").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetTaskCoord(ctx, f.user.ID, f.tasks[0].ID, domain.LensResearchRisk, 70, 85, "方法未定", -1); err != nil {
		t.Fatal(err)
	}
	addSession(t, f, f.tasks[0].ID, "completed", 125, now.AddDate(0, 0, -40))
	addProgressEvent(t, f, f.tasks[0].ID, "step", now.AddDate(0, 0, -19))

	result, err := f.app.GoalMap(ctx, f.user.ID, f.goal.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalID != f.goal.ID || result.Lens != domain.LensResearchRisk || result.Threshold != domain.CoordThreshold {
		t.Fatalf("map header = %#v", result)
	}
	byID := map[string]GoalMapNode{}
	for _, node := range result.Nodes {
		byID[node.TaskID] = node
	}
	// 叶子集合：4 个 fixture 任务（其一 cancelled）+ 子行动；里程碑有子节点，被排除。
	if len(result.Nodes) != 4 {
		t.Fatalf("nodes = %d (%#v)", len(result.Nodes), result.Nodes)
	}
	if _, ok := byID[parent.ID]; ok {
		t.Fatal("milestone with children is not a leaf")
	}
	if _, ok := byID[child.ID]; !ok {
		t.Fatal("leaf action missing from map")
	}
	if _, ok := byID[cancelled.ID]; ok {
		t.Fatal("cancelled task present in map")
	}
	plotted := byID[f.tasks[0].ID]
	if plotted.X != 70 || plotted.Y != 85 || plotted.Quadrant != "A" || plotted.Source != domain.CoordSourceUser || !plotted.Pinned {
		t.Fatalf("plotted node = %#v", plotted)
	}
	if plotted.FocusMinutes != 125 || plotted.DaysSinceProgress != 19 || plotted.CoordID == "" || plotted.CoordRevision != 1 {
		t.Fatalf("plotted node = %#v", plotted)
	}
	defaulted := byID[child.ID]
	if defaulted.X != 50 || defaulted.Y != 50 || defaulted.Source != domain.CoordSourceDefault || defaulted.CoordID != "" {
		t.Fatalf("unplotted node = %#v", defaulted)
	}
	if result.Unplotted != 3 {
		t.Fatalf("unplotted = %d", result.Unplotted)
	}
	if _, err := f.app.GoalMap(ctx, f.user.ID, f.goal.ID, "unknown"); !errors.Is(err, domain.ErrLens) {
		t.Fatalf("unknown lens err=%v", err)
	}
	if _, err := f.app.GoalMap(ctx, f.user.ID, "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing goal err=%v", err)
	}
}
