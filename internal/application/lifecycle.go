package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// These guards are shared by CRUD and approved agent operations. A historical
// foreign key remains valid after deletion, but never grants permission to edit
// or reactivate that resource through an old proposal.
func RequireLiveGoalTx(tx *gorm.DB, userID, id string) (*persistence.Goal, error) {
	var goal persistence.Goal
	if err := tx.Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).First(&goal).Error; err != nil {
		return nil, notFound(err)
	}
	return &goal, nil
}

func RequireLiveTaskTx(tx *gorm.DB, userID, id string) (*persistence.Task, error) {
	var task persistence.Task
	if err := tx.Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).First(&task).Error; err != nil {
		return nil, notFound(err)
	}
	if _, err := RequireLiveGoalTx(tx, userID, task.GoalID); err != nil {
		return nil, err
	}
	seen := map[string]bool{task.ID: true}
	for parentID := task.ParentID; parentID != nil; {
		if seen[*parentID] {
			return nil, ErrConflict
		}
		seen[*parentID] = true
		var parent persistence.Task
		if err := tx.Where("id = ? AND user_id = ? AND goal_id = ? AND deleted_at IS NULL", *parentID, userID, task.GoalID).First(&parent).Error; err != nil {
			return nil, notFound(err)
		}
		parentID = parent.ParentID
	}
	return &task, nil
}

func validateGoalFields(goal *persistence.Goal) error {
	goal.Title = strings.TrimSpace(goal.Title)
	goal.Description = strings.TrimSpace(goal.Description)
	goal.SuccessCriteria = strings.TrimSpace(goal.SuccessCriteria)
	if goal.Title == "" || utf8.RuneCountInString(goal.Title) > 200 || goal.SuccessCriteria == "" || utf8.RuneCountInString(goal.SuccessCriteria) > 2000 || utf8.RuneCountInString(goal.Description) > 4000 {
		return ErrValidation
	}
	if goal.TargetDate != nil {
		date := strings.TrimSpace(*goal.TargetDate)
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return ErrValidation
		}
		goal.TargetDate = &date
	}
	return nil
}

func validateTaskFields(task *persistence.Task) error {
	task.Type, task.Title = strings.TrimSpace(task.Type), strings.TrimSpace(task.Title)
	task.Description = strings.TrimSpace(task.Description)
	task.SuccessCriteria, task.MinimumAction = strings.TrimSpace(task.SuccessCriteria), strings.TrimSpace(task.MinimumAction)
	task.BlockedReason = strings.TrimSpace(task.BlockedReason)
	if (task.Type != "milestone" && task.Type != "task" && task.Type != "action") || task.Title == "" || utf8.RuneCountInString(task.Title) > 300 || task.SuccessCriteria == "" || utf8.RuneCountInString(task.SuccessCriteria) > 2000 || task.MinimumAction == "" || utf8.RuneCountInString(task.MinimumAction) > 2000 || utf8.RuneCountInString(task.Description) > 8000 || utf8.RuneCountInString(task.BlockedReason) > 2000 || task.Priority < 0 || task.Priority > 100 || task.EstimateMinutes < 1 || task.EstimateMinutes > 1440 || task.Position < 0 {
		return ErrValidation
	}
	return nil
}

func changeText(changes map[string]any, key string, destination *string) error {
	value, exists := changes[key]
	if !exists {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return ErrValidation
	}
	*destination = strings.TrimSpace(text)
	return nil
}

