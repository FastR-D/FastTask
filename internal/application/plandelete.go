package application

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type PlanItemDeletion struct {
	ID       string
	Revision int
}

type PlanItemDeletionResult struct {
	Plan       persistence.DailyPlan       `json:"plan"`
	Items      []persistence.DailyPlanItem `json:"items"`
	DeletedIDs []string                    `json:"deleted_ids"`
}

// DeletePlanItems removes current commitments from today's visible aggregate.
// Original rows, underlying tasks and their evidence remain in revision history;
// nothing is completed, skipped, erased or moved into another user's plan.
func (a *PlanService) DeletePlanItems(ctx context.Context, userID, planID string, expected int, selected []PlanItemDeletion) (*PlanItemDeletionResult, error) {
	if len(selected) == 0 || len(selected) > 3 {
		return nil, ErrValidation
	}
	requested := make(map[string]int, len(selected))
	for _, item := range selected {
		if strings.TrimSpace(item.ID) == "" || item.Revision < 1 {
			return nil, ErrValidation
		}
		if _, duplicate := requested[item.ID]; duplicate {
			return nil, ErrValidation
		}
		requested[item.ID] = item.Revision
	}
	var result PlanItemDeletionResult
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		// Every retry starts from the persisted aggregate, never a previous
		// attempt's partially moved in-memory items.
		result = PlanItemDeletionResult{Items: []persistence.DailyPlanItem{}, DeletedIDs: []string{}}
		var user persistence.User
		if err := tx.Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		var plan persistence.DailyPlan
		if err := tx.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expected {
			return ErrRevision
		}
		now := persistence.Now()
		loc, zone := resolveZone(user.Timezone)
		if plan.Status != "active" || plan.Timezone != zone || plan.LocalDate != now.In(loc).Format("2006-01-02") {
			return ErrConflict
		}
		items, err := livePlanItemsTx(tx, plan)
		if err != nil {
			return err
		}
		removed := []persistence.DailyPlanItem{}
		for _, item := range items {
			revision, deleting := requested[item.ID]
			if !deleting {
				result.Items = append(result.Items, item)
				continue
			}
			if item.UserID != userID || item.Kind != "core" {
				return ErrValidation
			}
			if item.Revision != revision {
				return ErrRevision
			}
			removed = append(removed, item)
			result.DeletedIDs = append(result.DeletedIDs, item.ID)
		}
		if len(removed) != len(requested) {
			return ErrNotFound
		}
		sources, err := scheduleItemSourcesTx(tx, ScheduleDay{Plan: plan, Items: result.Items})
		if err != nil {
			return err
		}
		sort.Strings(result.DeletedIDs)
		previous := plan.CurrentRevision
		plan.CurrentRevision++
		plan.Revision++
		plan.ManualItemsRemoved = true
		plan.UpdatedAt = now
		update := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND user_id = ? AND revision = ?", plan.ID, userID, expected).
			Updates(map[string]any{"current_revision": plan.CurrentRevision, "revision": plan.Revision, "manual_items_removed": true, "updated_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrRevision
		}
		for index := range result.Items {
			item := &result.Items[index]
			update := tx.Model(&persistence.DailyPlanItem{}).
				Where("id = ? AND user_id = ? AND plan_id = ? AND plan_revision = ? AND revision = ?", item.ID, userID, plan.ID, previous, item.Revision).
				UpdateColumn("plan_revision", plan.CurrentRevision)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrRevision
			}
			item.PlanRevision = plan.CurrentRevision
		}
		input, err := json.Marshal(map[string]any{"source": "manual_item_deletion", "base_revision": expected, "deleted_item_ids": result.DeletedIDs, "deleted_items": removed, "item_sources": sources})
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(result.Items)
		if err != nil {
			return err
		}
		revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: plan.CurrentRevision, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(snapshot), Reason: "manual item deletion", CreatedAt: now}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		if err := createOutbox(tx, userID, "daily_plan.items_deleted", map[string]any{"plan_id": plan.ID, "revision": plan.CurrentRevision, "deleted_item_ids": result.DeletedIDs}); err != nil {
			return err
		}
		result.Plan = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
