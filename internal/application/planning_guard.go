package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Protection follows a same-day logical item through revisions. Starting work,
// later pausing/stopping it, or reopening completion does not erase that history.
func protectedPlanItemsTx(tx *gorm.DB, userID string, plan persistence.DailyPlan, items []persistence.DailyPlanItem) (map[string]bool, error) {
	protected := map[string]bool{}
	if plan.UserID != userID {
		return nil, ErrNotFound
	}
	var logicalIDs []string
	if err := tx.Table("work_sessions AS ws").Joins("JOIN daily_plan_items AS dpi ON dpi.id = ws.daily_plan_item_id").Where("ws.user_id = ? AND dpi.user_id = ? AND dpi.plan_id = ?", userID, userID, plan.ID).Distinct().Pluck("COALESCE(NULLIF(dpi.logical_id, ''), dpi.id)", &logicalIDs).Error; err != nil {
		return nil, err
	}
	evidence := map[string]bool{}
	for _, id := range logicalIDs {
		evidence[id] = true
	}
	logicalIDs = nil
	if err := tx.Table("progress_events AS pe").Joins("JOIN daily_plan_items AS dpi ON dpi.id = pe.plan_item_id").Where("pe.user_id = ? AND dpi.user_id = ? AND dpi.plan_id = ?", userID, userID, plan.ID).Distinct().Pluck("COALESCE(NULLIF(dpi.logical_id, ''), dpi.id)", &logicalIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range logicalIDs {
		evidence[id] = true
	}
	for _, item := range items {
		if item.UserID != userID || item.PlanID != plan.ID {
			return nil, ErrNotFound
		}
		if item.Status == "satisfied" || item.Status == "in_progress" || evidence[logicalItemID(item)] {
			protected[item.ID] = true
		}
	}
	return protected, nil
}

// Explicit replanning can leave a corrected, withdrawn commitment in its old
// revision without erasing its evidence or occupying a current core slot.
// Keep every focus-bearing logical item, including running/paused sessions and
// past sessions whose time history still belongs to that commitment.
func retainProtectedItemForReplanTx(tx *gorm.DB, userID string, plan persistence.DailyPlan, item persistence.DailyPlanItem) (bool, error) {
	if item.Kind != "core" || item.Status != "skipped" {
		return true, nil
	}
	var sessions int64
	err := tx.Table("work_sessions AS ws").Joins("JOIN daily_plan_items AS dpi ON dpi.id = ws.daily_plan_item_id").
		Where("ws.user_id = ? AND dpi.user_id = ? AND dpi.plan_id = ? AND COALESCE(NULLIF(dpi.logical_id, ''), dpi.id) = ?", userID, userID, plan.ID, logicalItemID(item)).Count(&sessions).Error
	return sessions > 0, err
}

// Keep protected targets intact, including when they exceed a newly reduced
// budget. Only the remaining budget can fund new core commitments.
func remainingPlanBudget(items []persistence.DailyPlanItem, available int) (remainingMinutes, slots, targetMinutes int) {
	if available == 0 {
		available = defaultDailyBudget
	}
	remainingMinutes = available
	core := 0
	for _, item := range items {
		if item.Kind == "core" {
			core++
			if item.TargetMinutes > 0 {
				remainingMinutes -= item.TargetMinutes
			}
		}
	}
	if remainingMinutes < 0 {
		remainingMinutes = 0
	}
	slots = planningSlots(available) - core
	if maximum := 3 - core; slots > maximum {
		slots = maximum
	}
	// Keep a positive duration for each actual step without imposing the old
	// thirty-minute minimum, which prevented short days from having three steps.
	if affordable := remainingMinutes; slots > affordable {
		slots = affordable
	}
	if slots < 0 {
		slots = 0
	}
	if slots > 0 {
		targetMinutes = remainingMinutes / slots
	}
	return
}

type PlanningAvailability struct {
	Status       string `json:"status" enum:"ready,no_tasks,waiting_dependencies,no_ready_tasks,withdrawn,frozen,closed"`
	Reason       string `json:"reason"`
	ReadyCount   int    `json:"ready_count"`
	WaitingCount int    `json:"waiting_count"`
	CanReplan    bool   `json:"can_replan"`
}

// GetPlanningAvailability only reads the current aggregate and task eligibility;
// it never creates plans, reorders commitments, or infers future completion.
func (a *PlanService) GetPlanningAvailability(ctx context.Context, userID, planID string) (*PlanningAvailability, error) {
	var result *PlanningAvailability
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var plan persistence.DailyPlan
		if err := tx.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		items, err := livePlanItemsTx(tx, plan)
		if err != nil {
			return err
		}
		result, err = planningAvailabilityTx(tx, userID, plan, items)
		return err
	})
	return result, err
}