func changeInt(changes map[string]any, key string, destination *int) error {
	value, exists := changes[key]
	if !exists {
		return nil
	}
	switch typed := value.(type) {
	case int:
		*destination = typed
	case int64:
		*destination = int(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < 0 || typed > 1<<31-1 {
			return ErrValidation
		}
		*destination = int(typed)
	default:
		return ErrValidation
	}
	return nil
}

func (a *GoalService) createGoalTx(tx *gorm.DB, userID string, goal *persistence.Goal) error {
	if err := validateGoalFields(goal); err != nil {
		return err
	}
	now := persistence.Now()
	goal.ID, goal.UserID, goal.Status, goal.Revision = persistence.NewID("goal"), userID, "active", 1
	goal.DeletedAt, goal.DeletionID = nil, ""
	goal.CreatedAt, goal.UpdatedAt = now, now
	if err := tx.Create(goal).Error; err != nil {
		return err
	}
	return createOutbox(tx, userID, "goal.created", map[string]any{"goal_id": goal.ID})
}

func (a *GoalService) updateGoalTx(tx *gorm.DB, userID, id string, expected int, changes map[string]any) (*persistence.Goal, error) {
	goal, err := RequireLiveGoalTx(tx, userID, id)
	if err != nil {
		return nil, err
	}
	if goal.Revision != expected {
		return nil, ErrRevision
	}
	for key := range changes {
		switch key {
		case "title", "description", "success_criteria", "status", "target_date":
		default:
			return nil, ErrValidation
		}
	}
	for key, destination := range map[string]*string{"title": &goal.Title, "description": &goal.Description, "success_criteria": &goal.SuccessCriteria} {
		if err := changeText(changes, key, destination); err != nil {
			return nil, err
		}
	}
	oldStatus := goal.Status
	if err := changeText(changes, "status", &goal.Status); err != nil {
		return nil, err
	}
	if err := domain.ValidateGoalTransition(oldStatus, goal.Status); err != nil {
		return nil, ErrConflict
	}
	if value, exists := changes["target_date"]; exists {
		goal.TargetDate = nil
		if value != nil {
			date, ok := value.(string)
			if !ok {
				return nil, ErrValidation
			}
			goal.TargetDate = &date
		}
	}
	if err := validateGoalFields(goal); err != nil {
		return nil, err
	}
	now := persistence.Now()
	goal.Revision, goal.UpdatedAt = expected+1, now
	result := tx.Model(&persistence.Goal{}).Where("id = ? AND user_id = ? AND revision = ? AND deleted_at IS NULL", id, userID, expected).
		Updates(map[string]any{"title": goal.Title, "description": goal.Description, "success_criteria": goal.SuccessCriteria, "target_date": goal.TargetDate, "status": goal.Status, "revision": goal.Revision, "updated_at": now})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrRevision
	}
	if oldStatus != goal.Status {
		ids, err := goalTaskIDsTx(tx, userID, id, false)
		if err != nil {
			return nil, err
		}
		if goal.Status != "active" {
			if err := releaseTodayItemsTx(tx, userID, ids, now); err != nil {
				return nil, err
			}
		}
		if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
			return nil, err
		}
	}
	if err := createOutbox(tx, userID, "goal.updated", map[string]any{"goal_id": id}); err != nil {
		return nil, err
	}
	return goal, nil
}

