package application

import (
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Executing work requires the task and every ancestor to remain open. This
// status rule is shared by deterministic planning, focus starts and correction
// of daily evidence; correcting history does not reopen a closed task.
func executableTaskAncestry(task persistence.Task, byID map[string]persistence.Task) bool {
	return executableTaskEligibility(task, byID)
}

func taskExecutableTx(tx *gorm.DB, userID, taskID string) (bool, error) {
	task, err := RequireLiveTaskTx(tx, userID, taskID)
	if err != nil {
		return false, err
	}
	goal, err := RequireLiveGoalTx(tx, userID, task.GoalID)
	if err != nil {
		return false, err
	}
	if goal.Status != "active" {
		return false, nil
	}
	var tasks []persistence.Task
	// Keep deleted rows in the map so deleted prerequisites fail qualification
	// and can become valid again after an explicit restoration.
	if err := tx.Where("user_id = ? AND goal_id = ?", userID, task.GoalID).Find(&tasks).Error; err != nil {
		return false, err
	}
	byID := make(map[string]persistence.Task, len(tasks))
	for _, current := range tasks {
		byID[current.ID] = current
	}
	return executableTaskEligibility(*task, byID), nil
}