func planningAvailabilityTx(tx *gorm.DB, userID string, plan persistence.DailyPlan, items []persistence.DailyPlanItem) (*PlanningAvailability, error) {
	if plan.UserID != userID {
		return nil, ErrNotFound
	}
	availability := &PlanningAvailability{}
	tasks, settings, err := eligiblePlanningTasksTx(tx, userID, nil)
	if err != nil {
		return nil, err
	}
	availability.ReadyCount = len(tasks)
	openCount := 0
	if settings.PrimaryGoalID != nil {
		var all []persistence.Task
		if err := tx.Where("user_id = ? AND goal_id = ?", userID, *settings.PrimaryGoalID).Find(&all).Error; err != nil {
			return nil, err
		}
		byID, parents := map[string]persistence.Task{}, map[string]bool{}
		for _, task := range all {
			byID[task.ID] = task
			if task.ParentID != nil && task.DeletedAt == nil && task.Status != "superseded" && task.Status != "cancelled" {
				parents[*task.ParentID] = true
			}
		}
		for _, task := range all {
			if task.DeletedAt != nil || parents[task.ID] || (task.Type != "task" && task.Type != "action") || task.Status == "completed" || task.Status == "cancelled" || task.Status == "superseded" {
				continue
			}
			openCount++
			if strings.TrimSpace(task.MinimumAction) != "" && planningStateReady(task, byID) && !executableTaskEligibility(task, byID) {
				availability.WaitingCount++
			}
		}
	}
	protected, err := protectedPlanItemsTx(tx, userID, plan, items)
	if err != nil {
		return nil, err
	}
	scopeTasks, err := planningTaskIndexTx(tx, userID)
	if err != nil {
		return nil, err
	}
	protectedParents := protectedPlanParentIDs(items, protected, scopeTasks)
	kept := []persistence.DailyPlanItem{}
	protectedTask := map[string]bool{}
	todayTask := map[string]bool{}
	core, activeCore := 0, 0
	for _, item := range items {
		if item.Kind == "core" {
			core++
			if item.TaskID != nil {
				todayTask[*item.TaskID] = true
			}
			if item.Status != "skipped" && item.Status != "not_completed" {
				activeCore++
			}
		}
		if protected[item.ID] {
			retained, err := retainProtectedItemForReplanTx(tx, userID, plan, item)
			if err != nil {
				return nil, err
			}
			if !retained {
				continue
			}
			kept = append(kept, item)
			if item.TaskID != nil {
				protectedTask[*item.TaskID] = true
			}
		}
	}
	remainingReady, deferredScope := 0, 0
	for _, task := range tasks {
		if taskDescendsFrom(task, protectedParents, scopeTasks) {
			if !todayTask[task.ID] {
				deferredScope++
			}
			continue
		}
		if !todayTask[task.ID] {
			remainingReady++
		}
	}
	_, slots, _ := remainingPlanBudget(kept, settings.DailyBudgetMinutes)
	for _, task := range tasks {
		if taskDescendsFrom(task, protectedParents, scopeTasks) {
			continue
		}
		if !protectedTask[task.ID] && slots > 0 && plan.Status == "active" {
			availability.CanReplan = true
			break
		}
	}
	switch {
	case plan.Status != "active":
		availability.Status, availability.Reason, availability.CanReplan = "closed", "今日计划已关闭；历史安排与推进证据仍保留。", false
	case core > 0 && activeCore == 0:
		availability.Status = "withdrawn"
		switch {
		case availability.ReadyCount > 0:
			availability.Reason = fmt.Sprintf("原今日安排已移出；当前主要目标有 %d 个已就绪工作项，尚未安排今天，可明确重新安排。", availability.ReadyCount)
		case availability.WaitingCount > 0:
			availability.Reason = "原今日安排已移出；当前工作项正在等待前置任务最终完成。"
		default:
			availability.Reason = "原今日安排已移出；当前主要目标还没有可安排的工作项。"
		}
	case core > activeCore:
		availability.Status = "withdrawn"
		availability.Reason = fmt.Sprintf("今日保留 %d 项，另有 %d 项原安排已移出；历史和证据仍保留，可明确重新安排未开始的空位。", activeCore, core-activeCore)
		if availability.WaitingCount > 0 && remainingReady == 0 {
			availability.Reason += fmt.Sprintf("另有 %d 个工作项仍缺真实前置产物，不会直接补入。", availability.WaitingCount)
		}
	case core > 0 && core < 3:
		switch {
		case remainingReady > 0:
			availability.Status = "ready"
			_, free, _ := remainingPlanBudget(items, settings.DailyBudgetMinutes)
			if free == 0 {
				availability.Reason = fmt.Sprintf("今日已有 %d 项；原承诺已占用今日预算，其余已就绪工作顺延。已有工作和证据保留，可明确调整未开始的安排。", activeCore)
			} else {
				availability.Reason = fmt.Sprintf("今日已有 %d 项，仍可补入真实步骤；继续聊天添加工作后会补足可用名额，已有工作和证据保留。", activeCore)
			}
		case deferredScope > 0:
			availability.Status = "ready"
			availability.Reason = "保留父项今日承诺和成果；可在对话中明确选择不同产出的小步骤，自动安排暂顺延。"
			if availability.WaitingCount > 0 {
				availability.Reason += fmt.Sprintf("另有 %d 个工作项仍缺真实前置产物。", availability.WaitingCount)
			}
		case availability.WaitingCount > 0:
			availability.Status = "waiting_dependencies"
			availability.Reason = fmt.Sprintf("今日已有 %d 项，不足三项：另有 %d 个工作项仍缺真实前置产物；可继续聊天补充能独立开始的准备步骤，不会虚报前置完成。", activeCore, availability.WaitingCount)
		default:
			availability.Status = "ready"
			availability.Reason = fmt.Sprintf("今日已有 %d 项，不足三项：目前没有更多可独立开始的真实步骤；可继续聊天拆分或补充工作，不编造占位任务。", activeCore)
		}
	case len(protected) > 0:
		availability.Status, availability.Reason = "frozen", "已有开始、专注或完成证据的今日承诺会保留；今日三项已排满，其余工作顺延，明确重新安排只调整未开始的内容。"
	case plan.FrozenAt != nil && activeCore > 0:
		availability.Status, availability.Reason = "frozen", "今日三项已排满，其余工作顺延；继续聊天可添加或明确调整任务，已有工作和证据保留。"
	case availability.ReadyCount > 0:
		availability.Status, availability.Reason = "ready", fmt.Sprintf("当前主要目标有 %d 个已就绪工作项。", availability.ReadyCount)
	case availability.WaitingCount > 0:
		availability.Status, availability.Reason, availability.CanReplan = "waiting_dependencies", "工作项尚缺前置产物；前置任务最终完成后才可安排后续分析或正式成稿。", false
	case openCount == 0:
		availability.Status, availability.Reason, availability.CanReplan = "no_tasks", "当前主要目标还没有未完成的可执行工作项。", false
	default:
		availability.Status, availability.Reason, availability.CanReplan = "no_ready_tasks", "现有工作项暂未就绪，请检查暂停、受阻、祖先状态或缺少具体最小行动。", false
	}
	var structuralChange int64
	if err := tx.Model(&persistence.DailyPlanRevision{}).Where("plan_id = ? AND revision = ? AND reason = 'task structure updated'", plan.ID, plan.CurrentRevision).Count(&structuralChange).Error; err != nil {
		return nil, err
	}
	if structuralChange > 0 {
		availability.Reason = "任务结构已调整，原未开始的父任务、里程碑或重复子步骤安排已移入修订历史。" + availability.Reason
	}
	if plan.Status == "active" && plan.ManualItemsRemoved {
		availability.Status = "withdrawn"
		availability.Reason = fmt.Sprintf("已删除所选今日安排，今天保留 %d 项；需要补充时可明确重新安排今天。", core)
	}
	return availability, nil
}

// Distinguish waiting for an explicit prerequisite from paused/blocked ancestry.
// executableTaskAncestry includes dependencies for legacy execution callers, so
// it cannot be used as the status-only half of this projection.
func planningStateReady(task persistence.Task, byID map[string]persistence.Task) bool {
	seen := map[string]bool{}
	for current := task; ; {
		if seen[current.ID] || current.DeletedAt != nil || current.UserID != task.UserID || current.GoalID != task.GoalID || (current.Status != "ready" && current.Status != "in_progress") {
			return false
		}
		seen[current.ID] = true
		if current.ParentID == nil {
			return true
		}
		parent, found := byID[*current.ParentID]
		if !found {
			return false
		}
		current = parent
	}
}
