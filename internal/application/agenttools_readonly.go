package application

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// funcTool is a compact Tool implementation so readonly tools stay declarative.
type funcTool struct {
	def agent.ToolDefinition
	fn  func(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error)
}

func (t funcTool) Definition() agent.ToolDefinition { return t.def }
func (t funcTool) Level() ToolLevel                 { return ToolReadonly }
func (t funcTool) Execute(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	return t.fn(ctx, tc, args)
}

// toolError builds a structured, non-fatal tool result fed back to the model so
// it can self-correct (§5.2). It is not a Go error: a bad argument or a missing
// resource must not fail the whole run.
func toolError(format string, parts ...any) ToolResult {
	return ToolResult{IsError: true, Text: fmt.Sprintf(format, parts...)}
}

// NewReadonlyTools returns the seven read-only tools from agent.md §5.1. Every
// tool is a controlled projection of an existing application capability and
// scopes every query by ToolContext.UserID (invariant 5). None declares an
// identity argument (enforced at registry construction, §5.2).
func NewReadonlyTools(app *App) []Tool {
	return []Tool{
		funcTool{def: agent.ToolDefinition{
			Name:        "list_goals",
			Description: "List the user's goals with their acceptance criteria. Defaults to active goals; pass status to widen.",
			Parameters: objectSchema(map[string]any{
				"status": stringProp("Goal status filter.", "draft", "active", "paused", "completed", "abandoned", "archived"),
				"limit":  integerProp("Maximum goals to return (1-50).", 1, 50),
			}),
		}, fn: app.toolListGoals},

		funcTool{def: agent.ToolDefinition{
			Name:        "get_task_tree",
			Description: "Get the full task tree and current tree revision for one goal. Use the revision as the base for any task-tree proposal.",
			Parameters: objectSchema(map[string]any{
				"goal_id": stringProp("The goal whose task tree to read."),
			}, "goal_id"),
		}, fn: app.toolGetTaskTree},

		funcTool{def: agent.ToolDefinition{
			Name:        "get_daily_plan",
			Description: "Get the daily plan and its item statuses for a local date (defaults to today in the user's timezone). Shows the 0..3 core items and any support/input items.",
			Parameters: objectSchema(map[string]any{
				"local_date": stringProp("Local date YYYY-MM-DD; defaults to today."),
				"timezone":   stringProp("IANA timezone; defaults to the user's timezone."),
			}),
		}, fn: app.toolGetDailyPlan},

		funcTool{def: agent.ToolDefinition{
			Name:        "list_progress_events",
			Description: "List recent progress evidence (results, steps, time, minimum-action completions) to judge whether a task is really advancing or stuck.",
			Parameters: objectSchema(map[string]any{
				"goal_id": stringProp("Filter by goal."),
				"task_id": stringProp("Filter by task."),
				"limit":   integerProp("Maximum events to return (1-100).", 1, 100),
			}),
		}, fn: app.toolListProgressEvents},

		funcTool{def: agent.ToolDefinition{
			Name:        "get_weekly_review",
			Description: "Get the deterministic weekly review aggregate: focus minutes by quadrant, stalled high-risk tasks, completion evidence and the summary sentence.",
			Parameters: objectSchema(map[string]any{
				"week":     stringProp("ISO week like 2026-W37; defaults to the current week."),
				"timezone": stringProp("IANA timezone; defaults to the user's timezone."),
			}),
		}, fn: app.toolGetWeeklyReview},

		funcTool{def: agent.ToolDefinition{
			Name:        "list_stalled_tasks",
			Description: "List open tasks with no substantive progress (result/step) for at least 14 days, most-stalled first.",
			Parameters: objectSchema(map[string]any{
				"goal_id": stringProp("Filter by goal."),
				"limit":   integerProp("Maximum tasks to return (1-20).", 1, 20),
			}),
		}, fn: app.toolListStalledTasks},

		funcTool{def: agent.ToolDefinition{
			Name:        "get_task_coords",
			Description: "Get task coordinates under a decision lens (default research_risk): x=uncertainty, y=contribution, quadrant, source and whether the user pinned them.",
			Parameters: objectSchema(map[string]any{
				"goal_id": stringProp("Filter to tasks under one goal."),
				"task_id": stringProp("Filter to one task."),
				"lens":    stringProp("Decision lens id.", domain.LensResearchRisk),
			}),
		}, fn: app.toolGetTaskCoords},
	}
}

func (a *App) toolListGoals(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	db := a.Store.DB.WithContext(ctx)
	status := stringArg(args, "status")
	q := db.Model(&persistence.Goal{}).Where("user_id = ?", tc.UserID)
	if status != "" {
		q = q.Where("status = ?", status)
	} else {
		q = q.Where("status = ?", "active")
	}
	limit := clampInt(intArg(args, "limit", 25), 1, 50)
	var goals []persistence.Goal
	if err := q.Order("created_at DESC").Limit(limit).Find(&goals).Error; err != nil {
		return ToolResult{}, err
	}
	items := make([]map[string]any, 0, len(goals))
	for _, goal := range goals {
		items = append(items, map[string]any{
			"id": goal.ID, "title": goal.Title, "status": goal.Status,
			"success_criteria": goal.SuccessCriteria, "target_date": goal.TargetDate, "revision": goal.Revision,
		})
	}
	return ToolResult{Result: map[string]any{"count": len(items), "goals": items}}, nil
}

