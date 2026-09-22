package application

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

var (
	ErrNotFound             = errors.New("resource not found")
	ErrConflict             = errors.New("resource conflict")
	ErrRevision             = errors.New("revision mismatch")
	ErrPrecondition         = errors.New("precondition required")
	ErrValidation           = errors.New("validation failed")
	ErrActiveSession        = errors.New("active work session exists")
	ErrStaleAgentAttempt    = errors.New("stale agent attempt")
	ErrIdempotencyKeyReuse  = errors.New("idempotency key reused")
	ErrExternalImportExists = errors.New("external import already exists")
	ErrForbidden            = errors.New("administrator permission required")
)

// App is the application aggregate. wiring.md §4 splits it into focused
// per-aggregate services; step 3 extracts ProviderService and LensService, which
// App embeds so existing call sites resolve them by promotion while the split
// proceeds (steps 4-6 extract the remaining aggregates). The embedded services are
// independently constructible plain structs (wiring.md §2 rule 2), so they can be
// built and unit-tested without App or fx.
type App struct {
	Store *persistence.Store
	*ProviderService
	*LensService
	*DeviceService
	*IntegrationService
	*AdminService
}

func New(store *persistence.Store) *App {
	return newApp(store, nil)
}

func NewWithSecret(store *persistence.Store, secret string) *App {
	return newApp(store, deriveSecretKey(secret))
}

// newApp assembles the aggregate with the given (possibly nil) provider key. It
// preserves the historical New vs NewWithSecret behaviour exactly: New passes a
// nil key (encryption fails closed), NewWithSecret always derives one.
func newApp(store *persistence.Store, secretKey []byte) *App {
	return &App{
		Store:              store,
		ProviderService:    NewProviderService(store, secretKey),
		LensService:        NewLensService(store),
		DeviceService:      NewDeviceService(store),
		IntegrationService: NewIntegrationService(store),
		AdminService:       NewAdminService(store),
	}
}

func (a *App) CreateGoal(ctx context.Context, userID string, goal *persistence.Goal) error {
	now := persistence.Now()
	goal.ID, goal.UserID, goal.Status, goal.Revision = persistence.NewID("goal"), userID, "active", 1
	goal.CreatedAt, goal.UpdatedAt = now, now
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(goal).Error; err != nil {
			return err
		}
		return createOutbox(tx, userID, "goal.created", map[string]any{"goal_id": goal.ID})
	})
}

func (a *App) UpdateGoal(ctx context.Context, userID, id string, expected int, changes map[string]any) (*persistence.Goal, error) {
	var goal persistence.Goal
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&goal).Error; err != nil {
		return nil, notFound(err)
	}
	if goal.Revision != expected {
		return nil, ErrRevision
	}
	if next, ok := changes["status"].(string); ok {
		if err := domain.ValidateGoalTransition(goal.Status, next); err != nil {
			return nil, ErrConflict
		}
	}
	changes["revision"], changes["updated_at"] = goal.Revision+1, persistence.Now()
	result := a.Store.DB.WithContext(ctx).Model(&persistence.Goal{}).Where("id = ? AND user_id = ? AND revision = ?", id, userID, expected).Updates(changes)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrRevision
	}
	if err := a.Store.DB.WithContext(ctx).First(&goal, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &goal, nil
}

func (a *App) CreateTask(ctx context.Context, userID string, task *persistence.Task) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		return createTaskTx(tx, userID, task, "manual task creation", "user")
	})
}

type ConvertExternalImportCommand struct {
	ExistingTaskID  *string
	GoalID          string
	ParentID        *string
	Type            string
	Title           string
	Description     string
	SuccessCriteria string
	MinimumAction   string
	Priority        int
	EstimateMinutes int
	Position        int
	DecisionNote    string
}

func (a *IntegrationService) CreateExternalImport(ctx context.Context, userID string, item *persistence.ExternalImport) error {
	item.SchemaVersion = strings.TrimSpace(item.SchemaVersion)
	item.SourceSystem = strings.TrimSpace(item.SourceSystem)
	item.SourceExternalID = strings.TrimSpace(item.SourceExternalID)
	item.Kind = strings.TrimSpace(item.Kind)
	item.Title = strings.TrimSpace(item.Title)
	if item.SchemaVersion == "" {
		item.SchemaVersion = "1.0"
	}
	if item.SchemaVersion != "1.0" || !domain.ValidExternalImportSource(item.SourceSystem) || !domain.ValidExternalImportKind(item.Kind) || item.SourceExternalID == "" || item.Title == "" {
		return ErrValidation
	}
	if item.ArtifactsJSON == "" {
		item.ArtifactsJSON = "[]"
	}
	if item.MetadataJSON == "" {
		item.MetadataJSON = "{}"
	}
	var artifacts []any
	var metadata map[string]any
	if err := json.Unmarshal([]byte(item.ArtifactsJSON), &artifacts); err != nil {
		return ErrValidation
	}
	if err := json.Unmarshal([]byte(item.MetadataJSON), &metadata); err != nil {
		return ErrValidation
	}
	if len(item.ArtifactsJSON) > 128<<10 || len(item.MetadataJSON) > 128<<10 {
		return ErrValidation
	}
	now := persistence.Now()
	item.ID, item.UserID, item.Status, item.Revision = persistence.NewID("import"), userID, "candidate", 1
	item.CreatedAt, item.UpdatedAt = now, now
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if item.SuggestedGoalID != nil {
			var goal persistence.Goal
			if err := tx.Where("id = ? AND user_id = ?", *item.SuggestedGoalID, userID).First(&goal).Error; err != nil {
				return notFound(err)
			}
		}
		if err := tx.Create(item).Error; err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrExternalImportExists
			}
			return err
		}
		return createOutbox(tx, userID, "external_import.created", map[string]any{"import_id": item.ID, "source_system": item.SourceSystem, "source_external_id": item.SourceExternalID})
	})
}

