package application

import (
	"encoding/json"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Selection permission is separate from generated/explicit content provenance.
// It records identity anchors only; no task graph or public payload is changed.
func planningTaskIndexTx(tx *gorm.DB, userID string) (map[string]persistence.Task, error) {
	var tasks []persistence.Task
	if err := tx.Where("user_id = ? AND deleted_at IS NULL", userID).Find(&tasks).Error; err != nil {
		return nil, err
	}
	byID := map[string]persistence.Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	return byID, nil
}

func planningParentChain(task persistence.Task, byID map[string]persistence.Task) ([]string, bool) {
	chain := []string{}
	seen := map[string]bool{task.ID: true}
	for task.ParentID != nil {
		id := *task.ParentID
		parent, found := byID[id]
		if !found || seen[id] || parent.UserID != task.UserID || parent.GoalID != task.GoalID {
			return nil, false
		}
		seen[id] = true
		chain = append(chain, id)
		task = parent
	}
	return chain, true
}

func protectedPlanParentIDs(items []persistence.DailyPlanItem, protected map[string]bool, byID map[string]persistence.Task) map[string]bool {
	parents := map[string]bool{}
	for _, task := range byID {
		if task.ParentID != nil && task.Status != "cancelled" && task.Status != "superseded" {
			parents[*task.ParentID] = true
		}
	}
	selected := map[string]bool{}
	for _, item := range items {
		if item.Kind == "core" && item.TaskID != nil && parents[*item.TaskID] && protected[item.ID] && (item.Status == "planned" || item.Status == "in_progress" || item.Status == "satisfied") {
			selected[*item.TaskID] = true
		}
	}
	return selected
}

func approvedPlanItemParentScopeTx(tx *gorm.DB, userID string, item persistence.DailyPlanItem, task persistence.Task, byID map[string]persistence.Task) (bool, error) {
	if item.UserID != userID || task.UserID != userID || item.TaskID == nil || *item.TaskID != task.ID {
		return false, nil
	}
	chain, valid := planningParentChain(task, byID)
	if !valid {
		return false, nil
	}
	var revisions []persistence.DailyPlanRevision
	if err := tx.Where("plan_id = ? AND revision <= ?", item.PlanID, item.PlanRevision).Order("revision ASC").Find(&revisions).Error; err != nil {
		return false, err
	}
	for _, revision := range revisions {
		var snapshot []persistence.DailyPlanItem
		if json.Unmarshal([]byte(revision.PlanSnapshot), &snapshot) != nil {
			return false, nil
		}
		for _, recorded := range snapshot {
			if recorded.ID != item.ID {
				continue
			}
			if recorded.TaskID == nil || *recorded.TaskID != task.ID || recorded.Kind != "core" {
				return false, nil
			}
			var input struct {
				Scopes map[string][]string `json:"selected_task_parent_chains"`
			}
			if json.Unmarshal([]byte(revision.InputSnapshot), &input) != nil {
				return false, nil
			}
			approved, present := input.Scopes[task.ID]
			if !present || len(approved) != len(chain) {
				return false, nil
			}
			for i, id := range chain {
				if approved[i] != id {
					return false, nil
				}
			}
			return true, nil
		}
	}
	return false, nil
}