func (a *App) toolGetTaskTree(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	goalID := stringArg(args, "goal_id")
	if goalID == "" {
		return toolError("goal_id is required"), nil
	}
	db := a.Store.DB.WithContext(ctx)
	var goal persistence.Goal
	if err := db.Where("id = ? AND user_id = ?", goalID, tc.UserID).First(&goal).Error; err != nil {
		if persistence.IsNotFound(err) {
			return toolError("goal %s not found or not accessible", goalID), nil
		}
		return ToolResult{}, err
	}
	var tasks []persistence.Task
	if err := db.Where("goal_id = ? AND user_id = ?", goalID, tc.UserID).Order("position, id").Find(&tasks).Error; err != nil {
		return ToolResult{}, err
	}
	var revision int
	db.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goalID).Select("COALESCE(MAX(revision),0)").Scan(&revision)
	nodes := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		nodes = append(nodes, map[string]any{
			"id": task.ID, "parent_id": task.ParentID, "type": task.Type, "title": task.Title,
			"status": task.Status, "success_criteria": task.SuccessCriteria, "minimum_action": task.MinimumAction,
			"priority": task.Priority, "estimate_minutes": task.EstimateMinutes, "position": task.Position, "revision": task.Revision,
		})
	}
	return ToolResult{Result: map[string]any{
		"goal_id": goal.ID, "goal_title": goal.Title, "goal_status": goal.Status,
		"tree_revision": revision, "task_count": len(nodes),
		"tasks": nodes, "tree": BuildTree(tasks),
	}}, nil
}

func (a *App) toolGetDailyPlan(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	db := a.Store.DB.WithContext(ctx)
	var user persistence.User
	if err := db.Where("id = ?", tc.UserID).First(&user).Error; err != nil {
		if persistence.IsNotFound(err) {
			return toolError("user profile not found"), nil
		}
		return ToolResult{}, err
	}
	loc, zone := resolveZone(stringArg(args, "timezone"), user.Timezone)
	localDate := stringArg(args, "local_date")
	if localDate == "" {
		localDate = time.Now().In(loc).Format("2006-01-02")
	} else if err := ParseDateInZone(localDate, zone); err != nil {
		return toolError("local_date must be YYYY-MM-DD"), nil
	}
	var plan persistence.DailyPlan
	err := db.Where("user_id = ? AND local_date = ? AND timezone = ?", tc.UserID, localDate, zone).First(&plan).Error
	if err != nil {
		if persistence.IsNotFound(err) {
			return ToolResult{Result: map[string]any{"local_date": localDate, "timezone": zone, "plan": nil, "items": []any{}, "note": "no plan for this date"}}, nil
		}
		return ToolResult{}, err
	}
	var items []persistence.DailyPlanItem
	db.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("kind, position").Find(&items)
	projected := make([]map[string]any, 0, len(items))
	coreCount := 0
	for _, item := range items {
		if item.Kind == "core" {
			coreCount++
		}
		projected = append(projected, map[string]any{
			"id": item.ID, "kind": item.Kind, "task_id": item.TaskID, "title": item.Title,
			"commitment": item.Commitment, "minimum_action": item.MinimumAction, "status": item.Status,
			"completion_type": item.CompletionType, "target_minutes": item.TargetMinutes, "position": item.Position,
		})
	}
	return ToolResult{Result: map[string]any{
		"local_date": localDate, "timezone": zone, "plan_status": plan.Status,
		"plan_revision": plan.CurrentRevision, "core_count": coreCount, "items": projected,
	}}, nil
}

func (a *App) toolListProgressEvents(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	db := a.Store.DB.WithContext(ctx)
	q := db.Model(&persistence.ProgressEvent{}).Where("user_id = ?", tc.UserID)
	if goalID := stringArg(args, "goal_id"); goalID != "" {
		q = q.Where("goal_id = ?", goalID)
	}
	if taskID := stringArg(args, "task_id"); taskID != "" {
		q = q.Where("task_id = ?", taskID)
	}
	limit := clampInt(intArg(args, "limit", 20), 1, 100)
	var events []persistence.ProgressEvent
	if err := q.Order("occurred_at DESC").Limit(limit).Find(&events).Error; err != nil {
		return ToolResult{}, err
	}
	items := make([]map[string]any, 0, len(events))
	for _, event := range events {
		items = append(items, map[string]any{
			"id": event.ID, "type": event.Type, "summary": event.Summary,
			"goal_id": event.GoalID, "task_id": event.TaskID, "plan_item_id": event.PlanItemID,
			"occurred_at": event.OccurredAt.Format(time.RFC3339),
		})
	}
	return ToolResult{Result: map[string]any{"count": len(items), "events": items}}, nil
}

