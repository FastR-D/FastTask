package application

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

const (
	defaultDailyBudget = 120
	scheduleDays       = 7
)

// ScheduleDay is a projection of the existing plan aggregate, not another task
// queue. Future items become the same daily commitments when their day arrives.
type ScheduleDay struct {
	Plan         persistence.DailyPlan       `json:"plan"`
	Items        []persistence.DailyPlanItem `json:"items"`
	Availability *PlanningAvailability       `json:"availability,omitempty"`
	reason       string
	itemSources  map[string]string
}

func planningSettingsTx(tx *gorm.DB, userID string) (persistence.PlanningSettings, error) {
	settings := persistence.PlanningSettings{UserID: userID, DailyBudgetMinutes: defaultDailyBudget}
	err := tx.Where("user_id = ?", userID).First(&settings).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return settings, nil
	}
	return settings, err
}

func primaryGoalTx(tx *gorm.DB, userID string, settings persistence.PlanningSettings) (string, error) {
	var goal persistence.Goal
	if settings.PrimaryGoalID != nil {
		err := tx.Where("id = ? AND user_id = ? AND status = 'active' AND deleted_at IS NULL", *settings.PrimaryGoalID, userID).First(&goal).Error
		if err == nil {
			return goal.ID, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", err
		}
	}
	err := tx.Where("user_id = ? AND status = 'active' AND deleted_at IS NULL", userID).Order("created_at, id").First(&goal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return goal.ID, err
}

func resolvePlanningSettingsProjectionTx(tx *gorm.DB, userID string, settings persistence.PlanningSettings) (persistence.PlanningSettings, error) {
	// Preserve the saved choice separately from its effective fallback. A client
	// saving only a budget must not accidentally pin an automatically chosen goal.
	settings.ConfiguredPrimaryGoalID = settings.PrimaryGoalID
	settings.PrimaryGoalMode = "automatic"
	if settings.PrimaryGoalID != nil {
		settings.PrimaryGoalMode = "fixed"
	}
	goalID, err := primaryGoalTx(tx, userID, settings)
	if err != nil {
		return settings, err
	}
	if goalID != "" {
		settings.PrimaryGoalID = &goalID
	} else {
		settings.PrimaryGoalID = nil
	}
	return settings, nil
}

func (a *PlanService) GetPlanningSettings(ctx context.Context, userID string) (*persistence.PlanningSettings, error) {
	tx := a.Store.DB.WithContext(ctx)
	var user persistence.User
	if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, notFound(err)
	}
	settings, err := planningSettingsTx(tx, userID)
	if err != nil {
		return nil, err
	}
	settings, err = resolvePlanningSettingsProjectionTx(tx, userID, settings)
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

func (a *PlanService) UpdatePlanningSettings(ctx context.Context, userID string, expected int, primaryGoalID *string, budget int) (*persistence.PlanningSettings, error) {
	if budget < 30 || budget > 480 {
		return nil, ErrValidation
	}
	var settings persistence.PlanningSettings
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		current, err := planningSettingsTx(tx, userID)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrRevision
		}
		configuredGoalID := primaryGoalID
		if configuredGoalID != nil {
			id := strings.TrimSpace(*configuredGoalID)
			if id == "" {
				configuredGoalID = nil
			} else {
				// Keeping a previously fixed, now unavailable goal is different
				// from selecting one anew. Budget changes preserve user intent.
				if current.PrimaryGoalID == nil || *current.PrimaryGoalID != id {
					var goal persistence.Goal
					if err := tx.Where("id = ? AND user_id = ? AND status = 'active' AND deleted_at IS NULL", id, userID).First(&goal).Error; err != nil {
						return notFound(err)
					}
				}
				configuredGoalID = &id
			}
		}
		now := persistence.Now()
		// Rebuild the saved choice and effective projection for every attempt;
		// a rolled-back fallback must not become the next attempt's saved choice.
		settings = persistence.PlanningSettings{UserID: userID, PrimaryGoalID: configuredGoalID, DailyBudgetMinutes: budget, Revision: expected + 1, CreatedAt: current.CreatedAt, UpdatedAt: now}
		if expected == 0 {
			settings.CreatedAt = now
			if err := tx.Create(&settings).Error; err != nil {
				return err
			}
		} else {
			result := tx.Model(&persistence.PlanningSettings{}).Where("user_id = ? AND revision = ?", userID, expected).Updates(map[string]any{"primary_goal_id": configuredGoalID, "daily_budget_minutes": budget, "revision": expected + 1, "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRevision
			}
		}
		settings, err = resolvePlanningSettingsProjectionTx(tx, userID, settings)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

func planningSlots(budget int) int {
	// The daily budget sizes the steps; it does not impose a thirty-minute
	// admission fee on each real action. Even a short day can have three small
	// outputs. Existing commitments still consume their promised minutes below.
	return 3
}

// Zero means the user's saved budget. Explicit budgets must use the same bounds
// as planning settings, whichever entry point creates the commitment.
func resolvePlanningBudget(available int, settings persistence.PlanningSettings) (int, error) {
	if available == 0 {
		available = settings.DailyBudgetMinutes
	}
	if available < 30 || available > 480 {
		return 0, ErrValidation
	}
	return available, nil
}

func planningTargetMinutes(budget int) int {
	if budget <= 0 {
		budget = defaultDailyBudget
	}
	if slots := planningSlots(budget); slots > 0 {
		return budget / slots
	}
	return 30
}

// eligiblePlanningTasksTx chooses executable leaves under the primary research
// goal. A parent is navigation and context; its live children do the work. It is
// never scheduled next to those children as another independent commitment.
func eligiblePlanningTasksTx(tx *gorm.DB, userID string, taskIDs []string) ([]persistence.Task, persistence.PlanningSettings, error) {
	settings, err := planningSettingsTx(tx, userID)
	if err != nil {
		return nil, settings, err
	}
	goalID, err := primaryGoalTx(tx, userID, settings)
	if err != nil {
		return nil, settings, err
	}
	if goalID == "" {
		settings.PrimaryGoalID = nil
		return nil, settings, nil
	}
	settings.PrimaryGoalID = &goalID
	var tasks []persistence.Task
	query := tx.Model(&persistence.Task{}).
		Where("user_id = ? AND goal_id = ? AND deleted_at IS NULL AND type IN ('task','action') AND status IN ('ready','in_progress') AND TRIM(minimum_action) != ''", userID, goalID).
		Where("NOT EXISTS (SELECT 1 FROM tasks AS child WHERE child.parent_id = tasks.id AND child.deleted_at IS NULL AND child.status NOT IN ('superseded','cancelled'))")
	if len(taskIDs) > 0 {
		query = query.Where("id IN ?", taskIDs)
	}
	err = query.Order("priority DESC, position, id").Find(&tasks).Error
	if err != nil {
		return nil, settings, err
	}
	var allTasks []persistence.Task
	if err := tx.Where("user_id = ? AND goal_id = ?", userID, goalID).Find(&allTasks).Error; err != nil {
		return nil, settings, err
	}
	byID := map[string]persistence.Task{}
	for _, task := range allTasks {
		byID[task.ID] = task
	}
	eligible := make([]persistence.Task, 0, len(tasks))
	for _, task := range tasks {
		if executableTaskEligibility(task, byID) {
			eligible = append(eligible, task)
		}
	}
	return eligible, settings, nil
}

func candidatesForTasks(tasks []persistence.Task) []domain.Candidate {
	candidates := make([]domain.Candidate, 0, len(tasks))
	for _, task := range tasks {
		candidates = append(candidates, domain.Candidate{ID: task.ID, Status: task.Status, Priority: task.Priority, Position: task.Position, Estimate: task.EstimateMinutes, MinimumAction: task.MinimumAction, GoalActive: true, DependencyDone: true})
	}
	return candidates
}

func newScheduledItem(userID, planID string, revision, position, budget int, task persistence.Task, now time.Time) persistence.DailyPlanItem {
	id, taskID := persistence.NewID("dpi"), task.ID
	target := planningTargetMinutes(budget)
	return persistence.DailyPlanItem{ID: id, LogicalID: id, UserID: userID, PlanID: planID, PlanRevision: revision, TaskID: &taskID, Kind: "core", Title: task.Title, Commitment: dailyTaskCommitment(task, target), MinimumAction: task.MinimumAction, AllowedTypes: "result,step,time,minimum_action", TargetMinutes: target, Status: "planned", Position: position, Revision: 1, CreatedAt: now, UpdatedAt: now}
}

// A genuinely small leaf can promise its concrete local result. A longer or
// unestimated task only promises an advancement slice; its final criterion must
// not silently become a forty-minute promise to finish an entire paper.
func dailyTaskCommitment(task persistence.Task, targetMinutes int) string {
	if task.EstimateMinutes > 0 && task.EstimateMinutes <= targetMinutes && strings.TrimSpace(task.SuccessCriteria) != "" {
		return strings.TrimSpace(task.SuccessCriteria)
	}
	return task.MinimumAction
}

// Future reservations follow edits to their source task. Revision history
// distinguishes generated commitments from explicitly added custom items and
// retains the latest approved first step and reason across schedule refreshes.
// Today's frozen commitment is never passed to this function.
func refreshReservedTaskContentTx(tx *gorm.DB, plan persistence.DailyPlan, item persistence.DailyPlanItem, task persistence.Task) (persistence.DailyPlanItem, bool, error) {
	// Keep existing approved/legacy reservations until their source scope changes
	// or the user explicitly replans. A software update is not authorization to
	// rewrite the user's already visible future commitments.
	if item.Title == task.Title && item.MinimumAction == task.MinimumAction && item.Commitment == "推进："+task.SuccessCriteria {
		return item, false, nil
	}
	if item.Title == task.Title && item.MinimumAction == task.MinimumAction && item.Commitment == task.MinimumAction {
		return item, false, nil
	}
	if item.Title == task.Title && item.MinimumAction == task.MinimumAction && item.Commitment == dailyTaskCommitment(task, item.TargetMinutes) {
		return item, false, nil
	}
	var history []persistence.DailyPlanItem
	if err := tx.Where("plan_id = ? AND task_id = ?", plan.ID, task.ID).Order("plan_revision DESC, created_at DESC, id DESC").Find(&history).Error; err != nil {
		return item, false, err
	}
	var original persistence.DailyPlanItem
	var revision persistence.DailyPlanRevision
	for _, previous := range history {
		revision = persistence.DailyPlanRevision{}
		if err := tx.Where("plan_id = ? AND revision = ?", plan.ID, previous.PlanRevision).First(&revision).Error; err != nil {
			return item, false, err
		}
		var snapshot []persistence.DailyPlanItem
		if err := json.Unmarshal([]byte(revision.PlanSnapshot), &snapshot); err != nil {
			return item, false, err
		}
		generated := false
		for _, recorded := range snapshot {
			if recorded.ID == previous.ID {
				generated = true
				break
			}
		}
		if !generated {
			return item, false, nil
		}
		original = previous
		// Schedule refreshes copy commitments. The most recent explicit
		// generation/replan owns their approved content, even when it reused a
		// logical ID that originally came from automatic scheduling.
		if revision.Reason != "seven-day scheduling" {
			break
		}
	}
	if original.ID == "" {
		return item, false, nil
	}
	var input struct {
		Source     string          `json:"source"`
		Candidates []PlanCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(revision.InputSnapshot), &input); err != nil {
		return item, false, err
	}
	updated := item
	updated.Title = task.Title
	updated.MinimumAction = task.MinimumAction
	updated.Commitment = dailyTaskCommitment(task, item.TargetMinutes)
	if input.Source == "agent_proposal" {
		updated.MinimumAction = original.MinimumAction
		updated.Commitment = original.MinimumAction
		for _, candidate := range input.Candidates {
			if candidate.TaskID == task.ID && strings.TrimSpace(candidate.Commitment) != "" {
				updated.Commitment = strings.TrimSpace(candidate.Commitment)
				break
			}
		}
	}
	changed := updated.Title != item.Title || updated.MinimumAction != item.MinimumAction || updated.Commitment != item.Commitment
	return updated, changed, nil
}

func logicalItemID(item persistence.DailyPlanItem) string {
	if item.LogicalID != "" {
		return item.LogicalID
	}
	return item.ID
}

func historicalLogicalItemIDTx(tx *gorm.DB, userID, planID, taskID string) (string, error) {
	var original persistence.DailyPlanItem
	err := tx.Select("id, logical_id").Where("user_id = ? AND plan_id = ? AND task_id = ?", userID, planID, taskID).Order("plan_revision, created_at, id").First(&original).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return logicalItemID(original), nil
}

func freezePlanWithCore(plan *persistence.DailyPlan, now time.Time, hasCore bool) {
	if !hasCore || plan.FrozenAt != nil {
		return
	}
	loc, err := time.LoadLocation(plan.Timezone)
	if err == nil && plan.LocalDate <= now.In(loc).Format("2006-01-02") {
		plan.FrozenAt = &now
	}
}

func hasCoreItem(items []persistence.DailyPlanItem) bool {
	for _, item := range items {
		if item.Kind == "core" {
			return true
		}
	}
	return false
}

func livePlanItemsTx(tx *gorm.DB, plan persistence.DailyPlan) ([]persistence.DailyPlanItem, error) {
	items := []persistence.DailyPlanItem{}
	err := tx.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("kind, position, id").Find(&items).Error
	return items, err
}

// EnsureDailyPlan is safe to replay after a restart or concurrent request. An
// existing commitments are preserved, while open slots can receive new work.
// Creating a day also fills the remaining six-day forecast.
func (a *PlanService) EnsureDailyPlan(ctx context.Context, userID, localDate, timezone string) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	var result persistence.DailyPlan
	var items []persistence.DailyPlanItem
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		loc, zone := resolveZone(timezone, user.Timezone)
		if localDate == "" {
			localDate = persistence.Now().In(loc).Format("2006-01-02")
		}
		if err := ParseDateInZone(localDate, zone); err != nil {
			return err
		}
		err := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, localDate, zone).First(&result).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		days, err := a.refreshScheduleTx(tx, user, localDate, loc, zone, persistence.Now())
		if err != nil {
			return err
		}
		result, items = days[0].Plan, days[0].Items
		return nil
	})
	return &result, items, err
}