func (a *IntegrationService) ConvertExternalImport(ctx context.Context, userID, id string, expected int, command ConvertExternalImportCommand) (*persistence.ExternalImport, *persistence.Task, error) {
	var item persistence.ExternalImport
	var task persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&item).Error; err != nil {
			return notFound(err)
		}
		if item.Revision != expected {
			return ErrRevision
		}
		if err := domain.ValidateExternalImportTransition(item.Status, "converted"); err != nil {
			return ErrConflict
		}
		if command.ExistingTaskID != nil {
			if strings.TrimSpace(*command.ExistingTaskID) == "" {
				return ErrValidation
			}
			if err := tx.Where("id = ? AND user_id = ?", *command.ExistingTaskID, userID).First(&task).Error; err != nil {
				return notFound(err)
			}
		} else {
			if strings.TrimSpace(command.GoalID) == "" && item.SuggestedGoalID != nil {
				command.GoalID = *item.SuggestedGoalID
			}
			if strings.TrimSpace(command.Title) == "" {
				command.Title = item.Title
			}
			if strings.TrimSpace(command.Description) == "" {
				command.Description = item.Description
			}
			task = persistence.Task{GoalID: command.GoalID, ParentID: command.ParentID, Type: command.Type, Title: command.Title, Description: command.Description, SuccessCriteria: command.SuccessCriteria, MinimumAction: command.MinimumAction, Priority: command.Priority, EstimateMinutes: command.EstimateMinutes, Position: command.Position}
			if err := createTaskTx(tx, userID, &task, "external import converted", "external_import"); err != nil {
				return err
			}
		}
		now := persistence.Now()
		result := tx.Model(&persistence.ExternalImport{}).Where("id = ? AND user_id = ? AND revision = ? AND status = 'candidate'", item.ID, userID, expected).Updates(map[string]any{"status": "converted", "task_id": task.ID, "decision_note": strings.TrimSpace(command.DecisionNote), "decided_at": now, "revision": expected + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return createOutbox(tx, userID, "external_import.converted", map[string]any{"import_id": item.ID, "task_id": task.ID})
	})
	if err != nil {
		return nil, nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&item, "id = ?", id).Error; err != nil {
		return nil, nil, err
	}
	return &item, &task, nil
}

func (a *IntegrationService) RejectExternalImport(ctx context.Context, userID, id string, expected int, note string) (*persistence.ExternalImport, error) {
	var item persistence.ExternalImport
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&item).Error; err != nil {
			return notFound(err)
		}
		if item.Revision != expected {
			return ErrRevision
		}
		if err := domain.ValidateExternalImportTransition(item.Status, "rejected"); err != nil {
			return ErrConflict
		}
		now := persistence.Now()
		result := tx.Model(&persistence.ExternalImport{}).Where("id = ? AND user_id = ? AND revision = ? AND status = 'candidate'", item.ID, userID, expected).Updates(map[string]any{"status": "rejected", "decision_note": strings.TrimSpace(note), "decided_at": now, "revision": expected + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return createOutbox(tx, userID, "external_import.rejected", map[string]any{"import_id": item.ID})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&item, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *App) UpdateTask(ctx context.Context, userID, id string, expected int, changes map[string]any) (*persistence.Task, error) {
	var task persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&task).Error; err != nil {
			return notFound(err)
		}
		if task.Revision != expected {
			return ErrRevision
		}
		if status, ok := changes["status"].(string); ok && status == "completed" {
			return ErrValidation
		}
		if parent, ok := changes["parent_id"].(string); ok && parent != "" {
			var tasks []persistence.Task
			if err := tx.Where("goal_id = ? AND user_id = ?", task.GoalID, userID).Find(&tasks).Error; err != nil {
				return err
			}
			parents := map[string]string{}
			valid := false
			for _, item := range tasks {
				if item.ParentID != nil {
					parents[item.ID] = *item.ParentID
				}
				if item.ID == parent {
					valid = true
				}
			}
			if !valid || domain.WouldCreateCycle(task.ID, parent, parents) {
				return ErrValidation
			}
		}
		changes["revision"], changes["updated_at"] = task.Revision+1, persistence.Now()
		result := tx.Model(&persistence.Task{}).Where("id = ? AND user_id = ? AND revision = ?", id, userID, expected).Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return snapshotTaskTree(tx, userID, task.GoalID, "manual task update", "user")
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&task, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &task, nil
}

