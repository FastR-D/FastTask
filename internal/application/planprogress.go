package application

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type planItemEvidence struct {
	LogicalID       string   `json:"logical_id"`
	WorkSessionIDs  []string `json:"work_session_ids,omitempty"`
	ReversesEventID string   `json:"reverses_event_id,omitempty"`
}

type PlanItemCompletionCommand struct {
	PlanID         string
	ItemID         string
	Expected       int
	Type           string
	Summary        string
	WorkSessionIDs []string
	CompleteTask   bool
	ExpectedTask   int
}

func completeTaskFromDailyEligibleTx(tx *gorm.DB, userID string, task persistence.Task) (bool, error) {
	if task.UserID != userID || (task.Type != "action" && task.Type != "task") || strings.TrimSpace(task.SuccessCriteria) == "" {
		return false, nil
	}
	var children int64
	if err := tx.Model(&persistence.Task{}).Where("user_id = ? AND parent_id = ? AND deleted_at IS NULL AND status NOT IN ('cancelled','superseded')", userID, task.ID).Count(&children).Error; err != nil {
		return false, err
	}
	if children > 0 {
		return false, nil
	}
	return taskExecutableTx(tx, userID, task.ID)
}

func logicalPlanItemID(item persistence.DailyPlanItem) string {
	if item.LogicalID != "" {
		return item.LogicalID
	}
	return item.ID
}

// A historical revision is an audit snapshot, never an alternative writable plan.
func actionablePlanItem(tx *gorm.DB, userID string, item persistence.DailyPlanItem) error {
	if item.TaskID != nil {
		if _, err := RequireLiveTaskTx(tx, userID, *item.TaskID); err != nil {
			return err
		}
	}
	var plan persistence.DailyPlan
	if err := tx.Where("id = ? AND user_id = ?", item.PlanID, userID).First(&plan).Error; err != nil {
		return notFound(err)
	}
	if plan.Status != "active" || item.PlanRevision != plan.CurrentRevision {
		return ErrConflict
	}
	var user persistence.User
	if err := tx.First(&user, "id = ?", userID).Error; err != nil {
		return notFound(err)
	}
	loc, _ := resolveZone(user.Timezone)
	if plan.LocalDate > persistence.Now().In(loc).Format("2006-01-02") {
		return ErrConflict
	}
	// Core is a planning priority, not a prerequisite for reading or learning.
	// Real task prerequisites continue to be checked by task eligibility.
	return nil
}

func touchPlanTx(tx *gorm.DB, planID string, now time.Time) error {
	return tx.Model(&persistence.DailyPlan{}).Where("id = ?", planID).
		Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "updated_at": now}).Error
}

func itemSessionsQuery(tx *gorm.DB, userID string, item persistence.DailyPlanItem) *gorm.DB {
	return tx.Table("work_sessions AS ws").Joins("JOIN daily_plan_items AS dpi ON dpi.id = ws.daily_plan_item_id").
		Where("ws.user_id = ? AND dpi.user_id = ? AND dpi.plan_id = ? AND COALESCE(NULLIF(dpi.logical_id, ''), dpi.id) = ?", userID, userID, item.PlanID, logicalPlanItemID(item)).
		Where("ws.status IN ('completed','stopped')")
}

// Only named, valid sessions belonging to this logical daily item count. Duplicate
// references never increase the duration, and an unrelated reference is rejected.
func completionSessions(tx *gorm.DB, userID string, item persistence.DailyPlanItem, ids []string) ([]string, int, error) {
	unique := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, 0, ErrValidation
		}
		unique[id] = true
	}
	if len(unique) == 0 || len(unique) > 1000 {
		return nil, 0, ErrValidation
	}
	ids = make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sessions []persistence.WorkSession
	if err := itemSessionsQuery(tx, userID, item).Where("ws.id IN ?", ids).Select("ws.*").Scan(&sessions).Error; err != nil {
		return nil, 0, err
	}
	if len(sessions) != len(ids) {
		return nil, 0, ErrValidation
	}
	total := 0
	for _, session := range sessions {
		total += session.DurationSeconds
	}
	return ids, total, nil
}

func lastItemCompletion(tx *gorm.DB, userID string, item persistence.DailyPlanItem) (*persistence.ProgressEvent, error) {
	var event persistence.ProgressEvent
	err := tx.Table("progress_events AS pe").Select("pe.*").
		Joins("JOIN daily_plan_items AS dpi ON dpi.id = pe.plan_item_id").
		Where("pe.user_id = ? AND dpi.plan_id = ? AND COALESCE(NULLIF(dpi.logical_id, ''), dpi.id) = ?", userID, item.PlanID, logicalPlanItemID(item)).
		Where("pe.type IN ('result','step','time','minimum_action')").
		Order("pe.created_at DESC, pe.id DESC").First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &event, err
}