// RefreshSchedule preserves today's commitments, fills its vacancies, then
// queues additional work in future holes. Existing future order is retained;
// an unstarted reservation can move forward into a vacant slot today. Actual
// progress history only (never a future reservation) decides the next rotation.
func (a *PlanService) RefreshSchedule(ctx context.Context, userID string, now time.Time) ([]ScheduleDay, error) {
	if now.IsZero() {
		now = persistence.Now()
	}
	var days []ScheduleDay
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		loc, zone := resolveZone(user.Timezone)
		var err error
		days, err = a.refreshScheduleTx(tx, user, now.In(loc).Format("2006-01-02"), loc, zone, now)
		return err
	})
	return days, err
}

func (a *PlanService) MaintainSchedules(ctx context.Context, now time.Time) error {
	var users []persistence.User
	if err := a.Store.DB.WithContext(ctx).Where("status = 'active' AND (EXISTS (SELECT 1 FROM goals WHERE goals.user_id = users.id AND goals.status = 'active' AND goals.deleted_at IS NULL) OR EXISTS (SELECT 1 FROM daily_plans WHERE daily_plans.user_id = users.id AND daily_plans.status = 'active'))").Find(&users).Error; err != nil {
		return err
	}
	for _, user := range users {
		if _, err := a.RefreshSchedule(ctx, user.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func (a *PlanService) refreshScheduleTx(tx *gorm.DB, user persistence.User, date string, loc *time.Location, zone string, now time.Time) ([]ScheduleDay, error) {
	start, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return nil, ErrValidation
	}
	tasks, settings, err := eligiblePlanningTasksTx(tx, user.ID, nil)
	if err != nil {
		return nil, err
	}
	byTask := make(map[string]persistence.Task, len(tasks))
	for _, task := range tasks {
		byTask[task.ID] = task
	}
	// Finishing an old day records the miss; it never completes its underlying
	// task and never copies a blocked task into tomorrow.
	var past []persistence.DailyPlan
	if err := tx.Where("user_id = ? AND timezone = ? AND local_date < ? AND status = 'active'", user.ID, zone, date).Find(&past).Error; err != nil {
		return nil, err
	}
	for _, plan := range past {
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND status IN ('planned','in_progress')", plan.ID, plan.CurrentRevision).Updates(map[string]any{"status": "not_completed", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return nil, err
		}
		if err := tx.Model(&persistence.DailyPlan{}).Where("id = ?", plan.ID).Updates(map[string]any{"status": "closed", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return nil, err
		}
	}
	end := start.AddDate(0, 0, scheduleDays-1).Format("2006-01-02")
	var plans []persistence.DailyPlan
	if err := tx.Where("user_id = ? AND timezone = ? AND local_date >= ? AND local_date <= ?", user.ID, zone, date, end).Order("local_date").Find(&plans).Error; err != nil {
		return nil, err
	}
	byDate := map[string]persistence.DailyPlan{}
	for _, plan := range plans {
		byDate[plan.LocalDate] = plan
	}
	days := make([]ScheduleDay, scheduleDays)
	changed := make([]bool, scheduleDays)
	protectedDays := make([]bool, scheduleDays)
	appendOnlyDays := make([]bool, scheduleDays)
	protectedTodayParents := map[string]bool{}
	var allTasks []persistence.Task
	if err := tx.Where("user_id = ? AND deleted_at IS NULL", user.ID).Find(&allTasks).Error; err != nil {
		return nil, err
	}
	allByID := map[string]persistence.Task{}
	for _, task := range allTasks {
		allByID[task.ID] = task
	}
	assigned := map[string]bool{}
	slots := planningSlots(settings.DailyBudgetMinutes)
	for day := range days {
		localDate := start.AddDate(0, 0, day).Format("2006-01-02")
		plan, found := byDate[localDate]
		if !found {
			plan = persistence.DailyPlan{ID: persistence.NewID("plan"), UserID: user.ID, LocalDate: localDate, Timezone: zone, Status: "active", AlgorithmVersion: "deterministic-v3", CurrentRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
			changed[day] = true
		}
		existing := []persistence.DailyPlanItem{}
		reason := ""
		if found {
			existing, err = livePlanItemsTx(tx, plan)
			if err != nil {
				return nil, err
			}
			protected, err := protectedPlanItemsTx(tx, user.ID, plan, existing)
			if err != nil {
				return nil, err
			}
			protectedDays[day] = len(protected) > 0
			if day == 0 && plan.Status == "active" && localDate == now.In(loc).Format("2006-01-02") {
				var structuralChange bool
				existing, protectedTodayParents, structuralChange, err = refreshTodayTaskStructureTx(tx, user.ID, existing, protected, allByID, now)
				if err != nil {
					return nil, err
				}
				if structuralChange {
					changed[day], reason = true, "task structure updated"
				}
			}
		}
		// A deliberately cancelled or closed future day is not a reservation.
		// Its history remains visible, but it cannot hide tasks from the queue.
		if plan.Status != "active" {
			days[day] = ScheduleDay{Plan: plan, Items: existing}
			continue
		}
		keep := []persistence.DailyPlanItem{}
		core := 0
		frozen := (day == 0 && plan.FrozenAt != nil) || protectedDays[day]
		appendOnlyDays[day] = frozen
		for _, item := range existing {
			if item.Kind != "core" || frozen {
				keep = append(keep, item)
				if item.Kind == "core" && item.TaskID != nil && item.Status != "skipped" {
					assigned[*item.TaskID] = true
				}
				continue
			}
			if item.TaskID == nil {
				changed[day] = true
				continue
			}
			task, eligible := byTask[*item.TaskID]
			if !eligible || assigned[*item.TaskID] || core >= slots {
				changed[day] = true
				continue
			}
			keep = append(keep, item)
			if day > 0 {
				// Resolve the new slice before deriving its output. Otherwise a
				// reduced budget first retains yesterday's larger promise, and the
				// next identical refresh changes it again with another revision.
				if item.TargetMinutes != planningTargetMinutes(settings.DailyBudgetMinutes) {
					item.TargetMinutes = planningTargetMinutes(settings.DailyBudgetMinutes)
					changed[day] = true
				}
				updated, contentChanged, err := refreshReservedTaskContentTx(tx, plan, item, task)
				if err != nil {
					return nil, err
				}
				keep[len(keep)-1] = updated
				changed[day] = changed[day] || contentChanged
			}
			assigned[*item.TaskID] = true
			core++
		}
		days[day] = ScheduleDay{Plan: plan, Items: keep, reason: reason}
	}
	// Evidence is written when work actually happens. Future rows have no say
	// in this ordering, so forecasting cannot manufacture attempts or repeats.
	var progress []persistence.ProgressEvent
	if err := tx.Where("user_id = ? AND occurred_at <= ? AND type IN ('result','step','time','minimum_action')", user.ID, now).
		Where("NOT EXISTS (SELECT 1 FROM progress_events reversal WHERE reversal.user_id = progress_events.user_id AND reversal.type = 'daily_item_reopened' AND json_valid(reversal.evidence_json) AND json_extract(reversal.evidence_json, '$.reverses_event_id') = progress_events.id)").
		Order("occurred_at").Find(&progress).Error; err != nil {
		return nil, err
	}
	lastAttempt := map[string]time.Time{}
	for _, event := range progress {
		if event.TaskID != nil {
			lastAttempt[*event.TaskID] = event.OccurredAt
		}
	}
	var sessions []persistence.WorkSession
	if err := tx.Where("user_id = ? AND started_at <= ? AND status != 'invalidated'", user.ID, now).Find(&sessions).Error; err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if session.StartedAt.After(lastAttempt[session.TaskID]) {
			lastAttempt[session.TaskID] = session.StartedAt
		}
	}
	queue := make([]persistence.Task, 0, len(tasks))
	for _, task := range tasks {
		if !assigned[task.ID] {
			queue = append(queue, task)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool {
		iAt, jAt := lastAttempt[queue[i].ID], lastAttempt[queue[j].ID]
		if !iAt.Equal(jAt) {
			return iAt.Before(jAt)
		}
		if queue[i].Priority != queue[j].Priority {
			return queue[i].Priority > queue[j].Priority
		}
		if queue[i].Position != queue[j].Position {
			return queue[i].Position < queue[j].Position
		}
		return queue[i].ID < queue[j].ID
	})
	for day := range days {
		plan := &days[day].Plan
		if plan.Status != "active" {
			continue
		}
		// An explicit deletion leaves today's vacancies empty until the user
		// explicitly replans. A later GET or background schedule refresh must
		// not immediately undo the deletion by putting tasks back into them.
		if day == 0 && plan.ManualItemsRemoved {
			continue
		}
		core, activeCore, nextPosition := 0, 0, 1
		for _, item := range days[day].Items {
			if item.Kind == "core" {
				core++
				if item.Status != "skipped" && item.Status != "not_completed" {
					activeCore++
				}
				if item.Position >= nextPosition {
					nextPosition = item.Position + 1
				}
			}
		}
		// A deliberately withdrawn day needs an explicit replan. Appending to an
		// otherwise active day preserves every existing row, even after work starts.
		if appendOnlyDays[day] && core > 0 && activeCore == 0 {
			continue
		}
		remainingSlots, targetMinutes := slots-core, planningTargetMinutes(settings.DailyBudgetMinutes)
		if appendOnlyDays[day] {
			_, remainingSlots, targetMinutes = remainingPlanBudget(days[day].Items, settings.DailyBudgetMinutes)
		}
		for remainingSlots > 0 {
			var item persistence.DailyPlanItem
			itemSource := planItemSourceGenerated
			queueIndex := 0
			if day == 0 {
				for queueIndex < len(queue) && taskDescendsFrom(queue[queueIndex], protectedTodayParents, allByID) {
					queueIndex++
				}
			}
			if queueIndex < len(queue) {
				task := queue[queueIndex]
				queue = append(queue[:queueIndex], queue[queueIndex+1:]...)
				item = newScheduledItem(user.ID, plan.ID, plan.CurrentRevision, nextPosition, settings.DailyBudgetMinutes, task, now)
				item.Commitment = dailyTaskCommitment(task, targetMinutes)
			} else if day == 0 {
				// Work already reserved for a later day may fill today's vacancy.
				// Remove that reservation in this transaction so the task stays unique.
				found := false
				for future := 1; future < len(days) && !found; future++ {
					if days[future].Plan.Status != "active" || protectedDays[future] {
						continue
					}
					for index, reserved := range days[future].Items {
						if reserved.Kind != "core" || reserved.TaskID == nil || reserved.Status != "planned" {
							continue
						}
						if taskDescendsFrom(allByID[*reserved.TaskID], protectedTodayParents, allByID) {
							continue
						}
						itemSource, err = planItemScopeSourceTx(tx, reserved)
						if err != nil {
							return nil, err
						}
						// Derive an automatic promise against today's actual remaining
						// slice while the reservation still has its original provenance.
						// Explicit agent/manual promises retain their approved wording.
						reserved.TargetMinutes = targetMinutes
						promoted, _, err := refreshReservedTaskContentTx(tx, days[future].Plan, reserved, allByID[*reserved.TaskID])
						if err != nil {
							return nil, err
						}
						item = promoted
						item.ID = persistence.NewID("dpi")
						item.LogicalID, item.PlanID, item.PlanRevision = item.ID, plan.ID, plan.CurrentRevision
						item.Position, item.Revision, item.CreatedAt, item.UpdatedAt = nextPosition, 1, now, now
						days[future].Items = append(days[future].Items[:index], days[future].Items[index+1:]...)
						changed[future], found = true, true
						break
					}
				}
				if !found {
					break
				}
			} else {
				break
			}
			item.TargetMinutes = targetMinutes
			days[day].Items = append(days[day].Items, item)
			if days[day].itemSources == nil {
				days[day].itemSources = map[string]string{}
			}
			days[day].itemSources[item.ID] = itemSource
			changed[day] = true
			core++
			remainingSlots--
			nextPosition++
		}
		if day == 0 && plan.FrozenAt == nil && core > 0 {
			plan.FrozenAt = &now
			changed[day] = true
		}
	}
	for day := range days {
		if !changed[day] {
			continue
		}
		_, exists := byDate[days[day].Plan.LocalDate]
		if exists && appendOnlyDays[day] {
			if err := a.appendScheduleDayTx(tx, &days[day], settings, now); err != nil {
				return nil, err
			}
		} else if err := a.saveScheduleDayTx(tx, &days[day], exists, settings, now); err != nil {
			return nil, err
		}
	}
	for i := range days {
		availability, err := planningAvailabilityTx(tx, user.ID, days[i].Plan, days[i].Items)
		if err != nil {
			return nil, err
		}
		days[i].Availability = availability
		days[i].itemSources = nil
	}
	return days, nil
}

// Appending moves unchanged rows into the new aggregate snapshot without
// changing their IDs, item revisions or evidence links. Immutable revision
// snapshots retain the previous plan; completion retries keep their identity.
func (a *PlanService) appendScheduleDayTx(tx *gorm.DB, day *ScheduleDay, settings persistence.PlanningSettings, now time.Time) error {
	sources, err := scheduleItemSourcesTx(tx, *day)
	if err != nil {
		return err
	}
	plan := &day.Plan
	previous := plan.CurrentRevision
	plan.CurrentRevision++
	plan.Revision++
	plan.UpdatedAt = now
	update := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, plan.Revision-1).Updates(map[string]any{"current_revision": plan.CurrentRevision, "revision": plan.Revision, "updated_at": now, "frozen_at": plan.FrozenAt})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrRevision
	}
	for i := range day.Items {
		item := &day.Items[i]
		var count int64
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND plan_id = ? AND plan_revision = ?", item.ID, plan.ID, previous).Count(&count).Error; err != nil {
			return err
		}
		item.PlanRevision = plan.CurrentRevision
		if count == 1 {
			update := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", item.ID, item.Revision).UpdateColumn("plan_revision", item.PlanRevision)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrRevision
			}
		} else if err := tx.Create(item).Error; err != nil {
			return err
		}
	}
	input, _ := json.Marshal(map[string]any{"algorithm": "deterministic-v3", "primary_goal_id": settings.PrimaryGoalID, "daily_budget_minutes": settings.DailyBudgetMinutes, "local_date": plan.LocalDate, "append_only": true, "item_sources": sources})
	snapshot, _ := json.Marshal(day.Items)
	reason := day.reason
	if reason == "" {
		reason = "fill daily vacancies"
	}
	revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: plan.CurrentRevision, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(snapshot), Reason: reason, CreatedAt: now}
	if err := tx.Create(&revision).Error; err != nil {
		return err
	}
	return createOutbox(tx, plan.UserID, "daily_plan.scheduled", map[string]any{"plan_id": plan.ID, "local_date": plan.LocalDate})
}

