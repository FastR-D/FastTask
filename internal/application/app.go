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
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
	// ErrTooManyJobs refuses a job while its owner already has MaxPendingJobsPerUser unfinished ones.
	ErrTooManyJobs          = errors.New("too many unfinished jobs; wait for some to finish")
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
// per-aggregate services, which App embeds so existing call sites resolve them by
// promotion while the split proceeds. Each embedded service is an independently
// constructible plain struct (wiring.md §2 rule 2), so it can be built and
// unit-tested without App or fx. After step 6 App declares no domain methods of
// its own — only the agent readonly-tool adapters remain (§9: no struct >15
// methods); the cross-aggregate compositions live in the services (§4.1).
type App struct {
	Store *persistence.Store
	*ProviderService
	*LensService
	*DeviceService
	*IntegrationService
	*RemoteToolService
	*AdminService
	*JobService
	*GoalService
	*PlanService
	*ProgressService
	*NotificationService
	*NotificationTargetService
}

func New(store *persistence.Store) *App {
	return newApp(store, "", nil)
}

// NewWithSecret builds the aggregate with the provider encryption key. The
// variadic materializers let the fx composition root inject the
// "job_materializers" value group (wiring.md §5); callers that pass none (harden,
// tests) get the builtins, preserving the historical two-argument behaviour.
func NewWithSecret(store *persistence.Store, secret string, materializers ...JobMaterializer) *App {
	return newApp(store, secret, materializers)
}

// newApp assembles the aggregate from the operator's secret. It preserves the
// historical New vs NewWithSecret behaviour exactly: New passes an empty secret, so
// every derived key is nil and encryption fails closed; NewWithSecret always derives
// one. Each secret store derives its own key from its own label (secrets.go), so a
// model-provider ciphertext and a notification credential are never interchangeable.
// Services with cross-aggregate composition (§4.1) are constructed in dependency
// order: IntegrationService needs GoalService.createTaskTx, ProgressService needs
// PlanService.satisfyItemTx.
func newApp(store *persistence.Store, secret string, materializers []JobMaterializer) *App {
	var providerKey, notificationKey, integrationKey []byte
	if secret != "" {
		providerKey, notificationKey = deriveSecretKey(secret), deriveNotificationKey(secret)
		integrationKey = deriveLabeledKey("fasttask:integration-credential:v1:", secret)
	}
	goals := NewGoalService(store)
	plans := NewPlanService(store)
	notifications := NewNotificationService(store, notificationKey)
	imports := NewIntegrationService(store, goals)
	return &App{
		Store:              store,
		ProviderService:    NewProviderService(store, providerKey),
		LensService:        NewLensService(store),
		DeviceService:      NewDeviceService(store),
		IntegrationService: imports,
		RemoteToolService:  NewRemoteToolService(store, integrationKey),
		AdminService:       NewAdminService(store),
		JobService:         NewJobService(store, materializers),
		GoalService:        goals,
		PlanService:        plans,
		ProgressService:    NewProgressService(store, plans),

		NotificationService:       notifications,
		NotificationTargetService: NewNotificationTargetService(store, notificationKey, notifications),
	}
}

func (a *GoalService) CreateGoal(ctx context.Context, userID string, goal *persistence.Goal) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		return a.createGoalTx(tx, userID, goal)
	})
}

func (a *GoalService) UpdateGoal(ctx context.Context, userID, id string, expected int, changes map[string]any) (*persistence.Goal, error) {
	var goal *persistence.Goal
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		goal, err = a.updateGoalTx(tx, userID, id, expected, changes)
		return err
	})
	return goal, err
}

func (a *GoalService) CreateTask(ctx context.Context, userID string, task *persistence.Task) error {
	return a.createTask(ctx, userID, task, false)
}

// CreateTaskWithExplicitPriority preserves a supplied zero priority. Legacy Go
// callers of CreateTask still get the existing zero-value default of 50.
func (a *GoalService) CreateTaskWithExplicitPriority(ctx context.Context, userID string, task *persistence.Task) error {
	return a.createTask(ctx, userID, task, true)
}