func (a *GoalService) updateTaskTx(tx *gorm.DB, userID, id string, expected int, changes map[string]any, deferEffects ...bool) (*persistence.Task, error) {
	task, err := RequireLiveTaskTx(tx, userID, id)
	if err != nil {
		return nil, err
	}
	if task.Revision != expected {
		return nil, ErrRevision
	}
	before := *task
	for key := range changes {
		switch key {
		case "type", "title", "description", "success_criteria", "minimum_action", "blocked_reason", "status", "priority", "estimate_minutes", "position", "parent_id", "depends_on":
		default:
			return nil, ErrValidation
		}
	}
	for key, destination := range map[string]*string{"type": &task.Type, "title": &task.Title, "description": &task.Description, "success_criteria": &task.SuccessCriteria, "minimum_action": &task.MinimumAction, "blocked_reason": &task.BlockedReason} {
		if err := changeText(changes, key, destination); err != nil {
			return nil, err
		}
	}
	for key, destination := range map[string]*int{"priority": &task.Priority, "estimate_minutes": &task.EstimateMinutes, "position": &task.Position} {
		if err := changeInt(changes, key, destination); err != nil {
			return nil, err
		}
	}
	oldStatus := task.Status
	if raw, exists := changes["depends_on"]; exists {
		dependencies, err := taskDependencyList(raw)
		if err != nil {
			return nil, err
		}
		task.DependsOn = dependencies
	}
	if _, exists := changes["status"]; exists {
		if err := changeText(changes, "status", &task.Status); err != nil {
			return nil, err
		}
		if task.Status == "completed" {
			return nil, ErrValidation
		}
		if err := domain.ValidateTaskTransition(oldStatus, task.Status); err != nil {
			return nil, ErrConflict
		}
	}
	if value, exists := changes["parent_id"]; exists {
		task.ParentID = nil
		if value != nil {
			parentID, ok := value.(string)
			if !ok || strings.TrimSpace(parentID) == "" {
				return nil, ErrValidation
			}
			parentID = strings.TrimSpace(parentID)
			parent, err := RequireLiveTaskTx(tx, userID, parentID)
			if err != nil || parent.GoalID != task.GoalID || parent.Status == "completed" || parent.Status == "cancelled" || parent.Status == "superseded" {
				return nil, ErrValidation
			}
			parents := map[string]string{}
			var tasks []persistence.Task
			if err := tx.Where("user_id = ? AND goal_id = ?", userID, task.GoalID).Find(&tasks).Error; err != nil {
				return nil, err
			}
			for _, item := range tasks {
				if item.ParentID != nil {
					parents[item.ID] = *item.ParentID
				}
			}
			if domain.WouldCreateCycle(task.ID, parentID, parents) {
				return nil, ErrValidation
			}
			task.ParentID = &parentID
		}
	}
	if err := validateTaskFields(task); err != nil {
		return nil, err
	}
	_, moved := changes["parent_id"]
	_, changedDependencies := changes["depends_on"]
	if moved || changedDependencies {
		if err := validateTaskDependenciesTx(tx, *task); err != nil {
			return nil, err
		}
	}
	if task.Status != "blocked" {
		task.BlockedReason = ""
	}
	now := persistence.Now()
	task.Revision, task.UpdatedAt = expected+1, now
	dependencyJSON, _ := json.Marshal(task.DependsOn)
	result := tx.Model(&persistence.Task{}).Where("id = ? AND user_id = ? AND revision = ? AND deleted_at IS NULL", id, userID, expected).
		Updates(map[string]any{"type": task.Type, "title": task.Title, "description": task.Description, "success_criteria": task.SuccessCriteria, "minimum_action": task.MinimumAction, "blocked_reason": task.BlockedReason, "status": task.Status, "priority": task.Priority, "estimate_minutes": task.EstimateMinutes, "position": task.Position, "parent_id": task.ParentID, "depends_on": string(dependencyJSON), "revision": task.Revision, "updated_at": now})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrRevision
	}
	if err := syncEditedTaskTodayItemsTx(tx, userID, before, *task, now); err != nil {
		return nil, err
	}
	// A multi-operation approved proposal snapshots and refreshes once after
	// the entire ordered patch is valid, within the same transaction.
	if len(deferEffects) > 0 && deferEffects[0] {
		return task, nil
	}
	if err := a.snapshotTaskTree(tx, userID, task.GoalID, "manual task update", "user"); err != nil {
		return nil, err
	}
	if oldStatus != task.Status && task.Status != "ready" && task.Status != "in_progress" {
		subtree, err := subtreeTasksTx(tx, userID, *task)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(subtree))
		for _, child := range subtree {
			ids = append(ids, child.ID)
		}
		if err := releaseTodayItemsTx(tx, userID, ids, now); err != nil {
			return nil, err
		}
	}
	if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
		return nil, err
	}
	if err := createOutbox(tx, userID, "task.updated", map[string]any{"task_id": id}); err != nil {
		return nil, err
	}
	return task, nil
}

func goalTaskIDsTx(tx *gorm.DB, userID, goalID string, liveOnly bool) ([]string, error) {
	ids := []string{}
	query := tx.Model(&persistence.Task{}).Where("user_id = ? AND goal_id = ?", userID, goalID)
	if liveOnly {
		query = query.Where("deleted_at IS NULL")
	}
	err := query.Pluck("id", &ids).Error
	return ids, err
}

