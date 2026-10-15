package application

import (
	"context"
	"fmt"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// IsTaskExecutable projects the same qualification used by planning, focus and
// new daily evidence. It never changes a task or its historical commitments.
func (a *GoalService) IsTaskExecutable(ctx context.Context, userID, taskID string) (bool, error) {
	var eligible bool
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		eligible, err = taskExecutableTx(tx, userID, taskID)
		return err
	})
	return eligible, err
}

type TodaySummaryItem struct {
	Title         string `json:"title"`
	Commitment    string `json:"commitment"`
	MinimumAction string `json:"minimum_action"`
	TargetMinutes int    `json:"target_minutes"`
}

type TodaySummary struct {
	Status         string             `json:"status"`
	Message        string             `json:"message"`
	Items          []TodaySummaryItem `json:"items"`
	MilestoneCount int                `json:"milestone_count"`
	WorkItemCount  int                `json:"work_item_count"`
}

// The changed goal and the account's effective planning direction can differ.
// This is explanatory text only: today's retained rows may span older goals.
func approvalGoalContextTx(tx *gorm.DB, userID, goalID string) (string, error) {
	if goalID == "" {
		return "", nil
	}
	var changed persistence.Goal
	if err := tx.Where("id = ? AND user_id = ?", goalID, userID).First(&changed).Error; err != nil {
		if persistence.IsNotFound(err) {
			return "", nil // Keep legacy/missing references conservative and private.
		}
		return "", err
	}
	settings, err := planningSettingsTx(tx, userID)
	if err != nil {
		return "", err
	}
	primaryID, err := primaryGoalTx(tx, userID, settings)
	if err != nil || primaryID == goalID {
		return "", err
	}
	if primaryID == "" {
		return fmt.Sprintf("本次更新的是「%s」；当前尚无有效排期方向。", changed.Title), nil
	}
	var primary persistence.Goal
	if err := tx.Where("id = ? AND user_id = ?", primaryID, userID).First(&primary).Error; err != nil {
		return "", err
	}
	return fmt.Sprintf("本次更新的是「%s」；当前排期方向为「%s」。", changed.Title, primary.Title), nil
}

func (a *App) approvalTodaySummary(ctx context.Context, userID, goalID string) TodaySummary {
	var result TodaySummary
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		// A busy read can retry after counts were already projected. Only the
		// successful attempt's counts and item list belong in the receipt.
		result = TodaySummary{Status: "unavailable", Message: "变更已生效；今日安排暂时无法读取，请到今日推进查看。", Items: []TodaySummaryItem{}}
		var tasks []persistence.Task
		if goalID != "" {
			if err := tx.Where("user_id = ? AND goal_id = ? AND deleted_at IS NULL", userID, goalID).Find(&tasks).Error; err != nil {
				return err
			}
			for _, task := range tasks {
				if task.Type == "milestone" {
					result.MilestoneCount++
				} else if task.Type == "task" || task.Type == "action" {
					result.WorkItemCount++
				}
			}
		}
		var user persistence.User
		if err := tx.First(&user, "id = ?", userID).Error; err != nil {
			return err
		}
		loc, zone := resolveZone(user.Timezone)
		goalContext, err := approvalGoalContextTx(tx, userID, goalID)
		if err != nil {
			return err
		}
		var plan persistence.DailyPlan
		if err := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, persistence.Now().In(loc).Format("2006-01-02"), zone).First(&plan).Error; err != nil {
			if persistence.IsNotFound(err) {
				result.Status, result.Message = "not_arranged", "任务树已更新；今天尚未建立安排。可在今日推进查看并明确安排。"
				if goalContext != "" {
					result.Message = goalContext + "未自动切换排期方向。" + result.Message
				}
				return nil
			}
			return err
		}
		items, err := livePlanItemsTx(tx, plan)
		if err != nil {
			return err
		}
		availability, err := planningAvailabilityTx(tx, userID, plan, items)
		if err != nil {
			return err
		}
		result.Status, result.Message = availability.Status, availability.Reason
		if plan.Status == "active" {
			for _, item := range items {
				if item.Kind == "core" && (item.Status == "planned" || item.Status == "in_progress" || item.Status == "satisfied") {
					result.Items = append(result.Items, TodaySummaryItem{item.Title, item.Commitment, item.MinimumAction, item.TargetMinutes})
				}
			}
			if len(result.Items) > 0 {
				result.Status = "arranged"
				result.Message = "任务树已更新；下列是今日实际保留的核心推进，记录今日推进不代表任务最终完成。"
			}
		}
		if goalContext != "" {
			if len(result.Items) > 0 {
				result.Message = goalContext + "下列是今日实际保留的安排，未自动切换排期方向。记录今日推进不代表任务最终完成。"
			} else {
				result.Message = goalContext + "未自动切换排期方向。" + result.Message
			}
		}
		return nil
	})
	if err != nil {
		result.Status, result.Message, result.Items = "unavailable", "变更已生效；今日安排暂时无法读取，请到今日推进查看。", []TodaySummaryItem{}
	}
	return result
}