func (a *GoalService) createTask(ctx context.Context, userID string, task *persistence.Task, explicitPriority bool) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		return a.createTaskWithPriorityTx(tx, userID, task, "manual task creation", "user", explicitPriority)
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
			if _, err := RequireLiveGoalTx(tx, userID, *item.SuggestedGoalID); err != nil {
				return err
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
			liveTask, err := RequireLiveTaskTx(tx, userID, *command.ExistingTaskID)
			if err != nil {
				return err
			}
			task = *liveTask
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
			// §4.1: Integration reads the import, Goal writes the task — composed in
			// this transaction by calling GoalService's tx-taking domain function.
			if err := a.Goals.createTaskTx(tx, userID, &task, "external import converted", "external_import"); err != nil {
				return err
			}
		}
		now := persistence.Now()
		updates := map[string]any{"status": "converted", "task_id": task.ID, "decision_note": strings.TrimSpace(command.DecisionNote), "decided_at": now, "revision": expected + 1, "updated_at": now}
		if command.ExistingTaskID != nil {
			metadata, restored, err := reattachTaskMaterialMetadata(item, task.ID, now)
			if err != nil {
				return err
			}
			if restored {
				updates["metadata_json"] = metadata
			}
		}
		result := tx.Model(&persistence.ExternalImport{}).Where("id = ? AND user_id = ? AND revision = ? AND status = 'candidate'", item.ID, userID, expected).Updates(updates)
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

func (a *GoalService) UpdateTask(ctx context.Context, userID, id string, expected int, changes map[string]any) (*persistence.Task, error) {
	var task *persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		task, err = a.updateTaskTx(tx, userID, id, expected, changes)
		return err
	})
	return task, err
}

func (a *PlanService) ReplanDailyPlan(ctx context.Context, userID, localDate, timezone string, taskIDs []string, available, expected int) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	var plan persistence.DailyPlan
	var resultItems []persistence.DailyPlanItem
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		// Store.Transaction can retry after SQLite busy rolls an attempt back.
		// Its in-memory snapshots must not retain clones from that failed attempt.
		resultItems = nil
		if err := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, localDate, timezone).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expected {
			return ErrRevision
		}
		if plan.Status != "active" {
			return ErrConflict
		}
		tasks, settings, err := eligiblePlanningTasksTx(tx, userID, taskIDs)
		if err != nil {
			return err
		}
		if len(taskIDs) > 0 && len(tasks) == 0 {
			return ErrValidation
		}
		resolvedAvailable, err := resolvePlanningBudget(available, settings)
		if err != nil {
			return err
		}
		var oldItems []persistence.DailyPlanItem
		if err := tx.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position, id").Find(&oldItems).Error; err != nil {
			return err
		}
		protected, err := protectedPlanItemsTx(tx, userID, plan, oldItems)
		if err != nil {
			return err
		}
		scopeTasks, err := planningTaskIndexTx(tx, userID)
		if err != nil {
			return err
		}
		protectedParents := protectedPlanParentIDs(oldItems, protected, scopeTasks)
		selectedScopes := map[string][]string{}
		now := persistence.Now()
		newRevision := plan.CurrentRevision + 1
		satisfiedTaskIDs := map[string]bool{}
		logicalByTask := map[string]string{}
		for _, item := range oldItems {
			if item.TaskID != nil {
				logicalByTask[*item.TaskID] = logicalItemID(item)
			}
			if protected[item.ID] {
				retained, err := retainProtectedItemForReplanTx(tx, userID, plan, item)
				if err != nil {
					return err
				}
				if !retained {
					continue // Original corrected row and its evidence stay in history.
				}
				copy := item
				copy.LogicalID = logicalItemID(item)
				copy.ID, copy.PlanRevision, copy.Revision, copy.CreatedAt, copy.UpdatedAt = persistence.NewID("dpi"), newRevision, 1, now, now
				resultItems = append(resultItems, copy)
				if copy.TaskID != nil {
					satisfiedTaskIDs[*copy.TaskID] = true
				}
			} else if err := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{"status": "superseded", "revision": item.Revision + 1, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		_, coreSlots, targetMinutes := remainingPlanBudget(resultItems, resolvedAvailable)
		candidates := make([]domain.Candidate, 0, len(tasks))
		byID := map[string]persistence.Task{}
		for _, task := range tasks {
			if satisfiedTaskIDs[task.ID] {
				continue
			}
			if len(taskIDs) == 0 && taskDescendsFrom(task, protectedParents, scopeTasks) {
				continue
			}
			byID[task.ID] = task
			candidates = append(candidates, domain.Candidate{ID: task.ID, Status: task.Status, Priority: task.Priority, Position: task.Position, Estimate: task.EstimateMinutes, MinimumAction: task.MinimumAction, GoalActive: true, DependencyDone: true})
		}
		selected := domain.SelectDailyCandidates(candidates, resolvedAvailable)
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
			if len(taskIDs) > 0 {
				if chain, valid := planningParentChain(task, scopeTasks); valid {
					selectedScopes[task.ID] = chain
				}
			}
			item := newScheduledItem(userID, plan.ID, newRevision, position, resolvedAvailable, task, now)
			item.TargetMinutes = targetMinutes
			item.Commitment = dailyTaskCommitment(task, targetMinutes)
			logicalID := logicalByTask[task.ID]
			if logicalID == "" {
				var err error
				logicalID, err = historicalLogicalItemIDTx(tx, userID, plan.ID, task.ID)
				if err != nil {
					return err
				}
			}
			if logicalID != "" {
				item.LogicalID = logicalID
			}
			resultItems = append(resultItems, item)
			position++
		}
		if len(resultItems) > 0 {
			if err := tx.Create(&resultItems).Error; err != nil {
				return err
			}
		}
		input, _ := json.Marshal(map[string]any{"task_ids": taskIDs, "available_minutes": resolvedAvailable, "algorithm": plan.AlgorithmVersion, "base_revision": expected, "selected_task_parent_chains": selectedScopes})
		snapshot, _ := json.Marshal(resultItems)
		revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: newRevision, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(snapshot), Reason: "same-day replanning", CreatedAt: now}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		freezePlanWithCore(&plan, now, hasCoreItem(resultItems))
		update := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, expected).Updates(map[string]any{"current_revision": newRevision, "revision": expected + 1, "status": "active", "updated_at": now, "frozen_at": plan.FrozenAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrRevision
		}
		if err := createOutbox(tx, userID, "daily_plan.replanned", map[string]any{"plan_id": plan.ID, "revision": newRevision}); err != nil {
			return err
		}
		// Keep the plan revision paired with the items built in this transaction.
		return tx.Where("id = ? AND user_id = ?", plan.ID, userID).First(&plan).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return &plan, resultItems, nil
}