func (a *App) ReplanDailyPlan(ctx context.Context, userID, localDate, timezone string, taskIDs []string, available, expected int) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	var plan persistence.DailyPlan
	var resultItems []persistence.DailyPlanItem
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, localDate, timezone).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expected {
			return ErrRevision
		}
		var oldItems []persistence.DailyPlanItem
		if err := tx.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position, id").Find(&oldItems).Error; err != nil {
			return err
		}
		now := persistence.Now()
		newRevision := plan.CurrentRevision + 1
		satisfiedTaskIDs := map[string]bool{}
		coreSlots := 3
		for _, item := range oldItems {
			if item.Status == "satisfied" {
				copy := item
				copy.ID, copy.PlanRevision, copy.Revision, copy.CreatedAt, copy.UpdatedAt = persistence.NewID("dpi"), newRevision, 1, now, now
				resultItems = append(resultItems, copy)
				if copy.Kind == "core" {
					coreSlots--
				}
				if copy.TaskID != nil {
					satisfiedTaskIDs[*copy.TaskID] = true
				}
			} else if err := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{"status": "superseded", "revision": item.Revision + 1, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if coreSlots < 0 {
			coreSlots = 0
		}
		var tasks []persistence.Task
		query := tx.Table("tasks").Select("tasks.*").Joins("JOIN goals ON goals.id = tasks.goal_id").Where("tasks.user_id = ? AND goals.status = 'active'", userID)
		if len(taskIDs) > 0 {
			query = query.Where("tasks.id IN ?", taskIDs)
		}
		if err := query.Find(&tasks).Error; err != nil {
			return err
		}
		candidates := make([]domain.Candidate, 0, len(tasks))
		byID := map[string]persistence.Task{}
		for _, task := range tasks {
			if satisfiedTaskIDs[task.ID] {
				continue
			}
			byID[task.ID] = task
			candidates = append(candidates, domain.Candidate{ID: task.ID, Status: task.Status, Priority: task.Priority, Estimate: task.EstimateMinutes, MinimumAction: task.MinimumAction, GoalActive: true, DependencyDone: true})
		}
		selected := domain.SelectDailyCandidates(candidates, available)
		if len(selected) > coreSlots {
			selected = selected[:coreSlots]
		}
		position := 1
		for _, existing := range resultItems {
			if existing.Kind == "core" && existing.Position >= position {
				position = existing.Position + 1
			}
		}
		for _, candidate := range selected {
			task := byID[candidate.ID]
			taskID := task.ID
			resultItems = append(resultItems, persistence.DailyPlanItem{ID: persistence.NewID("dpi"), UserID: userID, PlanID: plan.ID, PlanRevision: newRevision, TaskID: &taskID, Kind: "core", Title: task.Title, Commitment: "推进：" + task.SuccessCriteria, MinimumAction: task.MinimumAction, AllowedTypes: "result,step,time,minimum_action", TargetMinutes: 50, Status: "planned", Position: position, Revision: 1, CreatedAt: now, UpdatedAt: now})
			position++
		}
		if len(resultItems) > 0 {
			if err := tx.Create(&resultItems).Error; err != nil {
				return err
			}
		}
		input, _ := json.Marshal(map[string]any{"task_ids": taskIDs, "available_minutes": available, "algorithm": plan.AlgorithmVersion, "base_revision": expected})
		snapshot, _ := json.Marshal(resultItems)
		revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: newRevision, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(snapshot), Reason: "same-day replanning", CreatedAt: now}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		update := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, expected).Updates(map[string]any{"current_revision": newRevision, "revision": expected + 1, "status": "active", "updated_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrRevision
		}
		return createOutbox(tx, userID, "daily_plan.replanned", map[string]any{"plan_id": plan.ID, "revision": newRevision})
	})
	if err != nil {
		return nil, nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&plan, "id = ?", plan.ID).Error; err != nil {
		return nil, nil, err
	}
	return &plan, resultItems, nil
}

func (a *App) CloseDailyPlan(ctx context.Context, userID, planID string, expected int, status string) (*persistence.DailyPlan, error) {
	var plan persistence.DailyPlan
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expected {
			return ErrRevision
		}
		if status != "active" && status != "closed" && status != "cancelled" {
			return ErrValidation
		}
		now := persistence.Now()
		if status == "closed" {
			if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND status IN ('planned','in_progress')", plan.ID, plan.CurrentRevision).Updates(map[string]any{"status": "not_completed", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, expected).Updates(map[string]any{"status": status, "revision": expected + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&plan, "id = ?", plan.ID).Error; err != nil {
		return nil, err
	}
	return &plan, nil
}

func (a *App) CompleteTask(ctx context.Context, userID, id string, expected int, eventType, summary string, reopen bool) (*persistence.Task, error) {
	var task persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&task).Error; err != nil {
			return notFound(err)
		}
		if task.Revision != expected {
			return ErrRevision
		}
		status := "completed"
		if reopen {
			if task.Status != "completed" {
				return ErrConflict
			}
			status, eventType = "in_progress", "task_reopened"
		} else if strings.TrimSpace(summary) == "" || task.SuccessCriteria == "" {
			return ErrValidation
		}
		now := persistence.Now()
		if result := tx.Model(&persistence.Task{}).Where("id = ? AND revision = ?", id, expected).Updates(map[string]any{"status": status, "revision": expected + 1, "updated_at": now}); result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return ErrRevision
		}
		event := persistence.ProgressEvent{ID: persistence.NewID("progress"), UserID: userID, GoalID: &task.GoalID, TaskID: &task.ID, Type: eventType, Summary: summary, EvidenceJSON: "[]", OccurredAt: now, CreatedAt: now}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		if err := snapshotTaskTree(tx, userID, task.GoalID, eventType, "user"); err != nil {
			return err
		}
		return createOutbox(tx, userID, "task.status_changed", map[string]any{"task_id": id, "status": status})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&task, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &task, nil
}