func subtreeTasksTx(tx *gorm.DB, userID string, root persistence.Task) ([]persistence.Task, error) {
	var tasks []persistence.Task
	if err := tx.Where("user_id = ? AND goal_id = ?", userID, root.GoalID).Find(&tasks).Error; err != nil {
		return nil, err
	}
	children := map[string][]persistence.Task{}
	for _, task := range tasks {
		if task.ParentID != nil {
			children[*task.ParentID] = append(children[*task.ParentID], task)
		}
	}
	result, pending, seen := []persistence.Task{}, []persistence.Task{root}, map[string]bool{}
	for len(pending) > 0 {
		task := pending[0]
		pending = pending[1:]
		if seen[task.ID] {
			continue
		}
		seen[task.ID] = true
		result = append(result, task)
		pending = append(pending, children[task.ID]...)
	}
	return result, nil
}

func noActiveTaskSessionsTx(tx *gorm.DB, userID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	var count int64
	if err := tx.Model(&persistence.WorkSession{}).Where("user_id = ? AND task_id IN ? AND status IN ('running','paused')", userID, taskIDs).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: end the active focus session before deleting this task or goal", ErrActiveSession)
	}
	return nil
}

// An explicit lifecycle operation overrides the frozen day. It skips only the
// remaining commitments; completion evidence and real sessions are untouched.
func releaseTodayItemsTx(tx *gorm.DB, userID string, taskIDs []string, now time.Time) error {
	if len(taskIDs) == 0 {
		return nil
	}
	var user persistence.User
	if err := tx.First(&user, "id = ?", userID).Error; err != nil {
		return notFound(err)
	}
	loc, zone := resolveZone(user.Timezone)
	var plans []persistence.DailyPlan
	if err := tx.Where("user_id = ? AND local_date = ? AND timezone = ? AND status = 'active'", userID, now.In(loc).Format("2006-01-02"), zone).Find(&plans).Error; err != nil {
		return err
	}
	for _, plan := range plans {
		result := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND task_id IN ? AND status IN ('planned','in_progress')", plan.ID, plan.CurrentRevision, taskIDs).
			Updates(map[string]any{"status": "skipped", "revision": gorm.Expr("revision + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			if err := touchPlanTx(tx, plan.ID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// Recovery creates new future reservations; it never silently reinstates a
// cancelled commitment or a completion from earlier today.
func refreshLifecycleScheduleTx(tx *gorm.DB, userID string, now time.Time, futureOnly ...bool) error {
	preserveEmptyToday := len(futureOnly) > 0 && futureOnly[0]
	var user persistence.User
	if err := tx.First(&user, "id = ?", userID).Error; err != nil {
		return notFound(err)
	}
	loc, zone := resolveZone(user.Timezone)
	date := now.In(loc).Format("2006-01-02")
	// Moving a subtree or reopening a prerequisite can remove execution
	// eligibility without changing the dependent task's own status. Withdraw
	// only unfulfilled commitments; evidence and real focus sessions stay intact.
	var pending []persistence.DailyPlanItem
	if err := tx.Table("daily_plan_items AS i").Select("i.*").
		Joins("JOIN daily_plans p ON p.id = i.plan_id AND p.current_revision = i.plan_revision").
		Where("p.user_id = ? AND p.local_date = ? AND p.timezone = ? AND p.status = 'active' AND i.status IN ('planned','in_progress') AND i.task_id IS NOT NULL", userID, date, zone).
		Find(&pending).Error; err != nil {
		return err
	}
	withdraw := []string{}
	for _, item := range pending {
		eligible, err := taskExecutableTx(tx, userID, *item.TaskID)
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
			return err
		}
		if !eligible {
			withdraw = append(withdraw, *item.TaskID)
		}
	}
	if err := releaseTodayItemsTx(tx, userID, withdraw, now); err != nil {
		return err
	}
	var today persistence.DailyPlan
	err := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, date, zone).First(&today).Error
	if err == nil && today.FrozenAt == nil && preserveEmptyToday {
		if err := tx.Model(&persistence.DailyPlan{}).Where("id = ?", today.ID).Updates(map[string]any{"frozen_at": now, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
	}
	if err != nil && !persistence.IsNotFound(err) {
		return err
	}
	if persistence.IsNotFound(err) && preserveEmptyToday {
		today = persistence.DailyPlan{ID: persistence.NewID("plan"), UserID: userID, LocalDate: date, Timezone: zone, Status: "active", AlgorithmVersion: "deterministic-v2", CurrentRevision: 1, Revision: 1, FrozenAt: &now, CreatedAt: now, UpdatedAt: now}
		settings, err := planningSettingsTx(tx, userID)
		if err != nil {
			return err
		}
		day := ScheduleDay{Plan: today, Items: []persistence.DailyPlanItem{}}
		if err := NewPlanService(&persistence.Store{DB: tx}).saveScheduleDayTx(tx, &day, false, settings, now); err != nil {
			return err
		}
	}
	_, err = NewPlanService(&persistence.Store{DB: tx}).refreshScheduleTx(tx, user, date, loc, zone, now)
	return err
}

func (a *GoalService) DeleteGoal(ctx context.Context, userID, id string, expected int) (*persistence.Goal, error) {
	var goal *persistence.Goal
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		goal, err = a.deleteGoalTx(tx, userID, id, expected)
		return err
	})
	return goal, err
}

func (a *GoalService) deleteGoalTx(tx *gorm.DB, userID, id string, expected int) (*persistence.Goal, error) {
	goal, err := RequireLiveGoalTx(tx, userID, id)
	if err != nil {
		return nil, err
	}
	if goal.Revision != expected {
		return nil, ErrRevision
	}
	ids, err := goalTaskIDsTx(tx, userID, id, true)
	if err != nil {
		return nil, err
	}
	if err := noActiveTaskSessionsTx(tx, userID, ids); err != nil {
		return nil, err
	}
	now, batch := persistence.Now(), persistence.NewID("deletion")
	goal.DeletedAt, goal.DeletionID, goal.Revision, goal.UpdatedAt = &now, batch, expected+1, now
	if err := tx.Model(&persistence.Goal{}).Where("id = ? AND revision = ? AND deleted_at IS NULL", id, expected).
		Updates(map[string]any{"deleted_at": now, "deletion_id": batch, "revision": expected + 1, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := tx.Model(&persistence.Task{}).Where("id IN ? AND deleted_at IS NULL", ids).
			Updates(map[string]any{"deleted_at": now, "deletion_id": batch, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return nil, err
		}
	}
	if err := tx.Model(&persistence.PlanningSettings{}).Where("user_id = ? AND primary_goal_id = ?", userID, id).
		Updates(map[string]any{"primary_goal_id": nil, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
		return nil, err
	}
	if err := a.snapshotTaskTree(tx, userID, id, "goal deleted", "user"); err != nil {
		return nil, err
	}
	if err := releaseTodayItemsTx(tx, userID, ids, now); err != nil {
		return nil, err
	}
	if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
		return nil, err
	}
	if err := createOutbox(tx, userID, "goal.deleted", map[string]any{"goal_id": id, "deletion_id": batch}); err != nil {
		return nil, err
	}
	return goal, nil
}

func (a *GoalService) RestoreGoal(ctx context.Context, userID, id string, expected int) (*persistence.Goal, error) {
	var goal persistence.Goal
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&goal).Error; err != nil {
			return notFound(err)
		}
		if goal.Revision != expected {
			return ErrRevision
		}
		if goal.DeletedAt == nil || goal.DeletionID == "" {
			return ErrConflict
		}
		now, batch := persistence.Now(), goal.DeletionID
		if err := tx.Model(&persistence.Goal{}).Where("id = ? AND revision = ?", id, expected).
			Updates(map[string]any{"deleted_at": nil, "deletion_id": "", "revision": expected + 1, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.Task{}).Where("user_id = ? AND goal_id = ? AND deletion_id = ?", userID, id, batch).
			Updates(map[string]any{"deleted_at": nil, "deletion_id": "", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		goal.DeletedAt, goal.DeletionID, goal.Revision, goal.UpdatedAt = nil, "", expected+1, now
		if err := a.snapshotTaskTree(tx, userID, id, "goal restored", "user"); err != nil {
			return err
		}
		if err := refreshLifecycleScheduleTx(tx, userID, now, true); err != nil {
			return err
		}
		return createOutbox(tx, userID, "goal.restored", map[string]any{"goal_id": id, "deletion_id": batch})
	})
	return &goal, err
}

func (a *GoalService) DeleteTask(ctx context.Context, userID, id string, expected int) (*persistence.Task, error) {
	var task *persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		task, err = a.deleteTaskTx(tx, userID, id, expected)
		return err
	})
	return task, err
}

func (a *GoalService) deleteTaskTx(tx *gorm.DB, userID, id string, expected int) (*persistence.Task, error) {
	root, err := RequireLiveTaskTx(tx, userID, id)
	if err != nil {
		return nil, err
	}
	if root.Revision != expected {
		return nil, ErrRevision
	}
	tasks, err := subtreeTasksTx(tx, userID, *root)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, task := range tasks {
		if task.DeletedAt == nil {
			ids = append(ids, task.ID)
		}
	}
	if err := noActiveTaskSessionsTx(tx, userID, ids); err != nil {
		return nil, err
	}
	now, batch := persistence.Now(), persistence.NewID("deletion")
	if err := tx.Model(&persistence.Task{}).Where("user_id = ? AND id IN ? AND deleted_at IS NULL", userID, ids).
		Updates(map[string]any{"deleted_at": now, "deletion_id": batch, "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
		return nil, err
	}
	root.DeletedAt, root.DeletionID, root.Revision, root.UpdatedAt = &now, batch, expected+1, now
	if err := a.snapshotTaskTree(tx, userID, root.GoalID, "task subtree deleted", "user"); err != nil {
		return nil, err
	}
	if err := releaseTodayItemsTx(tx, userID, ids, now); err != nil {
		return nil, err
	}
	if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
		return nil, err
	}
	if err := createOutbox(tx, userID, "task.deleted", map[string]any{"task_id": id, "deletion_id": batch}); err != nil {
		return nil, err
	}
	return root, nil
}

func (a *GoalService) RestoreTask(ctx context.Context, userID, id string, expected int) (*persistence.Task, error) {
	var task *persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		task, err = a.restoreTaskTx(tx, userID, id, expected)
		return err
	})
	return task, err
}

func (a *GoalService) restoreTaskTx(tx *gorm.DB, userID, id string, expected int) (*persistence.Task, error) {
	var root persistence.Task
	if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&root).Error; err != nil {
		return nil, notFound(err)
	}
	if root.Revision != expected {
		return nil, ErrRevision
	}
	if root.DeletedAt == nil || root.DeletionID == "" {
		return nil, ErrConflict
	}
	if _, err := RequireLiveGoalTx(tx, userID, root.GoalID); err != nil {
		return nil, fmt.Errorf("%w: restore the goal first", ErrConflict)
	}
	if root.ParentID != nil {
		parent, err := RequireLiveTaskTx(tx, userID, *root.ParentID)
		if err != nil || parent.GoalID != root.GoalID {
			return nil, fmt.Errorf("%w: restore the parent task first", ErrConflict)
		}
	}
	tasks, err := subtreeTasksTx(tx, userID, root)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, task := range tasks {
		if task.DeletionID == root.DeletionID {
			ids = append(ids, task.ID)
		}
	}
	now, batch := persistence.Now(), root.DeletionID
	if err := tx.Model(&persistence.Task{}).Where("user_id = ? AND id IN ? AND deletion_id = ?", userID, ids, batch).
		Updates(map[string]any{"deleted_at": nil, "deletion_id": "", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
		return nil, err
	}
	root.DeletedAt, root.DeletionID, root.Revision, root.UpdatedAt = nil, "", expected+1, now
	if err := a.snapshotTaskTree(tx, userID, root.GoalID, "task subtree restored", "user"); err != nil {
		return nil, err
	}
	if err := refreshLifecycleScheduleTx(tx, userID, now, true); err != nil {
		return nil, err
	}
	if err := createOutbox(tx, userID, "task.restored", map[string]any{"task_id": id, "deletion_id": batch}); err != nil {
		return nil, err
	}
	return &root, nil
}