func (a *PlanService) CloseDailyPlan(ctx context.Context, userID, planID string, expected int, status string) (*persistence.DailyPlan, error) {
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
		return tx.Where("id = ? AND user_id = ?", plan.ID, userID).First(&plan).Error
	})
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func (a *GoalService) CompleteTask(ctx context.Context, userID, id string, expected int, eventType, summary string, reopen bool) (*persistence.Task, error) {
	var task persistence.Task
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := a.completeTaskTx(tx, userID, id, expected, eventType, summary, reopen, "[]", &task); err != nil {
			return err
		}
		// Return this mutation's committed version, not a later opposite write
		// that may arrive before the HTTP response and its idempotency cache.
		return tx.Where("id = ? AND user_id = ?", id, userID).First(&task).Error
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// completeTaskTx is shared by the standalone task endpoint and an explicit
// daily result that also delivers the entire leaf task. Callers compose this in
// their existing transaction so neither evidence layer can be partially saved.
func (a *GoalService) completeTaskTx(tx *gorm.DB, userID, id string, expected int, eventType, summary string, reopen bool, evidenceJSON string, task *persistence.Task) error {
	live, err := RequireLiveTaskTx(tx, userID, id)
	if err != nil {
		return err
	}
	*task = *live
	if task.Revision != expected {
		return ErrRevision
	}
	status := "completed"
	if reopen {
		if task.Status != "completed" {
			return ErrConflict
		}
		status, eventType = "in_progress", "task_reopened"
	} else {
		if strings.TrimSpace(summary) == "" || task.SuccessCriteria == "" || (eventType != "result" && eventType != "step") {
			return ErrValidation
		}
		if task.Status == "completed" || task.Status == "cancelled" || task.Status == "superseded" {
			return ErrConflict
		}
		eligible, err := taskExecutableTx(tx, userID, id)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrConflict
		}
	}
	now := persistence.Now()
	if result := tx.Model(&persistence.Task{}).Where("id = ? AND revision = ?", id, expected).Updates(map[string]any{"status": status, "revision": expected + 1, "updated_at": now}); result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return result.Error
		}
		return ErrRevision
	}
	event := persistence.ProgressEvent{ID: persistence.NewID("progress"), UserID: userID, GoalID: &task.GoalID, TaskID: &task.ID, Type: eventType, Summary: summary, EvidenceJSON: evidenceJSON, OccurredAt: now, CreatedAt: now}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	if err := a.snapshotTaskTree(tx, userID, task.GoalID, eventType, "user"); err != nil {
		return err
	}
	if !reopen {
		subtree, err := subtreeTasksTx(tx, userID, *task)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(subtree))
		for _, descendant := range subtree {
			ids = append(ids, descendant.ID)
		}
		if err := releaseTodayItemsTx(tx, userID, ids, now); err != nil {
			return err
		}
	}
	if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
		return err
	}
	return createOutbox(tx, userID, "task.status_changed", map[string]any{"task_id": id, "status": status})
}