func reopenPlanItemTx(tx *gorm.DB, userID string, item persistence.DailyPlanItem, summary string) error {
	event, err := lastItemCompletion(tx, userID, item)
	if err != nil {
		return err
	}
	evidence := planItemEvidence{LogicalID: logicalPlanItemID(item)}
	if event != nil {
		evidence.ReversesEventID = event.ID
	}
	encoded, _ := json.Marshal(evidence)
	now := persistence.Now()
	status := "planned"
	var plan persistence.DailyPlan
	if err := tx.First(&plan, "id = ?", item.PlanID).Error; err != nil {
		return err
	}
	if plan.Status != "active" {
		status = "not_completed"
	} else if item.TaskID != nil {
		executable, err := taskExecutableTx(tx, userID, *item.TaskID)
		// A task may have entered the recycle bin after its focus was recorded.
		// Correcting that evidence remains valid; it must not reactivate the task.
		if err != nil && !errors.Is(err, ErrNotFound) && !persistence.IsNotFound(err) {
			return err
		}
		if !executable {
			status = "skipped"
		}
	}
	result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", item.ID, item.Revision).
		Updates(map[string]any{"status": status, "completion_type": "", "completion_summary": "", "revision": item.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRevision
	}
	reversal := persistence.ProgressEvent{ID: persistence.NewID("progress"), UserID: userID, TaskID: item.TaskID, PlanItemID: &item.ID, Type: "daily_item_reopened", Summary: strings.TrimSpace(summary), EvidenceJSON: string(encoded), OccurredAt: now, CreatedAt: now}
	if err := tx.Create(&reversal).Error; err != nil {
		return err
	}
	if err := touchPlanTx(tx, item.PlanID, now); err != nil {
		return err
	}
	return createOutbox(tx, userID, "daily_item.reopened", map[string]any{"item_id": item.ID, "logical_id": logicalPlanItemID(item)})
}

// ReopenPlanItem corrects a daily commitment; it does not erase work or reopen a
// separately completed underlying Task.
func (a *ProgressService) ReopenPlanItem(ctx context.Context, userID, planID, itemID string, expected int, summary string) (*persistence.DailyPlanItem, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, ErrValidation
	}
	var item persistence.DailyPlanItem
	err := a.Store.WithTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND plan_id = ? AND user_id = ?", itemID, planID, userID).First(&item).Error; err != nil {
			return notFound(err)
		}
		if item.Revision != expected {
			return ErrRevision
		}
		if err := actionablePlanItem(tx, userID, item); err != nil {
			return err
		}
		if item.Status != "satisfied" {
			return ErrConflict
		}
		if err := reopenPlanItemTx(tx, userID, item, summary); err != nil {
			return err
		}
		// Preserve the version produced by this correction even when another
		// caller records new completion before our response is sent.
		return tx.Where("id = ? AND plan_id = ? AND user_id = ?", itemID, planID, userID).First(&item).Error
	})
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *ProgressService) invalidateSessionCompletionTx(tx *gorm.DB, userID string, session persistence.WorkSession) error {
	if session.DailyPlanItemID == nil {
		return nil
	}
	var original persistence.DailyPlanItem
	if err := tx.Where("id = ? AND user_id = ?", *session.DailyPlanItemID, userID).First(&original).Error; err != nil {
		return err
	}
	var current persistence.DailyPlanItem
	err := tx.Table("daily_plan_items AS dpi").Select("dpi.*").Joins("JOIN daily_plans AS dp ON dp.id = dpi.plan_id").
		Where("dpi.user_id = ? AND dpi.plan_id = ? AND COALESCE(NULLIF(dpi.logical_id, ''), dpi.id) = ? AND dpi.plan_revision = dp.current_revision AND dpi.status = 'satisfied' AND dpi.completion_type = 'time'", userID, original.PlanID, logicalPlanItemID(original)).First(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	event, err := lastItemCompletion(tx, userID, current)
	if err != nil {
		return err
	}
	evidence := planItemEvidence{}
	if event != nil {
		// Legacy evidence was [] and had no references. Recheck all sessions only
		// for those old records; new completions always cite their actual evidence.
		_ = json.Unmarshal([]byte(event.EvidenceJSON), &evidence)
	}
	query := itemSessionsQuery(tx, userID, current)
	if len(evidence.WorkSessionIDs) > 0 {
		query = query.Where("ws.id IN ?", evidence.WorkSessionIDs)
	}
	var total int64
	if err := query.Select("COALESCE(SUM(ws.duration_seconds),0)").Scan(&total).Error; err != nil {
		return err
	}
	if total >= int64(current.TargetMinutes*60) {
		return nil
	}
	return reopenPlanItemTx(tx, userID, current, "专注记录已作废，有效时长不足，撤销当日完成")
}