func (a *App) CreateDailyPlan(ctx context.Context, userID, localDate, timezone string, taskIDs []string, available int) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	var tasks []persistence.Task
	query := a.Store.DB.WithContext(ctx).Table("tasks").Select("tasks.*").Joins("JOIN goals ON goals.id = tasks.goal_id").Where("tasks.user_id = ? AND goals.status = 'active'", userID)
	if len(taskIDs) > 0 {
		query = query.Where("tasks.id IN ?", taskIDs)
	}
	if err := query.Find(&tasks).Error; err != nil {
		return nil, nil, err
	}
	candidates := make([]domain.Candidate, 0, len(tasks))
	byID := map[string]persistence.Task{}
	for _, task := range tasks {
		byID[task.ID] = task
		candidates = append(candidates, domain.Candidate{ID: task.ID, Status: task.Status, Priority: task.Priority, Estimate: task.EstimateMinutes, MinimumAction: task.MinimumAction, GoalActive: true, DependencyDone: true})
	}
	selected := domain.SelectDailyCandidates(candidates, available)
	now := persistence.Now()
	plan := persistence.DailyPlan{ID: persistence.NewID("plan"), UserID: userID, LocalDate: localDate, Timezone: timezone, Status: "active", AlgorithmVersion: "deterministic-v1", CurrentRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	items := make([]persistence.DailyPlanItem, 0, len(selected))
	for position, candidate := range selected {
		task := byID[candidate.ID]
		id := task.ID
		items = append(items, persistence.DailyPlanItem{ID: persistence.NewID("dpi"), UserID: userID, PlanID: plan.ID, PlanRevision: 1, TaskID: &id, Kind: "core", Title: task.Title, Commitment: "推进：" + task.SuccessCriteria, MinimumAction: task.MinimumAction, AllowedTypes: "result,step,time,minimum_action", TargetMinutes: 50, Status: "planned", Position: position + 1, Revision: 1, CreatedAt: now, UpdatedAt: now})
	}
	input, _ := json.Marshal(map[string]any{"task_ids": taskIDs, "available_minutes": available, "algorithm": plan.AlgorithmVersion})
	planJSON, _ := json.Marshal(items)
	revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: 1, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(planJSON), Reason: "initial generation", CreatedAt: now}
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(&plan).Error; err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrConflict
			}
			return err
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		return createOutbox(tx, userID, "daily_plan.created", map[string]any{"plan_id": plan.ID})
	})
	if err != nil {
		return nil, nil, err
	}
	return &plan, items, nil
}

func (a *App) AddPlanItem(ctx context.Context, userID, planID string, expectedPlan int, item *persistence.DailyPlanItem) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var plan persistence.DailyPlan
		if err := tx.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expectedPlan {
			return ErrRevision
		}
		var existingTask int64
		if item.Kind == "core" && item.TaskID == nil {
			return ErrValidation
		}
		if item.TaskID != nil {
			var task persistence.Task
			if err := tx.Where("id = ? AND user_id = ?", *item.TaskID, userID).First(&task).Error; err != nil {
				return notFound(err)
			}
			if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND task_id = ? AND status != 'superseded'", planID, *item.TaskID).Count(&existingTask).Error; err != nil {
				return err
			}
			if existingTask > 0 {
				return ErrConflict
			}
		}
		var core, unsatisfied int64
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'core' AND status != 'superseded'", planID).Count(&core).Error; err != nil {
			return err
		}
		if item.Kind == "core" && core >= 3 {
			return domain.ErrCoreLimit
		}
		if item.Kind != "core" {
			if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'core' AND status != 'satisfied' AND status != 'superseded'", planID).Count(&unsatisfied).Error; err != nil {
				return err
			}
			if unsatisfied > 0 {
				return domain.ErrSupportBeforeCoreDone
			}
		}
		now := persistence.Now()
		item.ID, item.UserID, item.PlanID, item.PlanRevision = persistence.NewID("dpi"), userID, planID, plan.CurrentRevision
		item.Status, item.Revision, item.CreatedAt, item.UpdatedAt = "planned", 1, now, now
		if item.AllowedTypes == "" {
			item.AllowedTypes = "result,step,time,minimum_action"
		}
		if err := tx.Create(item).Error; err != nil {
			return err
		}
		result := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", planID, expectedPlan).Updates(map[string]any{"revision": expectedPlan + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
}

func (a *App) CompletePlanItem(ctx context.Context, userID, itemID string, expected int, completionType, summary string, sessionIDs []string) (*persistence.DailyPlanItem, error) {
	var item persistence.DailyPlanItem
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", itemID, userID).First(&item).Error; err != nil {
			return notFound(err)
		}
		if item.Revision != expected {
			return ErrRevision
		}
		if item.Status == "satisfied" {
			return nil
		}
		if !domain.ValidateCompletionType(completionType, item.AllowedTypes) {
			return ErrValidation
		}
		if completionType == "time" {
			var total int64
			if len(sessionIDs) == 0 {
				return ErrValidation
			}
			if err := tx.Model(&persistence.WorkSession{}).Where("id IN ? AND user_id = ? AND daily_plan_item_id = ? AND status IN ('completed','stopped')", sessionIDs, userID, itemID).Select("COALESCE(SUM(duration_seconds),0)").Scan(&total).Error; err != nil {
				return err
			}
			if total < int64(item.TargetMinutes*60) {
				return ErrValidation
			}
		}
		if strings.TrimSpace(summary) == "" {
			return ErrValidation
		}
		now := persistence.Now()
		result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", itemID, expected).Updates(map[string]any{"status": "satisfied", "completion_type": completionType, "completion_summary": summary, "revision": expected + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		event := persistence.ProgressEvent{ID: persistence.NewID("progress"), UserID: userID, TaskID: item.TaskID, PlanItemID: &item.ID, Type: completionType, Summary: summary, EvidenceJSON: "[]", OccurredAt: now, CreatedAt: now}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		return createOutbox(tx, userID, "daily_item.satisfied", map[string]any{"item_id": item.ID})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&item, "id = ?", itemID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *App) UpdatePlanItem(ctx context.Context, userID, planID, itemID string, expected int, changes map[string]any) (*persistence.DailyPlanItem, error) {
	var item persistence.DailyPlanItem
	if status, exists := changes["status"].(string); exists && status != "skipped" {
		return nil, ErrValidation
	}
	changes["revision"], changes["updated_at"] = expected+1, persistence.Now()
	result := a.Store.DB.WithContext(ctx).Model(&persistence.DailyPlanItem{}).Where("id = ? AND plan_id = ? AND user_id = ? AND revision = ?", itemID, planID, userID, expected).Updates(changes)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrRevision
	}
	if err := a.Store.DB.WithContext(ctx).First(&item, "id = ?", itemID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *App) StartSession(ctx context.Context, userID string, session *persistence.WorkSession) error {
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var task persistence.Task
		if err := tx.Where("id = ? AND user_id = ?", session.TaskID, userID).First(&task).Error; err != nil {
			return notFound(err)
		}
		if session.DailyPlanItemID != nil {
			var item persistence.DailyPlanItem
			if err := tx.Where("id = ? AND user_id = ?", *session.DailyPlanItemID, userID).First(&item).Error; err != nil {
				return notFound(err)
			}
			if item.TaskID == nil || *item.TaskID != session.TaskID {
				return ErrValidation
			}
		}
		now := persistence.Now()
		if session.StartedAt.IsZero() {
			session.StartedAt = now
		}
		session.ID, session.UserID, session.Status, session.Revision = persistence.NewID("work"), userID, "running", 1
		session.CreatedAt, session.UpdatedAt = now, now
		if session.SessionType == "" {
			session.SessionType = "pomodoro"
		}
		if session.TargetMinutes == 0 {
			session.TargetMinutes = 25
		}
		return tx.Create(session).Error
	})
	if err != nil {
		if strings.Contains(err.Error(), "idx_one_active_session") || strings.Contains(err.Error(), "UNIQUE") {
			return ErrActiveSession
		}
		return err
	}
	return nil
}