func (a *PlanService) CreateDailyPlan(ctx context.Context, userID, localDate, timezone string, taskIDs []string, available int) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	if err := ParseDateInZone(localDate, timezone); err != nil {
		return nil, nil, err
	}
	var plan persistence.DailyPlan
	var items []persistence.DailyPlanItem
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		tasks, settings, err := eligiblePlanningTasksTx(tx, userID, taskIDs)
		if err != nil {
			return err
		}
		if len(taskIDs) > 0 && len(tasks) == 0 {
			return ErrValidation
		}
		resolvedAvailable, err := resolvePlanningBudget(available, settings)
		if err != nil {
			return err
		}
		byID := map[string]persistence.Task{}
		for _, task := range tasks {
			byID[task.ID] = task
		}
		selected := domain.SelectDailyCandidates(candidatesForTasks(tasks), resolvedAvailable)
		if slots := planningSlots(resolvedAvailable); len(selected) > slots {
			selected = selected[:slots]
		}
		now := persistence.Now()
		plan = persistence.DailyPlan{ID: persistence.NewID("plan"), UserID: userID, LocalDate: localDate, Timezone: timezone, Status: "active", AlgorithmVersion: "deterministic-v1", CurrentRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
		loc, _ := time.LoadLocation(timezone)
		if localDate <= now.In(loc).Format("2006-01-02") && len(selected) > 0 {
			plan.FrozenAt = &now
		}
		items = make([]persistence.DailyPlanItem, 0, len(selected))
		for position, candidate := range selected {
			items = append(items, newScheduledItem(userID, plan.ID, 1, position+1, resolvedAvailable, byID[candidate.ID], now))
		}
		input, _ := json.Marshal(map[string]any{"task_ids": taskIDs, "available_minutes": resolvedAvailable, "algorithm": plan.AlgorithmVersion})
		planJSON, _ := json.Marshal(items)
		revision := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: 1, InputSnapshot: string(input), InputHash: persistence.Hash(string(input)), PlanSnapshot: string(planJSON), Reason: "initial generation", CreatedAt: now}
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

func (a *PlanService) AddPlanItem(ctx context.Context, userID, planID string, expectedPlan int, item *persistence.DailyPlanItem) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var plan persistence.DailyPlan
		if err := tx.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
			return notFound(err)
		}
		if plan.Revision != expectedPlan {
			return ErrRevision
		}
		if plan.Status != "active" {
			return ErrConflict
		}
		var existingTask int64
		if item.Kind == "core" && item.TaskID == nil {
			return ErrValidation
		}
		if item.TaskID != nil {
			if _, err := RequireLiveTaskTx(tx, userID, *item.TaskID); err != nil {
				return err
			}
			if item.Kind == "core" {
				eligible, _, err := eligiblePlanningTasksTx(tx, userID, []string{*item.TaskID})
				if err != nil {
					return err
				}
				if len(eligible) != 1 {
					return ErrValidation
				}
			}
			if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND task_id = ? AND status != 'superseded'", planID, plan.CurrentRevision, *item.TaskID).Count(&existingTask).Error; err != nil {
				return err
			}
			if existingTask > 0 {
				return ErrConflict
			}
		}
		item.Title = strings.TrimSpace(item.Title)
		item.Commitment = strings.TrimSpace(item.Commitment)
		item.MinimumAction = strings.TrimSpace(item.MinimumAction)
		if item.Kind != "core" && item.Kind != "support" && item.Kind != "input" {
			return ErrValidation
		}
		if item.Title == "" || item.Commitment == "" || item.MinimumAction == "" || item.TargetMinutes < 1 || item.TargetMinutes > 480 {
			return ErrValidation
		}
		var core int64
		if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ? AND kind = 'core' AND status NOT IN ('superseded','skipped')", planID, plan.CurrentRevision).Count(&core).Error; err != nil {
			return err
		}
		if item.Kind == "core" {
			settings, err := planningSettingsTx(tx, userID)
			if err != nil {
				return err
			}
			if core >= int64(planningSlots(settings.DailyBudgetMinutes)) {
				return domain.ErrCoreLimit
			}
		}
		now := persistence.Now()
		item.ID, item.UserID, item.PlanID, item.PlanRevision = persistence.NewID("dpi"), userID, planID, plan.CurrentRevision
		item.LogicalID = item.ID
		if item.TaskID != nil {
			logicalID, err := historicalLogicalItemIDTx(tx, userID, planID, *item.TaskID)
			if err != nil {
				return err
			}
			if logicalID != "" {
				item.LogicalID = logicalID
			}
		}
		item.Status, item.Revision, item.CreatedAt, item.UpdatedAt = "planned", 1, now, now
		if item.AllowedTypes == "" {
			item.AllowedTypes = "result,step,time,minimum_action"
		}
		if err := tx.Create(item).Error; err != nil {
			return err
		}
		freezePlanWithCore(&plan, now, item.Kind == "core")
		result := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", planID, expectedPlan).Updates(map[string]any{"revision": expectedPlan + 1, "updated_at": now, "frozen_at": plan.FrozenAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
}