func (a *PlanService) saveScheduleDayTx(tx *gorm.DB, day *ScheduleDay, exists bool, settings persistence.PlanningSettings, now time.Time) error {
	sources, err := scheduleItemSourcesTx(tx, *day)
	if err != nil {
		return err
	}
	plan := &day.Plan
	if exists {
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND status != 'satisfied' AND status != 'superseded'", plan.ID, plan.CurrentRevision).Updates(map[string]any{"status": "superseded", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		plan.CurrentRevision++
		plan.Revision++
		plan.UpdatedAt = now
		if err := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, plan.Revision-1).Updates(map[string]any{"current_revision": plan.CurrentRevision, "revision": plan.Revision, "updated_at": now, "frozen_at": plan.FrozenAt}).Error; err != nil {
			return err
		}
	} else if err := tx.Create(plan).Error; err != nil {
		return err
	}
	clonedSources := make(map[string]string, len(day.Items))
	for i := range day.Items {
		item := &day.Items[i]
		source := sources[item.ID]
		item.LogicalID = logicalItemID(*item)
		if item.TaskID != nil {
			logicalID, err := historicalLogicalItemIDTx(tx, plan.UserID, plan.ID, *item.TaskID)
			if err != nil {
				return err
			}
			if logicalID != "" {
				item.LogicalID = logicalID
			}
		}
		if exists {
			item.ID = persistence.NewID("dpi")
		}
		item.PlanRevision, item.Revision, item.CreatedAt, item.UpdatedAt = plan.CurrentRevision, 1, now, now
		clonedSources[item.ID] = source
	}
	if len(day.Items) > 0 {
		if err := tx.Create(&day.Items).Error; err != nil {
			return err
		}
	}
	input, _ := json.Marshal(map[string]any{"algorithm": "deterministic-v3", "primary_goal_id": settings.PrimaryGoalID, "daily_budget_minutes": settings.DailyBudgetMinutes, "local_date": plan.LocalDate, "item_sources": clonedSources})
	snapshot, _ := json.Marshal(day.Items)
	reason := day.reason
	if reason == "" {
		reason = "seven-day scheduling"
	}
	revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: plan.CurrentRevision, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(snapshot), Reason: reason, CreatedAt: now}
	if err := tx.Create(&revision).Error; err != nil {
		return err
	}
	return createOutbox(tx, plan.UserID, "daily_plan.scheduled", map[string]any{"plan_id": plan.ID, "local_date": plan.LocalDate})
}