func (a *App) toolGetWeeklyReview(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	review, err := a.WeeklyReview(ctx, tc.UserID, stringArg(args, "week"), stringArg(args, "timezone"))
	if err != nil {
		if err == ErrValidation {
			return toolError("week must be a valid ISO week like 2026-W37"), nil
		}
		if err == ErrNotFound {
			return toolError("user profile not found"), nil
		}
		return ToolResult{}, err
	}
	return ToolResult{Result: review}, nil
}

func (a *App) toolListStalledTasks(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	db := a.Store.DB.WithContext(ctx)
	var user persistence.User
	if err := db.Where("id = ?", tc.UserID).First(&user).Error; err != nil {
		if persistence.IsNotFound(err) {
			return toolError("user profile not found"), nil
		}
		return ToolResult{}, err
	}
	loc, _ := resolveZone(user.Timezone)
	now := persistence.Now()
	progress, err := lastProgressByTask(db, tc.UserID)
	if err != nil {
		return ToolResult{}, err
	}
	q := db.Model(&persistence.Task{}).Where("user_id = ?", tc.UserID)
	if goalID := stringArg(args, "goal_id"); goalID != "" {
		q = q.Where("goal_id = ?", goalID)
	}
	var tasks []persistence.Task
	if err := q.Find(&tasks).Error; err != nil {
		return ToolResult{}, err
	}
	type stalled struct {
		id     string
		title  string
		goal   string
		days   int
		status string
	}
	found := make([]stalled, 0)
	for _, task := range tasks {
		if isClosedTask(task.Status) {
			continue
		}
		days := daysSinceProgress(progress[task.ID], task.CreatedAt, now, loc)
		if days < domain.StalledProgressDays {
			continue
		}
		found = append(found, stalled{task.ID, task.Title, task.GoalID, days, task.Status})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].days != found[j].days {
			return found[i].days > found[j].days
		}
		return found[i].id < found[j].id
	})
	limit := clampInt(intArg(args, "limit", 10), 1, 20)
	if len(found) > limit {
		found = found[:limit]
	}
	items := make([]map[string]any, 0, len(found))
	for _, item := range found {
		items = append(items, map[string]any{
			"task_id": item.id, "title": item.title, "goal_id": item.goal,
			"status": item.status, "days_since_progress": item.days,
		})
	}
	return ToolResult{Result: map[string]any{"count": len(items), "threshold_days": domain.StalledProgressDays, "tasks": items}}, nil
}

func (a *App) toolGetTaskCoords(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	lens := normalizeLens(stringArg(args, "lens"))
	if !domain.ValidLens(lens) {
		return toolError("unknown lens %q; available: %s", lens, domain.LensResearchRisk), nil
	}
	db := a.Store.DB.WithContext(ctx)
	q := db.Model(&persistence.TaskCoord{}).Where("user_id = ? AND lens = ?", tc.UserID, lens)
	if taskID := stringArg(args, "task_id"); taskID != "" {
		q = q.Where("task_id = ?", taskID)
	}
	if goalID := stringArg(args, "goal_id"); goalID != "" {
		// Verify goal ownership, then restrict to its tasks.
		var goal persistence.Goal
		if err := db.Where("id = ? AND user_id = ?", goalID, tc.UserID).First(&goal).Error; err != nil {
			if persistence.IsNotFound(err) {
				return toolError("goal %s not found or not accessible", goalID), nil
			}
			return ToolResult{}, err
		}
		var ids []string
		db.Model(&persistence.Task{}).Where("goal_id = ? AND user_id = ?", goalID, tc.UserID).Pluck("id", &ids)
		if len(ids) == 0 {
			return ToolResult{Result: map[string]any{"lens": lens, "count": 0, "coords": []any{}}}, nil
		}
		q = q.Where("task_id IN ?", ids)
	}
	var coords []persistence.TaskCoord
	if err := q.Find(&coords).Error; err != nil {
		return ToolResult{}, err
	}
	// Attach task titles for context.
	titles := map[string]string{}
	if len(coords) > 0 {
		ids := make([]string, 0, len(coords))
		for _, coord := range coords {
			ids = append(ids, coord.TaskID)
		}
		var tasks []persistence.Task
		db.Where("user_id = ? AND id IN ?", tc.UserID, ids).Find(&tasks)
		for _, task := range tasks {
			titles[task.ID] = task.Title
		}
	}
	items := make([]map[string]any, 0, len(coords))
	for _, coord := range coords {
		items = append(items, map[string]any{
			"task_id": coord.TaskID, "title": titles[coord.TaskID], "lens": coord.Lens,
			"x": coord.X, "y": coord.Y, "quadrant": domain.Quadrant(coord.X, coord.Y),
			"quadrant_label": domain.QuadrantLabel(domain.Quadrant(coord.X, coord.Y)),
			"source":         coord.Source, "pinned": coord.Pinned, "rationale": coord.Rationale, "revision": coord.Revision,
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i]["task_id"].(string) < items[j]["task_id"].(string) })
	return ToolResult{Result: map[string]any{"lens": lens, "threshold": domain.CoordThreshold, "count": len(items), "coords": items}}, nil
}

// --- argument helpers ---

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func intArg(args map[string]any, key string, fallback int) int {
	if value, ok := args[key].(float64); ok {
		return int(value)
	}
	return fallback
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