func (a *App) TransitionSession(ctx context.Context, userID, id string, expected int, action, outcome, note string, at time.Time) (*persistence.WorkSession, error) {
	var session persistence.WorkSession
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
			return notFound(err)
		}
		if session.Revision != expected {
			return ErrRevision
		}
		if at.IsZero() {
			at = persistence.Now()
		}
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now()}
		switch action {
		case "pause":
			if session.Status != "running" {
				return ErrConflict
			}
			changes["status"], changes["paused_at"] = "paused", at
		case "resume":
			if session.Status != "paused" || session.PausedAt == nil {
				return ErrConflict
			}
			changes["status"], changes["accumulated_seconds"], changes["paused_at"] = "running", session.AccumulatedSeconds+int(at.Sub(*session.PausedAt).Seconds()), nil
		case "complete", "stop":
			if session.Status != "running" && session.Status != "paused" {
				return ErrConflict
			}
			duration := int(at.Sub(session.StartedAt).Seconds()) - session.AccumulatedSeconds
			if session.Status == "paused" && session.PausedAt != nil {
				duration -= int(at.Sub(*session.PausedAt).Seconds())
			}
			if duration < 0 {
				duration = 0
			}
			status := "completed"
			if action == "stop" {
				status = "stopped"
			}
			changes["status"], changes["ended_at"], changes["duration_seconds"], changes["outcome"], changes["note"] = status, at, duration, outcome, note
		case "invalidate":
			if session.Status != "completed" && session.Status != "stopped" {
				return ErrConflict
			}
			changes["status"] = "invalidated"
		default:
			return ErrValidation
		}
		result := tx.Model(&persistence.WorkSession{}).Where("id = ? AND revision = ?", id, expected).Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&session, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

