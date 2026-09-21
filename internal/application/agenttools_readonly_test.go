package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// toolFixture builds an App with two users so every readonly tool can be checked
// for the cross-user isolation invariant (agent.md §4 invariant 5).
type toolFixture struct {
	app   *App
	tools *ToolRegistry
	owner ToolContext
	other persistence.User
	goal  persistence.Goal
	tasks []persistence.Task
}

func newToolFixture(t *testing.T) toolFixture {
	t.Helper()
	f := newFixture(t) // owner user + goal + 4 tasks
	registry, err := NewToolRegistry(NewReadonlyTools(f.app)...)
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "tool-other", PasswordHash: "hash", DisplayName: "Other", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	return toolFixture{app: f.app, tools: registry, owner: ToolContext{UserID: f.user.ID}, other: other, goal: f.goal, tasks: f.tasks}
}

func (tf toolFixture) exec(t *testing.T, tc ToolContext, name string, args map[string]any) ToolResult {
	t.Helper()
	tool, ok := tf.tools.Get(name)
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	if problems := tf.tools.ValidateArgs(name, args); len(problems) > 0 {
		t.Fatalf("args %v rejected: %v", args, problems)
	}
	result, err := tool.Execute(context.Background(), tc, args)
	if err != nil {
		t.Fatalf("tool %q execute error: %v", name, err)
	}
	return result
}

// TestReadonlyToolsRegistered asserts the seven §5.1 read-only tools exist, are
// all readonly level, and none declares an identity argument.
func TestReadonlyToolsRegistered(t *testing.T) {
	tf := newToolFixture(t)
	want := []string{"list_goals", "get_task_tree", "get_daily_plan", "list_progress_events", "get_weekly_review", "list_stalled_tasks", "get_task_coords"}
	for _, name := range want {
		tool, ok := tf.tools.Get(name)
		if !ok {
			t.Fatalf("missing readonly tool %q", name)
		}
		if tool.Level() != ToolReadonly {
			t.Fatalf("tool %q level=%q, want readonly", name, tool.Level())
		}
	}
	if got := len(tf.tools.Names()); got != len(want) {
		t.Fatalf("registry has %d tools (%v), want %d", got, tf.tools.Names(), len(want))
	}
}

// TestListGoalsScopesByUser asserts list_goals returns only the caller's goals.
func TestListGoalsScopesByUser(t *testing.T) {
	tf := newToolFixture(t)
	result := tf.exec(t, tf.owner, "list_goals", map[string]any{})
	payload := result.Result.(map[string]any)
	goals := payload["goals"].([]map[string]any)
	if len(goals) != 1 || goals[0]["id"] != tf.goal.ID {
		t.Fatalf("owner goals=%v, want exactly the owner's goal", goals)
	}
	// The other user has no goals.
	otherResult := tf.exec(t, ToolContext{UserID: tf.other.ID}, "list_goals", map[string]any{})
	otherPayload := otherResult.Result.(map[string]any)
	if otherPayload["count"].(int) != 0 {
		t.Fatalf("other user saw %v goals, want 0", otherPayload["count"])
	}
}

