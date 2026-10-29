package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

func planningContextTool(app *App) Tool {
	return funcTool{def: agent.ToolDefinition{Name: "get_planning_context", Description: "Read a compact consistent snapshot of the main research goal, task tree revision, up to 30 tasks and today's commitments. Read further task-tree pages when has_more=true before inferring absence or adding duplicate work. No writes.", Parameters: objectSchema(map[string]any{"goal_id": stringProp("Optional explicit goal; defaults to primary or newest active goal.")})}, fn: app.toolPlanningContext}
}
func (a *App) toolPlanningContext(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	result := map[string]any{}
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ?", tc.UserID).First(&user).Error; err != nil {
			return err
		}
		var settings persistence.PlanningSettings
		if err := tx.Where("user_id = ?", tc.UserID).First(&settings).Error; err != nil && !persistence.IsNotFound(err) {
			return err
		}
		id := stringArg(args, "goal_id")
		if id == "" {
			id = derefString(settings.PrimaryGoalID)
		}
		q := tx.Where("user_id = ? AND deleted_at IS NULL AND status = 'active'", tc.UserID)
		if id != "" {
			q = q.Where("id = ?", id)
		}
		var goal persistence.Goal
		err := q.Order("created_at DESC").First(&goal).Error
		if err != nil && !persistence.IsNotFound(err) {
			return err
		}
		result["goal"] = nil
		result["tasks"] = []any{}
		result["tree_revision"] = 0
		result["has_more"] = false
		if err == nil {
			result["goal"] = goal
			result["goal_id"] = goal.ID
			var revision int
			if err := tx.Model(&persistence.TaskTreeRevision{}).Where("user_id = ? AND goal_id = ?", tc.UserID, goal.ID).Select("COALESCE(MAX(revision),0)").Scan(&revision).Error; err != nil {
				return err
			}
			result["tree_revision"] = revision
			var tasks []persistence.Task
			if err := tx.Where("user_id = ? AND goal_id = ? AND deleted_at IS NULL", tc.UserID, goal.ID).Order("position,id").Limit(31).Find(&tasks).Error; err != nil {
				return err
			}
			result["has_more"] = len(tasks) > 30
			if len(tasks) > 30 {
				tasks = tasks[:30]
			}
			result["tasks"] = tasks
			result["next_offset"] = len(tasks)
		}
		_, zone := resolveZone("", user.Timezone)
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return err
		}
		date := time.Now().In(loc).Format("2006-01-02")
		var plan persistence.DailyPlan
		err = tx.Where("user_id = ? AND local_date = ? AND timezone = ?", tc.UserID, date, zone).First(&plan).Error
		result["plan"] = nil
		result["items"] = []any{}
		if err != nil && !persistence.IsNotFound(err) {
			return err
		}
		if err == nil {
			var items []persistence.DailyPlanItem
			if err := tx.Where("plan_id = ? AND plan_revision = ? AND status <> 'superseded'", plan.ID, plan.CurrentRevision).Order("kind,position").Find(&items).Error; err != nil {
				return err
			}
			result["plan"] = plan
			result["items"] = items
		}
		result["planning_settings"] = settings
		result["timezone"] = zone
		result["local_date"] = date
		return nil
	})
	return ToolResult{Result: result}, err
}

func observedTreeRevision(ctx context.Context, a *App, tc ToolContext, goalID string) (int, error) {
	var rows []persistence.AgentMessagePart
	err := a.Store.DB.WithContext(ctx).Table("agent_message_parts p").Select("p.*").Joins("JOIN agent_messages m ON m.id = p.message_id AND m.user_id = p.user_id").Where("m.run_id = ? AND p.user_id = ? AND p.tool_name IN ? AND p.is_error = 0 AND p.result_json <> ''", tc.RunID, tc.UserID, []string{"get_task_tree", "get_planning_context"}).Order("p.updated_at DESC").Find(&rows).Error
	if err != nil {
		return 0, err
	}
	for _, p := range rows {
		var v struct {
			GoalID       string `json:"goal_id"`
			TreeRevision int    `json:"tree_revision"`
		}
		if json.Unmarshal([]byte(p.ResultJSON), &v) == nil && v.GoalID == goalID {
			return v.TreeRevision, nil
		}
	}
	return 0, fmt.Errorf("read the current task tree or planning context before proposing a change")
}