func (a *App) CreateJob(ctx context.Context, userID, jobType, subjectType, subjectID string, baseRevision int, input any) (*persistence.AgentJob, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	now := persistence.Now()
	job := persistence.AgentJob{ID: persistence.NewID("job"), UserID: userID, Type: jobType, Status: "queued", SubjectType: subjectType, SubjectID: subjectID, BaseRevision: baseRevision, InputJSON: string(encoded), MaxAttempts: 3, RunAfter: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := a.Store.DB.WithContext(ctx).Create(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *App) RetryJob(ctx context.Context, userID, id string, expected int) (*persistence.AgentJob, error) {
	var old persistence.AgentJob
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&old).Error; err != nil {
		return nil, notFound(err)
	}
	if old.Revision != expected {
		return nil, ErrRevision
	}
	if old.Status != "failed" {
		return nil, ErrConflict
	}
	now := persistence.Now()
	job := old
	retryOf := old.ID
	job.ID, job.Status, job.RetryOfJobID, job.AttemptCount, job.LeaseVersion, job.Revision = persistence.NewID("job"), "queued", &retryOf, 0, 0, 1
	job.ErrorCode, job.ErrorMessage, job.OutputJSON, job.LockedBy, job.RunTokenHash = "", "", "", "", ""
	job.LockedUntil, job.StartedAt, job.FinishedAt = nil, nil, nil
	job.RunAfter, job.CreatedAt, job.UpdatedAt = now, now, now
	if err := a.Store.DB.WithContext(ctx).Create(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *App) CancelJob(ctx context.Context, userID, id string, expected int) (*persistence.AgentJob, error) {
	var job persistence.AgentJob
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
		return nil, notFound(err)
	}
	if job.Revision != expected {
		return nil, ErrRevision
	}
	if job.Status != "queued" && job.Status != "running" {
		return nil, ErrConflict
	}
	status := job.Status
	updates := map[string]any{"cancel_requested": true, "revision": expected + 1, "updated_at": persistence.Now()}
	if job.Status == "queued" {
		status = "cancelled"
		updates["status"] = status
		updates["finished_at"] = persistence.Now()
	}
	if err := a.Store.DB.WithContext(ctx).Model(&job).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *App) AgentCallback(ctx context.Context, userID, id string, attempt, lease int, runToken, status string, output any, errorCode, errorMessage string) (*persistence.AgentJob, error) {
	var job persistence.AgentJob
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
			return notFound(err)
		}
		if job.Status != "running" || job.AttemptCount != attempt || job.LeaseVersion != lease || job.RunTokenHash != persistence.Hash(runToken) {
			return ErrStaleAgentAttempt
		}
		if job.CancelRequested {
			status = "cancelled"
		}
		if status != "succeeded" && status != "failed" && status != "cancelled" {
			return ErrValidation
		}
		encoded, _ := json.Marshal(output)
		now := persistence.Now()
		if status == "succeeded" {
			if err := a.MaterializeJobResult(tx, job, output, now); err != nil {
				return err
			}
		}
		updates := map[string]any{"status": status, "output_json": string(encoded), "error_code": errorCode, "error_message": errorMessage, "finished_at": now, "locked_by": "", "locked_until": nil, "run_token_hash": "", "revision": job.Revision + 1, "updated_at": now}
		result := tx.Model(&persistence.AgentJob{}).Where("id = ? AND user_id = ? AND status = 'running' AND attempt_count = ? AND lease_version = ? AND run_token_hash = ?", id, userID, attempt, lease, persistence.Hash(runToken)).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrStaleAgentAttempt
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *App) RejectProposal(ctx context.Context, userID, goalID, proposalID string, expected int) (*persistence.Proposal, error) {
	var proposal persistence.Proposal
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND goal_id = ? AND user_id = ? AND status = 'pending'", proposalID, goalID, userID).First(&proposal).Error; err != nil {
			return notFound(err)
		}
		if proposal.Revision != expected {
			return ErrRevision
		}
		result := tx.Model(&persistence.Proposal{}).Where("id = ? AND revision = ? AND status = 'pending'", proposal.ID, expected).Updates(map[string]any{"status": "rejected", "revision": expected + 1, "updated_at": persistence.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&proposal, "id = ?", proposal.ID).Error; err != nil {
		return nil, err
	}
	return &proposal, nil
}

func (a *App) MaterializeJobResult(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error {
	encoded, _ := json.Marshal(output)
	switch job.Type {
	case "task_tree_generation", "task_tree_revision":
		var result map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &result); err != nil || len(result["proposal"]) == 0 {
			return ErrValidation
		}
		proposal := persistence.Proposal{ID: persistence.NewID("proposal"), UserID: job.UserID, GoalID: job.SubjectID, JobID: job.ID, BaseRevision: job.BaseRevision, Status: "pending", Instruction: "agent proposal", PatchJSON: string(result["proposal"]), Revision: 1, CreatedAt: now, UpdatedAt: now}
		return tx.Create(&proposal).Error
	case "conversation":
		var input map[string]any
		_ = json.Unmarshal([]byte(job.InputJSON), &input)
		var result map[string]any
		_ = json.Unmarshal(encoded, &result)
		conversationID, reply := textValue(input["conversation_id"]), textValue(result["reply"])
		if conversationID == "" || reply == "" {
			return ErrValidation
		}
		return tx.Create(&persistence.ConversationMessage{ID: persistence.NewID("msg"), UserID: job.UserID, ConversationID: conversationID, Role: "assistant", Content: reply, JobID: job.ID, CreatedAt: now}).Error
	case "support_generation":
		var plan persistence.DailyPlan
		if err := tx.Where("id = ? AND user_id = ?", job.SubjectID, job.UserID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		var unsatisfied int64
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'core' AND status NOT IN ('satisfied','superseded')", plan.ID).Count(&unsatisfied).Error; err != nil {
			return err
		}
		if unsatisfied > 0 {
			return ErrConflict
		}
		item := persistence.DailyPlanItem{ID: persistence.NewID("dpi"), UserID: job.UserID, PlanID: plan.ID, PlanRevision: plan.CurrentRevision, Kind: "input", Title: "阅读与当前阻碍直接相关的资料", Commitment: "限定范围阅读并记录三条与当前阻碍直接相关的信息", MinimumAction: "打开一份相关资料并定位摘要", AllowedTypes: "result,step,time,minimum_action", TargetMinutes: 25, Status: "planned", Position: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&item).Error; err != nil {
			return err
		}
		result := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, plan.Revision).Updates(map[string]any{"revision": plan.Revision + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
	}
	return nil
}

func (a *DeviceService) RegisterDevice(ctx context.Context, userID string, device *persistence.Device) (string, error) {
	token, err := secureToken()
	if err != nil {
		return "", err
	}
	now := persistence.Now()
	device.ID, device.UserID, device.TokenHash, device.Status, device.Revision = persistence.NewID("device"), userID, persistence.Hash(token), "active", 1
	device.CreatedAt, device.UpdatedAt = now, now
	if device.Kind == "" {
		device.Kind = "eink_panel"
	}
	if device.CapabilitiesJSON == "" {
		device.CapabilitiesJSON = "{}"
	}
	return token, a.Store.DB.WithContext(ctx).Create(device).Error
}

func (a *DeviceService) RotateDeviceToken(ctx context.Context, userID, deviceID string, expected int) (*persistence.Device, string, error) {
	var device persistence.Device
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", deviceID, userID).First(&device).Error; err != nil {
		return nil, "", notFound(err)
	}
	if device.Revision != expected {
		return nil, "", ErrRevision
	}
	token, err := secureToken()
	if err != nil {
		return nil, "", err
	}
	result := a.Store.DB.WithContext(ctx).Model(&persistence.Device{}).Where("id = ? AND revision = ?", device.ID, expected).Updates(map[string]any{"token_hash": persistence.Hash(token), "revision": expected + 1, "updated_at": persistence.Now()})
	if result.Error != nil {
		return nil, "", result.Error
	}
	if result.RowsAffected != 1 {
		return nil, "", ErrRevision
	}
	if err := a.Store.DB.WithContext(ctx).First(&device, "id = ?", device.ID).Error; err != nil {
		return nil, "", err
	}
	return &device, token, nil
}

func secureToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := cryptorand.Read(tokenBytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(tokenBytes), nil
}

func (a *DeviceService) DeviceByToken(ctx context.Context, token string) (*persistence.Device, error) {
	var device persistence.Device
	if err := a.Store.DB.WithContext(ctx).Where("token_hash = ? AND status = 'active'", persistence.Hash(token)).First(&device).Error; err != nil {
		return nil, notFound(err)
	}
	now := persistence.Now()
	a.Store.DB.WithContext(ctx).Model(&device).Update("last_seen_at", now)
	return &device, nil
}

func (a *App) ApplyProposal(ctx context.Context, userID string, proposal *persistence.Proposal, patches []map[string]any, expectedTreeRevision ...int) (int, error) {
	created := 0
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var current persistence.Proposal
		if err := tx.Where("id = ? AND user_id = ? AND status = 'pending'", proposal.ID, userID).First(&current).Error; err != nil {
			return notFound(err)
		}
		var treeRevision int
		if err := tx.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", current.GoalID).Select("COALESCE(MAX(revision),0)").Scan(&treeRevision).Error; err != nil {
			return err
		}
		if len(expectedTreeRevision) > 0 && treeRevision != expectedTreeRevision[0] {
			return ErrRevision
		}
		if current.BaseRevision > 0 && current.BaseRevision != treeRevision {
			return ErrRevision
		}
		now := persistence.Now()
		var existing []persistence.Task
		if err := tx.Where("goal_id = ? AND user_id = ?", current.GoalID, userID).Find(&existing).Error; err != nil {
			return err
		}
		byID := map[string]persistence.Task{}
		parents := map[string]string{}
		for _, task := range existing {
			byID[task.ID] = task
			if task.ParentID != nil {
				parents[task.ID] = *task.ParentID
			}
		}
		refs := map[string]string{}
		for i, patch := range patches {
			op := strings.TrimSpace(fmt.Sprint(patch["op"]))
			if op == "" || op == "<nil>" {
				op = "create"
			}
			switch op {
			case "create":
				task := persistence.Task{ID: persistence.NewID("task"), UserID: userID, GoalID: current.GoalID, Type: textValue(patch["type"]), Title: textValue(patch["title"]), Description: textValue(patch["description"]), SuccessCriteria: textValue(patch["success_criteria"]), MinimumAction: textValue(patch["minimum_action"]), Priority: intValue(patch["priority"]), EstimateMinutes: intValue(patch["estimate_minutes"]), Position: i, Status: "ready", Revision: 1, CreatedAt: now, UpdatedAt: now}
				if task.Type == "" || task.Title == "" || task.SuccessCriteria == "" || task.MinimumAction == "" {
					return ErrValidation
				}
				if task.Priority == 0 {
					task.Priority = 50
				}
				if task.EstimateMinutes == 0 {
					task.EstimateMinutes = 25
				}
				if task.Priority < 0 || task.Priority > 100 || task.EstimateMinutes < 1 || task.EstimateMinutes > 1440 || task.Position < 0 {
					return ErrValidation
				}
				parent := textValue(patch["parent_id"])
				if parent == "" {
					parent = refs[textValue(patch["parent_ref"])]
				}
				if parent != "" {
					if _, ok := byID[parent]; !ok {
						return ErrValidation
					}
					task.ParentID = &parent
					parents[task.ID] = parent
				}
				if err := tx.Create(&task).Error; err != nil {
					return err
				}
				byID[task.ID] = task
				if coord, ok := coordFromPatch(patch); ok {
					if err := createAgentCoord(tx, userID, task.ID, coord, now); err != nil {
						return err
					}
				}
				if ref := textValue(patch["client_ref"]); ref != "" {
					refs[ref] = task.ID
				}
				created++
			case "update", "move", "supersede":
				targetID := textValue(patch["target_id"])
				task, ok := byID[targetID]
				if !ok {
					return ErrValidation
				}
				updates := map[string]any{"revision": task.Revision + 1, "updated_at": now}
				if op == "supersede" {
					updates["status"] = "superseded"
				} else {
					for _, field := range []string{"title", "description", "success_criteria", "minimum_action"} {
						if value := textValue(patch[field]); value != "" {
							updates[field] = value
						}
					}
					if status := textValue(patch["status"]); status != "" {
						if status != "ready" && status != "in_progress" && status != "blocked" && status != "paused" && status != "cancelled" {
							return ErrValidation
						}
						updates["status"] = status
					}
					for _, field := range []string{"priority", "estimate_minutes", "position"} {
						if _, exists := patch[field]; exists {
							value := intValue(patch[field])
							if field == "priority" && (value < 0 || value > 100) {
								return ErrValidation
							}
							if field == "estimate_minutes" && (value < 1 || value > 1440) {
								return ErrValidation
							}
							if field == "position" && value < 0 {
								return ErrValidation
							}
							updates[field] = value
						}
					}
					if op == "move" || patch["parent_id"] != nil || patch["parent_ref"] != nil {
						parent := textValue(patch["parent_id"])
						if parent == "" {
							parent = refs[textValue(patch["parent_ref"])]
						}
						if parent != "" {
							if _, ok := byID[parent]; !ok || domain.WouldCreateCycle(targetID, parent, parents) {
								return ErrValidation
							}
							updates["parent_id"] = parent
							parents[targetID] = parent
						} else {
							updates["parent_id"] = nil
							delete(parents, targetID)
						}
					}
				}
				result := tx.Model(&persistence.Task{}).Where("id = ? AND user_id = ? AND revision = ?", targetID, userID, task.Revision).Updates(updates)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrRevision
				}
				if op == "update" {
					if coord, ok := coordFromPatch(patch); ok {
						if err := updateAgentCoord(tx, userID, targetID, coord, now); err != nil {
							return err
						}
					}
				}
			default:
				return ErrValidation
			}
		}
		if err := snapshotTaskTree(tx, userID, current.GoalID, "agent proposal applied", "agent"); err != nil {
			return err
		}
		result := tx.Model(&persistence.Proposal{}).Where("id = ? AND revision = ? AND status = 'pending'", current.ID, current.Revision).Updates(map[string]any{"status": "applied", "revision": current.Revision + 1, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return createOutbox(tx, userID, "task_tree.proposal_applied", map[string]any{"proposal_id": current.ID, "goal_id": current.GoalID})
	})
	return created, err
}