// satisfyItemTx marks a plan item satisfied inside the caller's transaction. It is
// the PlanService domain function that ProgressService.CompletePlanItem composes
// through the TxManager port (wiring.md §4.1: Progress writes the event, Plan
// changes the item status, and both commit in one transaction).
func (a *PlanService) satisfyItemTx(tx *gorm.DB, itemID string, expected int, completionType, summary string, now time.Time) error {
	result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", itemID, expected).Updates(map[string]any{"status": "satisfied", "completion_type": completionType, "completion_summary": summary, "revision": expected + 1, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRevision
	}
	return nil
}

func (a *ProgressService) CompletePlanItem(ctx context.Context, userID, itemID string, expected int, completionType, summary string, sessionIDs []string, planIDs ...string) (*persistence.DailyPlanItem, error) {
	command := PlanItemCompletionCommand{ItemID: itemID, Expected: expected, Type: completionType, Summary: summary, WorkSessionIDs: sessionIDs}
	if len(planIDs) > 0 {
		command.PlanID = planIDs[0]
	}
	return a.CompletePlanItemWithTask(ctx, userID, command)
}

func (a *ProgressService) CompletePlanItemWithTask(ctx context.Context, userID string, command PlanItemCompletionCommand) (*persistence.DailyPlanItem, error) {
	itemID, expected, completionType, summary, sessionIDs := command.ItemID, command.Expected, command.Type, command.Summary, command.WorkSessionIDs
	var item persistence.DailyPlanItem
	err := a.Store.WithTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", itemID, userID).First(&item).Error; err != nil {
			return notFound(err)
		}
		if item.Revision != expected {
			return ErrRevision
		}
		if command.PlanID != "" && item.PlanID != command.PlanID {
			return ErrNotFound
		}
		if err := actionablePlanItem(tx, userID, item); err != nil {
			return err
		}
		if item.Status == "satisfied" {
			if command.CompleteTask {
				return ErrConflict
			}
			return nil
		}
		if item.Status != "planned" && item.Status != "in_progress" {
			return ErrConflict
		}
		if item.TaskID != nil {
			eligible, err := taskExecutableTx(tx, userID, *item.TaskID)
			if err != nil {
				return err
			}
			if !eligible {
				return ErrConflict
			}
		}
		if !domain.ValidateCompletionType(completionType, item.AllowedTypes) {
			return ErrValidation
		}
		if completionType == "time" {
			var total int
			var err error
			sessionIDs, total, err = completionSessions(tx, userID, item, sessionIDs)
			if err != nil {
				return err
			}
			if total < item.TargetMinutes*60 {
				return ErrValidation
			}
		} else {
			sessionIDs = nil
		}
		if strings.TrimSpace(summary) == "" {
			return ErrValidation
		}
		var entireTask *persistence.Task
		if command.CompleteTask {
			if completionType != "result" || item.TaskID == nil {
				return ErrValidation
			}
			var err error
			entireTask, err = RequireLiveTaskTx(tx, userID, *item.TaskID)
			if err != nil {
				return err
			}
			if command.ExpectedTask <= 0 {
				return ErrPrecondition
			}
			if entireTask.Revision != command.ExpectedTask {
				return ErrRevision
			}
			eligible, err := completeTaskFromDailyEligibleTx(tx, userID, *entireTask)
			if err != nil {
				return err
			}
			if !eligible {
				return ErrConflict
			}
		}
		now := persistence.Now()
		// §4.1: the plan-item status change is a PlanService domain function; this
		// ProgressService use case composes it inside the SAME transaction (opened
		// via the TxManager port) so the event write and the status change are atomic.
		if err := a.Plans.satisfyItemTx(tx, itemID, expected, completionType, summary, now); err != nil {
			return err
		}
		evidence, _ := json.Marshal(planItemEvidence{LogicalID: logicalPlanItemID(item), WorkSessionIDs: sessionIDs})
		event := persistence.ProgressEvent{ID: persistence.NewID("progress"), UserID: userID, TaskID: item.TaskID, PlanItemID: &item.ID, Type: completionType, Summary: strings.TrimSpace(summary), EvidenceJSON: string(evidence), OccurredAt: now, CreatedAt: now}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		if err := touchPlanTx(tx, item.PlanID, now); err != nil {
			return err
		}
		if entireTask != nil {
			association, _ := json.Marshal(map[string]string{"daily_plan_item_id": item.ID, "daily_completion_event_id": event.ID, "logical_id": logicalPlanItemID(item)})
			goals := NewGoalService(a.Store)
			if err := goals.completeTaskTx(tx, userID, entireTask.ID, command.ExpectedTask, "result", strings.TrimSpace(summary), false, string(association), entireTask); err != nil {
				return err
			}
		}
		if err := createOutbox(tx, userID, "daily_item.satisfied", map[string]any{"item_id": item.ID}); err != nil {
			return err
		}
		// Joint completion can also refresh the plan snapshot. Capture all of
		// this transaction's final item fields before another caller can undo it.
		return tx.Where("id = ? AND user_id = ?", itemID, userID).First(&item).Error
	})
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (a *PlanService) UpdatePlanItem(ctx context.Context, userID, planID, itemID string, expected int, changes map[string]any) (*persistence.DailyPlanItem, error) {
	var item persistence.DailyPlanItem
	if value, exists := changes["status"]; exists {
		status, ok := value.(string)
		if !ok || status != "skipped" {
			return nil, ErrValidation
		}
	}
	updates := make(map[string]any, len(changes)+2)
	for key, value := range changes {
		switch key {
		case "title", "commitment", "minimum_action", "target_minutes", "position", "status":
			updates[key] = value
		default:
			return nil, ErrValidation
		}
	}
	if _, exists := updates["target_minutes"]; exists {
		var minutes int
		if err := changeInt(updates, "target_minutes", &minutes); err != nil || minutes < 1 || minutes > 480 {
			return nil, ErrValidation
		}
		updates["target_minutes"] = minutes
	}
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
		if item.Status == "satisfied" {
			// Completion corrections must retain a reversal event.
			return ErrConflict
		}
		// Validate only submitted fields; older snapshots can be corrected one field
		// at a time, and skipping must not rewrite unrelated historical values.
		for _, key := range []string{"title", "commitment", "minimum_action"} {
			if _, exists := updates[key]; !exists {
				continue
			}
			var text string
			if err := changeText(updates, key, &text); err != nil || text == "" {
				return ErrValidation
			}
			updates[key] = text
		}
		if _, exists := updates["position"]; exists {
			var position int
			if err := changeInt(updates, "position", &position); err != nil || position < 0 {
				return ErrValidation
			}
			updates["position"] = position
		}
		now := persistence.Now()
		updates["revision"], updates["updated_at"] = expected+1, now
		result := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", itemID, expected).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return touchPlanTx(tx, planID, now)
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&item, "id = ?", itemID).Error; err != nil {
		return nil, err
	}
	return &item, nil
}
func (a *ProgressService) StartSession(ctx context.Context, userID string, session *persistence.WorkSession) error {
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		executable, err := taskExecutableTx(tx, userID, session.TaskID)
		if err != nil {
			return err
		}
		if !executable {
			return ErrConflict
		}
		if session.DailyPlanItemID != nil {
			var item persistence.DailyPlanItem
			if err := tx.Where("id = ? AND user_id = ?", *session.DailyPlanItemID, userID).First(&item).Error; err != nil {
				return notFound(err)
			}
			if item.TaskID == nil || *item.TaskID != session.TaskID {
				return ErrValidation
			}
			if err := actionablePlanItem(tx, userID, item); err != nil {
				return err
			}
			if item.Status != "planned" && item.Status != "in_progress" {
				return ErrConflict
			}
			session.DailyPlanItemLogicalID = logicalPlanItemID(item)
		}
		now := persistence.Now()
		if session.StartedAt.IsZero() {
			session.StartedAt = now
		}
		if session.StartedAt.After(now.Add(5 * time.Second)) {
			return ErrValidation
		}
		session.ID, session.UserID, session.Status, session.Revision = persistence.NewID("work"), userID, "running", 1
		session.LastTransitionAt = &session.StartedAt
		session.CreatedAt, session.UpdatedAt = now, now
		if session.SessionType == "" {
			session.SessionType = "pomodoro"
		}
		if session.TargetMinutes == 0 {
			session.TargetMinutes = 25
		}
		if session.TargetMinutes < 1 || session.TargetMinutes > 480 {
			return ErrValidation
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

func (a *ProgressService) TransitionSession(ctx context.Context, userID, id string, expected int, action, outcome, note string, at time.Time) (*persistence.WorkSession, error) {
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
		if at.After(persistence.Now().Add(5*time.Second)) || at.Before(session.StartedAt) || (session.PausedAt != nil && at.Before(*session.PausedAt)) || (session.LastTransitionAt != nil && at.Before(*session.LastTransitionAt)) {
			return ErrValidation
		}
		// updated_at is processing time; it cannot order historical backfills.
		// Keep the last accepted business time even when resume clears paused_at.
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now(), "last_transition_at": at}
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
		if action == "invalidate" {
			return a.invalidateSessionCompletionTx(tx, userID, session)
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

func (a *GoalService) RejectProposal(ctx context.Context, userID, goalID, proposalID string, expected int) (*persistence.Proposal, error) {
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

func (a *GoalService) ApplyProposal(ctx context.Context, userID string, proposal *persistence.Proposal, patches []map[string]any, expectedTreeRevision ...int) (int, error) {
	created := 0
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		created, err = a.applyProposalTx(tx, userID, proposal, patches, expectedTreeRevision...)
		return err
	})
	return created, err
}

// applyProposalTx also serves new-goal approval so the goal, tree and approval
// land in one transaction; no partially created research workspace can escape.
func (a *GoalService) applyProposalTx(tx *gorm.DB, userID string, proposal *persistence.Proposal, patches []map[string]any, expectedTreeRevision ...int) (int, error) {
	created := 0
	err := func() error {
		var current persistence.Proposal
		if err := tx.Where("id = ? AND user_id = ? AND status = 'pending'", proposal.ID, userID).First(&current).Error; err != nil {
			return notFound(err)
		}
		goal, err := RequireLiveGoalTx(tx, userID, current.GoalID)
		if err != nil {
			return err
		}
		if goal.Status != "active" {
			return ErrConflict
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
		byID := map[string]persistence.Task{}
		parents := map[string]string{}
		loadLiveTree := func() error {
			var live []persistence.Task
			if err := tx.Where("goal_id = ? AND user_id = ? AND deleted_at IS NULL", current.GoalID, userID).Find(&live).Error; err != nil {
				return err
			}
			byID, parents = map[string]persistence.Task{}, map[string]string{}
			for _, task := range live {
				byID[task.ID] = task
				if task.ParentID != nil {
					parents[task.ID] = *task.ParentID
				}
			}
			return nil
		}
		if err := loadLiveTree(); err != nil {
			return err
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
				if (task.Type != "milestone" && task.Type != "task" && task.Type != "action") || task.Title == "" || task.SuccessCriteria == "" || task.MinimumAction == "" {
					return ErrValidation
				}
				if priority, specified := patch["priority"]; !specified || priority == nil {
					task.Priority = 50
				}
				if task.EstimateMinutes == 0 {
					task.EstimateMinutes = 25
				}
				if task.Priority < 0 || task.Priority > 100 || task.EstimateMinutes < 1 || task.EstimateMinutes > 1440 || task.Position < 0 {
					return ErrValidation
				}
				if err := validateTaskFields(&task); err != nil {
					return err
				}
				parent := textValue(patch["parent_id"])
				if parent == "" {
					if ref := textValue(patch["parent_ref"]); ref != "" {
						var found bool
						parent, found = refs[ref]
						if !found {
							return ErrValidation
						}
					}
				}
				if parent != "" {
					if _, ok := byID[parent]; !ok {
						return ErrValidation
					}
					parentTask, err := RequireLiveTaskTx(tx, userID, parent)
					if err != nil {
						return err
					}
					if parentTask.Status == "completed" || parentTask.Status == "cancelled" || parentTask.Status == "superseded" {
						return ErrValidation
					}
					task.ParentID = &parent
					parents[task.ID] = parent
				}
				dependencies, _, err := taskDependenciesFromPatch(patch, refs)
				if err != nil {
					return err
				}
				task.DependsOn = dependencies
				if err := validateTaskDependenciesTx(tx, task); err != nil {
					return err
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
			case "delete", "restore":
				targetID := textValue(patch["target_id"])
				var task persistence.Task
				if err := tx.Where("id = ? AND user_id = ? AND goal_id = ?", targetID, userID, current.GoalID).First(&task).Error; err != nil {
					return notFound(err)
				}
				if op == "delete" {
					if _, err := a.deleteTaskTx(tx, userID, targetID, task.Revision); err != nil {
						return err
					}
				} else {
					_, err := a.restoreTaskTx(tx, userID, targetID, task.Revision)
					if err != nil {
						return err
					}
				}
				// Lifecycle operations affect a whole deletion batch. Later ordered
				// operations must see every restored/removed descendant and parent.
				if err := loadLiveTree(); err != nil {
					return err
				}
			case "update", "move", "supersede":
				targetID := textValue(patch["target_id"])
				task, ok := byID[targetID]
				if !ok {
					return ErrValidation
				}
				if _, err := RequireLiveTaskTx(tx, userID, targetID); err != nil {
					return err
				}
				updates := map[string]any{"revision": task.Revision + 1, "updated_at": now}
				if op == "supersede" {
					updates["status"] = "superseded"
				} else {
					for _, field := range []string{"type", "title", "description", "success_criteria", "minimum_action", "blocked_reason"} {
						if value, exists := patch[field]; exists {
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
							value := 0
							if err := changeInt(patch, field, &value); err != nil {
								return err
							}
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
					_, hasParent := patch["parent_id"]
					_, hasParentRef := patch["parent_ref"]
					if op == "move" && !hasParent && !hasParentRef {
						return ErrValidation
					}
					if op == "move" || hasParent || hasParentRef {
						parent := textValue(patch["parent_id"])
						if parent == "" {
							if ref := textValue(patch["parent_ref"]); ref != "" {
								var found bool
								parent, found = refs[ref]
								if !found {
									return ErrValidation
								}
							}
						}
						if parent != "" {
							if _, ok := byID[parent]; !ok || domain.WouldCreateCycle(targetID, parent, parents) {
								return ErrValidation
							}
							if _, err := RequireLiveTaskTx(tx, userID, parent); err != nil {
								return err
							}
							updates["parent_id"] = parent
							parents[targetID] = parent
						} else {
							updates["parent_id"] = nil
							delete(parents, targetID)
						}
					}
					if dependencies, changed, err := taskDependenciesFromPatch(patch, refs); err != nil {
						return err
					} else if changed {
						updates["depends_on"] = dependencies
					}
				}
				delete(updates, "revision")
				delete(updates, "updated_at")
				updated, err := a.updateTaskTx(tx, userID, targetID, task.Revision, updates, true)
				if err != nil {
					return err
				}
				byID[targetID] = *updated
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
		if err := a.snapshotTaskTree(tx, userID, current.GoalID, "agent proposal applied", "agent"); err != nil {
			return err
		}
		if err := refreshLifecycleScheduleTx(tx, userID, now); err != nil {
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
	}()
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

func (a *GoalService) snapshotTaskTree(tx *gorm.DB, userID, goalID, reason, source string) error {
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

func (a *GoalService) createTaskTx(tx *gorm.DB, userID string, task *persistence.Task, reason, source string) error {
	return a.createTaskWithPriorityTx(tx, userID, task, reason, source, false)
}

func (a *GoalService) createTaskWithPriorityTx(tx *gorm.DB, userID string, task *persistence.Task, reason, source string, explicitPriority bool) error {
	goal, err := RequireLiveGoalTx(tx, userID, task.GoalID)
	if err != nil {
		return err
	}
	if goal.Status != "active" && goal.Status != "paused" {
		return ErrConflict
	}
	if task.ParentID != nil {
		parent, err := RequireLiveTaskTx(tx, userID, *task.ParentID)
		if err != nil || parent.GoalID != task.GoalID || parent.Status == "completed" || parent.Status == "cancelled" || parent.Status == "superseded" {
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
	if task.Priority == 0 && !explicitPriority {
		task.Priority = 50
	}
	if task.EstimateMinutes == 0 {
		task.EstimateMinutes = 25
	}
	if err := validateTaskFields(task); err != nil {
		return err
	}
	now := persistence.Now()
	task.ID, task.UserID, task.Status, task.Revision = persistence.NewID("task"), userID, "ready", 1
	task.DeletedAt, task.DeletionID = nil, ""
	task.CreatedAt, task.UpdatedAt = now, now
	if task.DependsOn == nil {
		task.DependsOn = []string{}
	}
	if err := validateTaskDependenciesTx(tx, *task); err != nil {
		return err
	}
	if err := tx.Create(task).Error; err != nil {
		return err
	}
	return a.snapshotTaskTree(tx, userID, task.GoalID, reason, source)
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
