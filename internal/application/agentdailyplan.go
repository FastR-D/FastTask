package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// dailyPlanAgentSchema versions the agent's daily-plan input snapshot so the
// deterministic commitment is reproducible against a known schema (arch.md §9.2:
// the snapshot "必须包含 Agent 分析结果、schema 版本和内容哈希").
const dailyPlanAgentSchema = "daily-plan-agent-v3"

// PlanCandidate is one task the agent recommends for today's plan, with the
// concrete 5-15 minute first step and the reason it belongs (agent.md §6). The
// agent supplies candidates and rationale; the PROGRAM owns the final filter,
// sort, dedup and top-3 cap (arch.md §9.2). A candidate missing its minimum
// action is invalid and rejected by the tool before it ever reaches here.
type PlanCandidate struct {
	TaskID        string `json:"task_id"`
	MinimumAction string `json:"minimum_action"`
	Commitment    string `json:"commitment,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// dailyPlanPayload is the Proposal.PatchJSON envelope staged by propose_daily_plan
// and replayed by ApplyDailyPlanProposal once the user approves.
type dailyPlanPayload struct {
	LocalDate        string          `json:"local_date"`
	Timezone         string          `json:"timezone"`
	AvailableMinutes int             `json:"available_minutes"`
	Candidates       []PlanCandidate `json:"candidates"`
	BasePlan         *dailyPlanBase  `json:"base_plan,omitempty"`
}

// A non-nil empty baseline records that there was no plan when the user saw the
// proposal. Nil means an older proposal without concurrency protection.
type dailyPlanBase struct {
	PlanID   string `json:"plan_id"`
	Revision int    `json:"revision"`
}

// ApplyDailyPlanProposal turns an approved propose_daily_plan proposal into a
// daily plan, creating one for the date or replanning the existing plan.
//
// The deterministic guarantee is preserved end to end: the agent's candidates are
// fed through domain.SelectDailyCandidates, which filters ineligible tasks, sorts
// by priority and caps the result at three core items. Proposing five candidates
// can never make five land (§6, acceptance "确定性取三不被绕过"). Tasks already
// satisfied today are excluded so replanning does not duplicate them.
//
// The agent's candidates, minimum actions and reasons are folded into the
// revision input snapshot and covered by its hash, so the same input reproduces
// the same plan (arch.md §9.2).
func (a *App) ApplyDailyPlanProposal(ctx context.Context, userID string, proposal *persistence.Proposal, payload dailyPlanPayload) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	if len(payload.Candidates) == 0 {
		return nil, nil, fmt.Errorf("%w: proposal has no candidates", ErrValidation)
	}
	if payload.AvailableMinutes != 0 && (payload.AvailableMinutes < 30 || payload.AvailableMinutes > 480) {
		return nil, nil, fmt.Errorf("%w: available_minutes out of range", ErrValidation)
	}
	tzName := strings.TrimSpace(payload.Timezone)
	if tzName == "" {
		var user persistence.User
		if err := a.Store.DB.WithContext(ctx).First(&user, "id = ?", userID).Error; err != nil {
			return nil, nil, notFound(err)
		}
		_, tzName = resolveZone(user.Timezone)
	}
	location, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid timezone", ErrValidation)
	}
	localDate := strings.TrimSpace(payload.LocalDate)
	if localDate == "" {
		localDate = persistence.Now().In(location).Format("2006-01-02")
	} else if _, err := time.ParseInLocation("2006-01-02", localDate, location); err != nil {
		return nil, nil, fmt.Errorf("%w: invalid local_date", ErrValidation)
	}

	// Load the candidate tasks (ownership + active goal). The agent's per-candidate
	// minimum action overrides the task's for eligibility and rendering, but the
	// deterministic filter still runs on the task's own status/priority/estimate.
	taskIDs := make([]string, 0, len(payload.Candidates))
	agentByTask := make(map[string]PlanCandidate, len(payload.Candidates))
	for _, candidate := range payload.Candidates {
		taskIDs = append(taskIDs, candidate.TaskID)
		agentByTask[candidate.TaskID] = candidate
	}
	now := persistence.Now()
	var plan persistence.DailyPlan
	var resultItems []persistence.DailyPlanItem
	err = a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		// A busy rollback can retry this closure. Only items built by the
		// successful attempt may escape; prior IDs and protected copies are stale.
		resultItems = nil
		// Approval can arrive after a task was blocked, deleted or moved off the
		// primary goal. Recheck inside the write transaction before replacing any
		// existing commitments; an obsolete proposal must not clear the day.
		tasks, settings, err := eligiblePlanningTasksTx(tx, userID, taskIDs)
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			return fmt.Errorf("%w: no executable candidates remain", ErrValidation)
		}
		resolvedAvailable, err := resolvePlanningBudget(payload.AvailableMinutes, settings)
		if err != nil {
			return err
		}
		byID := make(map[string]persistence.Task, len(tasks))
		for _, task := range tasks {
			byID[task.ID] = task
		}
		inputSnapshot := map[string]any{
			"source": "agent_proposal", "schema_version": dailyPlanAgentSchema,
			"local_date": localDate, "timezone": location.String(),
			"available_minutes": resolvedAvailable,
			"candidates":        payload.Candidates, "algorithm": "deterministic-v1",
			"base_plan": payload.BasePlan,
		}
		scopeTasks, err := planningTaskIndexTx(tx, userID)
		if err != nil {
			return err
		}
		selectedScopes := map[string][]string{}
		var existing persistence.DailyPlan
		lookup := tx.Where("user_id = ? AND local_date = ? AND timezone = ?", userID, localDate, location.String()).First(&existing).Error
		if lookup != nil && !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return lookup
		}
		replan := lookup == nil
		if replan && existing.Status != "active" {
			return ErrConflict
		}
		if payload.BasePlan == nil {
			if proposal != nil {
				return fmt.Errorf("%w: daily plan proposal has no baseline; read the current plan and propose again", ErrRevision)
			}
		} else if (!replan && (payload.BasePlan.PlanID != "" || payload.BasePlan.Revision != 0)) ||
			(replan && (payload.BasePlan.PlanID != existing.ID || payload.BasePlan.Revision != existing.Revision)) {
			return fmt.Errorf("%w: daily plan changed since this proposal; read the current plan and propose again", ErrRevision)
		}
		newRevision := 1
		retainedTaskIDs := map[string]bool{}
		logicalByTask := map[string]string{}
		if replan {
			newRevision = existing.CurrentRevision + 1
			var oldItems []persistence.DailyPlanItem
			if err := tx.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", existing.ID, existing.CurrentRevision).Order("position, id").Find(&oldItems).Error; err != nil {
				return err
			}
			protected, err := protectedPlanItemsTx(tx, userID, existing, oldItems)
			if err != nil {
				return err
			}
			for _, item := range oldItems {
				if item.TaskID != nil {
					logicalByTask[*item.TaskID] = logicalItemID(item)
				}
				if protected[item.ID] {
					retained, err := retainProtectedItemForReplanTx(tx, userID, existing, item)
					if err != nil {
						return err
					}
					if !retained {
						continue // Preserve the corrected row in its original revision.
					}
					keep := item
					keep.LogicalID = logicalItemID(item)
					keep.ID, keep.PlanRevision, keep.Revision, keep.CreatedAt, keep.UpdatedAt = persistence.NewID("dpi"), newRevision, 1, now, now
					resultItems = append(resultItems, keep)
					if keep.TaskID != nil {
						retainedTaskIDs[*keep.TaskID] = true
					}
				} else if err := tx.Model(&persistence.DailyPlanItem{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{"status": "superseded", "revision": item.Revision + 1, "updated_at": now}).Error; err != nil {
					return err
				}
			}
		}
		_, coreSlots, targetMinutes := remainingPlanBudget(resultItems, resolvedAvailable)
		// Resolve the target plan id up front so items built below always carry it,
		// whether we replan the existing row or create a fresh one.
		planID := existing.ID
		if !replan {
			planID = persistence.NewID("plan")
		}

		// Deterministic selection over the agent's candidates, excluding anything
		// already satisfied today. This is the single chokepoint that enforces the
		// top-3 cap; the agent cannot bypass it.
		candidates := make([]domain.Candidate, 0, len(tasks))
		for _, task := range tasks {
			if retainedTaskIDs[task.ID] {
				continue
			}
			minAction := task.MinimumAction
			if agent, ok := agentByTask[task.ID]; ok && agent.MinimumAction != "" {
				minAction = agent.MinimumAction
			}
			candidates = append(candidates, domain.Candidate{
				ID: task.ID, Status: task.Status, Priority: task.Priority, Position: task.Position,
				Estimate: task.EstimateMinutes, MinimumAction: minAction,
				GoalActive: true, DependencyDone: true,
			})
		}
		selected := domain.SelectDailyCandidates(candidates, resolvedAvailable)
		if len(selected) > coreSlots {
			selected = selected[:coreSlots]
		}

		position := 1
		for _, kept := range resultItems {
			if kept.Kind == "core" && kept.Position >= position {
				position = kept.Position + 1
			}
		}
		for _, candidate := range selected {
			task := byID[candidate.ID]
			if chain, valid := planningParentChain(task, scopeTasks); valid {
				selectedScopes[task.ID] = chain
			}
			item := buildAgentPlanItem(userID, planID, newRevision, position, task, agentByTask[task.ID], now)
			item.TargetMinutes = targetMinutes
			logicalID := logicalByTask[task.ID]
			if logicalID == "" {
				var err error
				logicalID, err = historicalLogicalItemIDTx(tx, userID, planID, task.ID)
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

		inputSnapshot["selected_task_parent_chains"] = selectedScopes
		input, _ := json.Marshal(inputSnapshot)
		inputHash := persistence.Hash(string(input))
		if replan {
			plan = existing
			plan.CurrentRevision, plan.Revision, plan.Status, plan.UpdatedAt = newRevision, existing.Revision+1, "active", now
			freezePlanWithCore(&plan, now, hasCoreItem(resultItems))
			if len(resultItems) > 0 {
				if err := tx.Create(&resultItems).Error; err != nil {
					return err
				}
			}
			snapshot, _ := json.Marshal(resultItems)
			rev := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: existing.ID, Revision: newRevision, InputSnapshot: string(input), InputHash: inputHash, PlanSnapshot: string(snapshot), Reason: "agent proposal replan", CreatedAt: now}
			if err := tx.Create(&rev).Error; err != nil {
				return err
			}
			update := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", existing.ID, existing.Revision).Updates(map[string]any{"current_revision": newRevision, "revision": existing.Revision + 1, "status": "active", "updated_at": now, "frozen_at": plan.FrozenAt})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrRevision
			}
			if err := markProposalApplied(tx, proposal, now); err != nil {
				return err
			}
			return createOutbox(tx, userID, "daily_plan.replanned", map[string]any{"plan_id": existing.ID, "revision": newRevision})
		}

		plan = persistence.DailyPlan{ID: planID, UserID: userID, LocalDate: localDate, Timezone: location.String(), Status: "active", AlgorithmVersion: "deterministic-v1", CurrentRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if localDate <= now.In(location).Format("2006-01-02") && len(resultItems) > 0 {
			plan.FrozenAt = &now
		}
		if err := tx.Create(&plan).Error; err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrConflict
			}
			return err
		}
		if len(resultItems) > 0 {
			if err := tx.Create(&resultItems).Error; err != nil {
				return err
			}
		}
		snapshot, _ := json.Marshal(resultItems)
		rev := persistence.DailyPlanRevision{ID: persistence.NewID("planrev"), PlanID: plan.ID, Revision: 1, InputSnapshot: string(input), InputHash: inputHash, PlanSnapshot: string(snapshot), Reason: "agent proposal", CreatedAt: now}
		if err := tx.Create(&rev).Error; err != nil {
			return err
		}
		if err := markProposalApplied(tx, proposal, now); err != nil {
			return err
		}
		return createOutbox(tx, userID, "daily_plan.created", map[string]any{"plan_id": plan.ID})
	})
	if err != nil {
		return nil, nil, err
	}
	return &plan, resultItems, nil
}

// markProposalApplied flips a pending proposal to applied inside the apply
// transaction, mirroring ApplyProposal so the daily-plan path records the same
// terminal state atomically with the plan write. A concurrent resolution (row no
// longer pending at this revision) yields ErrRevision rather than a double apply.
func markProposalApplied(tx *gorm.DB, proposal *persistence.Proposal, now time.Time) error {
	if proposal == nil {
		return nil
	}
	result := tx.Model(&persistence.Proposal{}).
		Where("id = ? AND revision = ? AND status = 'pending'", proposal.ID, proposal.Revision).
		Updates(map[string]any{"status": "applied", "revision": proposal.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRevision
	}
	return nil
}

// buildAgentPlanItem renders a daily slice. Older proposals without an explicit
// commitment conservatively promise only their approved minimum action.
func buildAgentPlanItem(userID, planID string, planRevision, position int, task persistence.Task, agent PlanCandidate, now time.Time) persistence.DailyPlanItem {
	itemID := persistence.NewID("dpi")
	taskID := task.ID
	minAction := task.MinimumAction
	if agent.MinimumAction != "" {
		minAction = agent.MinimumAction
	}
	commitment := strings.TrimSpace(agent.Commitment)
	if commitment == "" {
		commitment = minAction
	}
	return persistence.DailyPlanItem{
		ID: itemID, LogicalID: itemID, UserID: userID, PlanID: planID, PlanRevision: planRevision,
		TaskID: &taskID, Kind: "core", Title: task.Title, Commitment: commitment,
		MinimumAction: minAction, AllowedTypes: "result,step,time,minimum_action",
		TargetMinutes: planningTargetMinutes(defaultDailyBudget), Status: "planned", Position: position, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}