func textValue(value any) string {
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "<nil>" {
		return ""
	}
	return text
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		var parsed int
		_, _ = fmt.Sscanf(fmt.Sprint(value), "%d", &parsed)
		return parsed
	}
}

func snapshotTaskTree(tx *gorm.DB, userID, goalID, reason, source string) error {
	var tasks []persistence.Task
	if err := tx.Where("goal_id = ? AND user_id = ?", goalID, userID).Order("position, id").Find(&tasks).Error; err != nil {
		return err
	}
	var revision int
	if err := tx.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goalID).Select("COALESCE(MAX(revision),0)").Scan(&revision).Error; err != nil {
		return err
	}
	snapshot, _ := json.Marshal(tasks)
	record := persistence.TaskTreeRevision{ID: persistence.NewID("treerev"), UserID: userID, GoalID: goalID, Revision: revision + 1, Reason: reason, Source: source, SnapshotJSON: string(snapshot), CreatedAt: persistence.Now()}
	return tx.Create(&record).Error
}

func createTaskTx(tx *gorm.DB, userID string, task *persistence.Task, reason, source string) error {
	var goal persistence.Goal
	if err := tx.Where("id = ? AND user_id = ?", task.GoalID, userID).First(&goal).Error; err != nil {
		return notFound(err)
	}
	if task.ParentID != nil {
		var parent persistence.Task
		if err := tx.Where("id = ? AND goal_id = ? AND user_id = ?", *task.ParentID, task.GoalID, userID).First(&parent).Error; err != nil {
			return ErrValidation
		}
	}
	task.Type = strings.TrimSpace(task.Type)
	task.Title = strings.TrimSpace(task.Title)
	task.SuccessCriteria = strings.TrimSpace(task.SuccessCriteria)
	task.MinimumAction = strings.TrimSpace(task.MinimumAction)
	if task.Type == "" {
		task.Type = "task"
	}
	if task.Type != "milestone" && task.Type != "task" && task.Type != "action" || task.Title == "" || task.SuccessCriteria == "" || task.MinimumAction == "" || task.Priority < 0 || task.Priority > 100 || task.Position < 0 {
		return ErrValidation
	}
	if task.Priority == 0 {
		task.Priority = 50
	}
	if task.EstimateMinutes == 0 {
		task.EstimateMinutes = 25
	}
	if task.EstimateMinutes < 1 || task.EstimateMinutes > 1440 {
		return ErrValidation
	}
	now := persistence.Now()
	task.ID, task.UserID, task.Status, task.Revision = persistence.NewID("task"), userID, "ready", 1
	task.CreatedAt, task.UpdatedAt = now, now
	if err := tx.Create(task).Error; err != nil {
		return err
	}
	return snapshotTaskTree(tx, userID, task.GoalID, reason, source)
}

func createOutbox(tx *gorm.DB, userID, eventType string, payload any) error {
	encoded, _ := json.Marshal(payload)
	event := persistence.OutboxEvent{ID: persistence.NewID("event"), UserID: userID, EventType: eventType, PayloadJSON: string(encoded), Status: "pending", CreatedAt: persistence.Now()}
	return tx.Create(&event).Error
}

func notFound(err error) error {
	if persistence.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func BuildTree(tasks []persistence.Task) []map[string]any {
	children := map[string][]persistence.Task{}
	for _, task := range tasks {
		key := ""
		if task.ParentID != nil {
			key = *task.ParentID
		}
		children[key] = append(children[key], task)
	}
	for key := range children {
		sort.Slice(children[key], func(i, j int) bool {
			if children[key][i].Position != children[key][j].Position {
				return children[key][i].Position < children[key][j].Position
			}
			return children[key][i].ID < children[key][j].ID
		})
	}
	var build func(string) []map[string]any
	build = func(parent string) []map[string]any {
		result := []map[string]any{}
		for _, task := range children[parent] {
			result = append(result, map[string]any{"task": task, "children": build(task.ID)})
		}
		return result
	}
	return build("")
}

func ParseDateInZone(date, zone string) error {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ErrValidation
	}
	_, err = time.ParseInLocation("2006-01-02", date, location)
	if err != nil {
		return ErrValidation
	}
	return nil
}

func StrongETag(kind, id string, revision int) string {
	return fmt.Sprintf("\"%s_%s_rev_%d\"", kind, id, revision)
}
