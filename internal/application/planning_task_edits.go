package application

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

const (
	planItemSourceGenerated = "generated"
	planItemSourceExplicit  = "explicit"
)

// Source follows the physical item through immutable snapshots, independently
// of whether its words happen to match the task. Scheduler copies record this
// optional marker before moving/cloning an item. Missing legacy provenance is
// conservative; it is not permission to replace a possibly approved promise.
func planItemScopeSourceTx(tx *gorm.DB, item persistence.DailyPlanItem) (string, error) {
	var history []persistence.DailyPlanRevision
	if err := tx.Where("plan_id = ? AND revision <= ?", item.PlanID, item.PlanRevision).Order("revision DESC").Find(&history).Error; err != nil {
		return "", err
	}
	for _, revision := range history {
		var snapshot []persistence.DailyPlanItem
		if json.Unmarshal([]byte(revision.PlanSnapshot), &snapshot) != nil {
			return planItemSourceExplicit, nil
		}
		found := false
		for _, recorded := range snapshot {
			if recorded.ID == item.ID {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		var input struct {
			Source      string            `json:"source"`
			ItemSources map[string]string `json:"item_sources"`
		}
		if json.Unmarshal([]byte(revision.InputSnapshot), &input) != nil {
			return planItemSourceExplicit, nil
		}
		if source, exists := input.ItemSources[item.ID]; exists {
			if source == planItemSourceGenerated {
				return source, nil
			}
			return planItemSourceExplicit, nil
		}
		if input.Source == "agent_proposal" {
			return planItemSourceExplicit, nil
		}
		if revision.Reason == "initial generation" || revision.Reason == "same-day replanning" {
			return planItemSourceGenerated, nil
		}
		// An append can include an older row without creating it. Keep looking
		// for that row's original generation or explicit agent proposal.
	}
	return planItemSourceExplicit, nil
}

func scheduleItemSourcesTx(tx *gorm.DB, day ScheduleDay) (map[string]string, error) {
	sources := make(map[string]string, len(day.Items))
	for _, item := range day.Items {
		if source, supplied := day.itemSources[item.ID]; supplied {
			sources[item.ID] = source
			continue
		}
		source, err := planItemScopeSourceTx(tx, item)
		if err != nil {
			return nil, err
		}
		sources[item.ID] = source
	}
	return sources, nil
}

// This projection runs only for an explicit task edit, never an ordinary GET or
// a deployment. Unstarted daily fields that still follow the previous source
// can follow its edit; independently worded promises and evidence stay intact.
func syncEditedTaskTodayItemsTx(tx *gorm.DB, userID string, before, after persistence.Task, now time.Time) error {
	if before.Title == after.Title && before.MinimumAction == after.MinimumAction && before.SuccessCriteria == after.SuccessCriteria && before.EstimateMinutes == after.EstimateMinutes {
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
		items, err := livePlanItemsTx(tx, plan)
		if err != nil {
			return err
		}
		protected, err := protectedPlanItemsTx(tx, userID, plan, items)
		if err != nil {
			return err
		}
		changed := false
		for _, item := range items {
			if item.TaskID == nil || *item.TaskID != after.ID || item.Status != "planned" || protected[item.ID] {
				continue
			}
			updates := map[string]any{}
			if item.Title == before.Title && item.Title != after.Title {
				updates["title"] = after.Title
			}
			source, err := planItemScopeSourceTx(tx, item)
			if err != nil {
				return err
			}
			if source == planItemSourceGenerated && item.MinimumAction == before.MinimumAction {
				if item.MinimumAction != after.MinimumAction {
					updates["minimum_action"] = after.MinimumAction
				}
				// Recognize the legacy full-criteria wording without converting
				// untouched legacy records during a software update.
				if item.Commitment == before.MinimumAction || item.Commitment == strings.TrimSpace(before.SuccessCriteria) || item.Commitment == "推进："+before.SuccessCriteria {
					if commitment := dailyTaskCommitment(after, item.TargetMinutes); commitment != item.Commitment {
						updates["commitment"] = commitment
					}
				}
			}
			if len(updates) == 0 {
				continue
			}
			updates["revision"], updates["updated_at"] = item.Revision+1, now
			result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND user_id = ? AND revision = ? AND status = 'planned'", item.ID, userID, item.Revision).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRevision
			}
			changed = true
		}
		if changed {
			if err := touchPlanTx(tx, plan.ID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// A task that was explicitly split or converted to a milestone no longer owns
// an unstarted leaf commitment. Keep its original row in revision history, and
// let the real leaves use the vacancy. Evidenced promises keep their exact scope.
func refreshTodayTaskStructureTx(tx *gorm.DB, userID string, items []persistence.DailyPlanItem, protected map[string]bool, byID map[string]persistence.Task, now time.Time) ([]persistence.DailyPlanItem, map[string]bool, bool, error) {
	parents := map[string]bool{}
	for _, task := range byID {
		if task.ParentID != nil && task.Status != "superseded" && task.Status != "cancelled" {
			parents[*task.ParentID] = true
		}
	}
	kept := make([]persistence.DailyPlanItem, 0, len(items))
	protectedParents := map[string]bool{}
	for _, item := range items {
		if item.Kind == "core" && item.TaskID != nil && parents[*item.TaskID] && protected[item.ID] && (item.Status == "planned" || item.Status == "in_progress" || item.Status == "satisfied") {
			protectedParents[*item.TaskID] = true
		}
	}
	changed := false
	for _, item := range items {
		if item.Kind != "core" || item.TaskID == nil || (item.Status != "planned" && item.Status != "in_progress" && item.Status != "satisfied") {
			kept = append(kept, item)
			continue
		}
		task, found := byID[*item.TaskID]
		if !found || ((task.Type == "task" || task.Type == "action") && !parents[task.ID] && !taskDescendsFrom(task, protectedParents, byID)) {
			kept = append(kept, item)
			continue
		}
		if protected[item.ID] {
			kept = append(kept, item)
			continue
		}
		if found && (task.Type == "task" || task.Type == "action") && !parents[task.ID] && taskDescendsFrom(task, protectedParents, byID) {
			approved, err := approvedPlanItemParentScopeTx(tx, userID, item, task, byID)
			if err != nil {
				return nil, nil, false, err
			}
			if approved && executableTaskEligibility(task, byID) {
				kept = append(kept, item)
				continue
			}
		}
		result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND user_id = ? AND revision = ? AND status = 'planned'", item.ID, userID, item.Revision).
			Updates(map[string]any{"status": "superseded", "revision": item.Revision + 1, "updated_at": now})
		if result.Error != nil {
			return nil, nil, false, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, nil, false, ErrRevision
		}
		changed = true
	}
	return kept, protectedParents, changed, nil
}

func taskDescendsFrom(task persistence.Task, protectedParents map[string]bool, byID map[string]persistence.Task) bool {
	seen := map[string]bool{}
	for task.ParentID != nil && !seen[task.ID] {
		seen[task.ID] = true
		if protectedParents[*task.ParentID] {
			return true
		}
		var found bool
		task, found = byID[*task.ParentID]
		if !found {
			break
		}
	}
	return false
}