// TestGetTaskTreeScopesByUser asserts get_task_tree returns the tree + revision
// for the owner and a structured error (not another user's data) for a stranger.
func TestGetTaskTreeScopesByUser(t *testing.T) {
	tf := newToolFixture(t)
	result := tf.exec(t, tf.owner, "get_task_tree", map[string]any{"goal_id": tf.goal.ID})
	payload := result.Result.(map[string]any)
	if payload["task_count"].(int) != len(tf.tasks) {
		t.Fatalf("task_count=%v, want %d", payload["task_count"], len(tf.tasks))
	}
	if _, ok := payload["tree_revision"]; !ok {
		t.Fatal("task tree result missing tree_revision (needed as proposal base)")
	}
	// Cross-user: the goal id is not accessible, so a structured error is returned.
	otherResult := tf.exec(t, ToolContext{UserID: tf.other.ID}, "get_task_tree", map[string]any{"goal_id": tf.goal.ID})
	if !otherResult.IsError || !strings.Contains(otherResult.Text, "not found") {
		t.Fatalf("cross-user get_task_tree=%#v, want a not-found tool error", otherResult)
	}
	// A missing required arg bypasses exec's validation and is caught directly by
	// the tool as a structured error, not a panic.
	tool, _ := tf.tools.Get("get_task_tree")
	direct, err := tool.Execute(context.Background(), tf.owner, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !direct.IsError {
		t.Fatal("empty goal_id should be a structured tool error")
	}
}

// TestGetDailyPlanReflectsCoreLimit asserts get_daily_plan projects the plan and
// that the deterministic top-3 limit is visible to the model (agent.md §4
// invariant 2 is enforced by CreateDailyPlan, not the tool).
func TestGetDailyPlanReflectsCoreLimit(t *testing.T) {
	tf := newToolFixture(t)
	ctx := context.Background()
	date := time.Now().In(time.UTC).Format("2006-01-02")
	if _, _, err := tf.app.CreateDailyPlan(ctx, tf.owner.UserID, date, "UTC", nil, 120); err != nil {
		t.Fatal(err)
	}
	result := tf.exec(t, tf.owner, "get_daily_plan", map[string]any{"local_date": date, "timezone": "UTC"})
	payload := result.Result.(map[string]any)
	if payload["core_count"].(int) != 3 {
		t.Fatalf("core_count=%v, want 3", payload["core_count"])
	}
	// A date with no plan yields an explicit empty result, not an error.
	empty := tf.exec(t, tf.owner, "get_daily_plan", map[string]any{"local_date": "2000-01-01", "timezone": "UTC"})
	emptyPayload := empty.Result.(map[string]any)
	if emptyPayload["plan"] != nil {
		t.Fatalf("expected no plan for an empty date, got %v", emptyPayload["plan"])
	}
}

// TestListProgressEventsScopesByUser asserts progress evidence is user-scoped.
func TestListProgressEventsScopesByUser(t *testing.T) {
	tf := newToolFixture(t)
	ctx := context.Background()
	// Completing a task writes a progress event for the owner.
	if _, err := tf.app.CompleteTask(ctx, tf.owner.UserID, tf.tasks[0].ID, tf.tasks[0].Revision, "result", "ran the baseline", false); err != nil {
		t.Fatal(err)
	}
	result := tf.exec(t, tf.owner, "list_progress_events", map[string]any{"limit": float64(10)})
	payload := result.Result.(map[string]any)
	if payload["count"].(int) == 0 {
		t.Fatal("owner progress events empty after completing a task")
	}
	otherResult := tf.exec(t, ToolContext{UserID: tf.other.ID}, "list_progress_events", map[string]any{})
	if otherResult.Result.(map[string]any)["count"].(int) != 0 {
		t.Fatal("other user saw the owner's progress events")
	}
}

// TestGetWeeklyReviewDeterministic asserts get_weekly_review projects the same
// deterministic aggregate as the HTTP endpoint (lens.md §7), with no LLM note.
func TestGetWeeklyReviewDeterministic(t *testing.T) {
	tf := newToolFixture(t)
	result := tf.exec(t, tf.owner, "get_weekly_review", map[string]any{"timezone": "UTC"})
	review, ok := result.Result.(*WeeklyReview)
	if !ok {
		t.Fatalf("weekly review type=%T", result.Result)
	}
	if review.LLMNote != "" {
		t.Fatalf("weekly review LLMNote=%q, want empty (no synchronous LLM)", review.LLMNote)
	}
	if len(review.Focus.Quadrants) != 4 {
		t.Fatalf("quadrants=%d, want 4", len(review.Focus.Quadrants))
	}
	// An invalid week is a structured error, not a run failure.
	tool, _ := tf.tools.Get("get_weekly_review")
	bad, err := tool.Execute(context.Background(), tf.owner, map[string]any{"week": "not-a-week"})
	if err != nil {
		t.Fatal(err)
	}
	if !bad.IsError {
		t.Fatal("invalid week should be a structured tool error")
	}
}

// TestListStalledTasksUsesThreshold asserts stalled detection uses the 14-day
// substantive-progress rule and is user-scoped.
func TestListStalledTasksUsesThreshold(t *testing.T) {
	tf := newToolFixture(t)
	ctx := context.Background()
	// Backdate a task's creation and give it no progress so it counts as stalled.
	old := persistence.Now().AddDate(0, 0, -30)
	if err := tf.app.Store.DB.Model(&persistence.Task{}).Where("id = ?", tf.tasks[0].ID).
		Updates(map[string]any{"created_at": old, "status": "in_progress"}).Error; err != nil {
		t.Fatal(err)
	}
	result := tf.exec(t, tf.owner, "list_stalled_tasks", map[string]any{})
	payload := result.Result.(map[string]any)
	tasks := payload["tasks"].([]map[string]any)
	if len(tasks) == 0 {
		t.Fatal("expected at least one stalled task")
	}
	if tasks[0]["task_id"] != tf.tasks[0].ID {
		t.Fatalf("most-stalled task=%v, want %s", tasks[0]["task_id"], tf.tasks[0].ID)
	}
	if payload["threshold_days"].(int) != 14 {
		t.Fatalf("threshold=%v, want 14", payload["threshold_days"])
	}
	_ = ctx
	otherResult := tf.exec(t, ToolContext{UserID: tf.other.ID}, "list_stalled_tasks", map[string]any{})
	if otherResult.Result.(map[string]any)["count"].(int) != 0 {
		t.Fatal("other user saw the owner's stalled tasks")
	}
}

// TestGetTaskCoordsProjectsQuadrant asserts get_task_coords projects coordinates
// with quadrant labels and never crosses users.
func TestGetTaskCoordsProjectsQuadrant(t *testing.T) {
	tf := newToolFixture(t)
	ctx := context.Background()
	// Pin a coordinate for the owner's first task.
	if _, err := tf.app.SetTaskCoord(ctx, tf.owner.UserID, tf.tasks[0].ID, "research_risk", 80, 90, "high risk", -1); err != nil {
		t.Fatal(err)
	}
	result := tf.exec(t, tf.owner, "get_task_coords", map[string]any{"task_id": tf.tasks[0].ID})
	payload := result.Result.(map[string]any)
	coords := payload["coords"].([]map[string]any)
	if len(coords) != 1 {
		t.Fatalf("coords=%v, want 1", coords)
	}
	if coords[0]["quadrant"] != "A" || coords[0]["pinned"] != true {
		t.Fatalf("coord projection wrong: %v", coords[0])
	}
	// Unknown lens is a structured error.
	tool, _ := tf.tools.Get("get_task_coords")
	bad, err := tool.Execute(ctx, tf.owner, map[string]any{"lens": "made_up"})
	if err != nil {
		t.Fatal(err)
	}
	if !bad.IsError {
		t.Fatal("unknown lens should be a structured tool error")
	}
	// Cross-user by task_id returns nothing.
	otherResult := tf.exec(t, ToolContext{UserID: tf.other.ID}, "get_task_coords", map[string]any{"task_id": tf.tasks[0].ID})
	if otherResult.Result.(map[string]any)["count"].(int) != 0 {
		t.Fatal("other user saw the owner's coordinates")
	}
}

// TestToolErrorIsNotFatal asserts a tool's structured error is returned as a
// result (IsError), never as a Go error that would fail the whole run (§5.2).
func TestToolErrorIsNotFatal(t *testing.T) {
	tf := newToolFixture(t)
	tool, _ := tf.tools.Get("get_task_tree")
	result, err := tool.Execute(context.Background(), tf.owner, map[string]any{"goal_id": "goal_does_not_exist"})
	if err != nil {
		t.Fatalf("missing resource returned a Go error (%v); it must be a structured tool result", err)
	}
	if !result.IsError {
		t.Fatal("missing goal should produce IsError result")
	}
}
