package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type goalWithTasksPayload struct {
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	SuccessCriteria string           `json:"success_criteria"`
	ShortfallReason string           `json:"shortfall_reason,omitempty"`
	Nodes           []map[string]any `json:"nodes"`
}

type goalUpdatePayload struct {
	Changes map[string]any `json:"changes"`
}

func newGoalWithTasksTool(app *App) Tool {
	return proposalTool{app: app, kind: "goal_with_tasks", def: agent.ToolDefinition{
		Name:        "propose_goal_with_tasks",
		Description: "Propose a new research goal with concrete, distinct work steps, including for a user with no goals. Usually include at least three steps that can start now, each with an observable local output. If fewer are genuinely meaningful or ready, explain shortfall_reason instead of padding or bypassing real prerequisites. Stages only a proposal: a user must approve before the goal, tasks or schedule exist. Each node is create-only, with optional client_ref/parent_ref to an earlier node.",
		Parameters: objectSchema(map[string]any{
			"title":            stringProp("Research goal title."),
			"description":      stringProp("Context and explicit assumptions, not unrelated personal errands."),
			"success_criteria": stringProp("Observable outcome that makes the research goal complete."),
			"shortfall_reason": stringProp("When fewer than three meaningful steps can start now, explain the real reason (for example one genuinely atomic task, a missing required artifact, or insufficient scope). First try useful decomposition; never invent filler steps or remove true dependencies to reach three."),
			"instruction":      stringProp("Brief explanation of this proposed research plan."),
			"nodes": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 40,
				"description": "Create-only nodes in parent-before-child order. Array order gives the recommended work sequence; it does not create hard dependencies. Split a large deliverable into distinct, useful outputs with concrete next actions. Use numeric priority and estimate_minutes, never high/medium/low labels.",
				"items": objectSchema(map[string]any{
					"type":             stringProp("Node type. Only task/action leaves are scheduled.", "milestone", "task", "action"),
					"title":            stringProp("Concrete task title, up to 300 characters."),
					"success_criteria": stringProp("Observable outcome, up to 2000 characters."),
					"minimum_action":   stringProp("First small action that can be started now, up to 2000 characters."),
					"description":      stringProp("Execution advice and assumptions."),
					"client_ref":       stringProp("Unique temporary reference for this new node."),
					"parent_ref":       stringProp("Temporary reference of an earlier parent node; omit for a root node."),
					"depends_on_refs":  map[string]any{"type": "array", "maxItems": 40, "items": stringProp("Only a truly required finished artifact: temporary client_ref of an earlier prerequisite node. Explain the required artifact in description. It must reach final Task completion before this node is executable. Ordinary research order, drafting with explicit assumptions, or preparation work must not be encoded as a hard dependency.")},
					"priority":         integerProp("Numeric priority, 0..100; default 50.", 0, 100),
					"estimate_minutes": integerProp("Estimated TOTAL work minutes for the task; distinct from today's focus budget and minimum action. Do not invent word/paper/case counts the user did not request.", 1, 1440),
				}, "type", "title", "success_criteria", "minimum_action"),
			},
		}, "title", "success_criteria", "nodes"),
	}, build: func(ctx context.Context, a *App, tc ToolContext, args map[string]any) (PendingProposalRef, any, string, error) {
		payload, err := normalizeGoalWithTasks(args)
		if err != nil {
			return PendingProposalRef{}, nil, "", err
		}
		if err := validateNewGoalDecomposition(payload); err != nil {
			return PendingProposalRef{}, nil, "", err
		}
		return PendingProposalRef{Summary: "创建科研目标：" + payload.Title, Diff: payload}, payload, strings.TrimSpace(stringArg(args, "instruction")), nil
	}}
}

func normalizeGoalWithTasks(args map[string]any) (goalWithTasksPayload, error) {
	payload := goalWithTasksPayload{Title: strings.TrimSpace(stringArg(args, "title")), Description: strings.TrimSpace(stringArg(args, "description")), SuccessCriteria: strings.TrimSpace(stringArg(args, "success_criteria")), ShortfallReason: strings.TrimSpace(stringArg(args, "shortfall_reason"))}
	if utf8.RuneCountInString(payload.ShortfallReason) > 2000 {
		return payload, patchError("shortfall_reason must be at most 2000 characters")
	}
	goal := persistence.Goal{Title: payload.Title, Description: payload.Description, SuccessCriteria: payload.SuccessCriteria}
	if err := validateGoalFields(&goal); err != nil {
		return payload, patchError("a goal requires a title (up to 200 characters), success_criteria (up to 2000 characters), and description up to 4000 characters")
	}
	patch, problem := normalizePatch(args["nodes"])
	if problem != "" {
		return payload, patchError(problem)
	}
	refs := map[string]bool{}
	hasChildren := map[string]bool{}
	for i, node := range patch {
		op := textValue(node["op"])
		if (op != "" && op != "create") || textValue(node["parent_id"]) != "" || textValue(node["target_id"]) != "" {
			return payload, patchError("new-goal nodes must be create-only; use parent_ref for the new tree")
		}
		if _, exists := node["depends_on"]; exists {
			return payload, patchError("new-goal prerequisites must use depends_on_refs for the new tree")
		}
		if parent := textValue(node["parent_ref"]); parent != "" && !refs[parent] {
			return payload, patchError("parent_ref must name an earlier node")
		}
		hasChildren[textValue(node["parent_ref"])] = true
		if ref := textValue(node["client_ref"]); ref != "" {
			if refs[ref] {
				return payload, patchError("client_ref must be unique")
			}
			refs[ref] = true
		}
		task := persistence.Task{Type: textValue(node["type"]), Title: textValue(node["title"]), Description: textValue(node["description"]), SuccessCriteria: textValue(node["success_criteria"]), MinimumAction: textValue(node["minimum_action"]), Position: i}
		if changeInt(node, "priority", &task.Priority) != nil || changeInt(node, "estimate_minutes", &task.EstimateMinutes) != nil {
			return payload, patchError("task priority and estimate_minutes must be whole numbers")
		}
		if task.Priority == 0 {
			task.Priority = 50
		}
		if task.EstimateMinutes == 0 {
			task.EstimateMinutes = 25
		}
		if err := validateTaskFields(&task); err != nil {
			return payload, patchError("each task requires valid text, priority 0..100 and estimate_minutes 1..1440")
		}
	}
	leaves := 0
	for _, node := range patch {
		ref := textValue(node["client_ref"])
		if (node["type"] == "task" || node["type"] == "action") && (ref == "" || !hasChildren[ref]) {
			leaves++
		}
	}
	if leaves == 0 {
		return payload, patchError("include at least one executable task or action")
	}
	payload.Nodes = patch
	return payload, nil
}

// Quality feedback belongs at new-proposal time, where the model can revise its
// decomposition. ApplyGoalWithTasks keeps its legacy shape validation so saved
// proposals remain reviewable after an upgrade. No real task graph is rewritten.
func validateNewGoalDecomposition(payload goalWithTasksPayload) error {
	byID := make(map[string]persistence.Task, len(payload.Nodes))
	created := make(map[string]bool, len(payload.Nodes))
	refs := make(map[string]string, len(payload.Nodes))
	children := map[string]bool{}
	for i, node := range payload.Nodes {
		id := fmt.Sprintf("draft-%d", i)
		if ref := textValue(node["client_ref"]); ref != "" {
			refs[ref] = id
		}
		task := persistence.Task{ID: id, UserID: "draft-owner", GoalID: "draft-goal", Type: textValue(node["type"]), Title: textValue(node["title"]), Description: textValue(node["description"]), SuccessCriteria: textValue(node["success_criteria"]), MinimumAction: textValue(node["minimum_action"]), Status: "ready"}
		if parentRef := textValue(node["parent_ref"]); parentRef != "" {
			parentID := refs[parentRef]
			task.ParentID = &parentID
			children[parentID] = true
		}
		dependencies, _, err := taskDependenciesFromPatch(node, refs)
		if err != nil {
			return patchError("the draft prerequisites must reference earlier nodes")
		}
		task.DependsOn = dependencies
		byID[id] = task
		created[id] = true
	}
	ready, leaves := 0, 0
	work := []persistence.Task{}
	for _, task := range byID {
		if !taskDependencyGraphValid(task, byID) {
			return patchError("the draft task tree contains an impossible prerequisite or ancestry relationship; preserve real required artifacts but separate preparation from blocked deliverables")
		}
		if children[task.ID] || (task.Type != "task" && task.Type != "action") {
			continue
		}
		leaves++
		work = append(work, task)
		if executableTaskEligibility(task, byID) {
			ready++
		}
	}
	if err := validateDistinctDraftSteps(work, draftWorkIdentity(byID, created)); err != nil {
		return err
	}
	if ready < 3 && payload.ShortfallReason == "" {
		return patchError(fmt.Sprintf("the proposed goal has %d executable leaves but only %d can start now. Usually decompose the goal into three distinct, meaningful work steps with concrete local outputs. Array order is sufficient for recommended sequence; use hard dependencies only for truly required finished artifacts. Do not bypass true prerequisites or invent filler: if fewer than three steps are genuinely possible, provide shortfall_reason and re-propose", leaves, ready))
	}
	return nil
}

func validateDistinctDraftSteps(work []persistence.Task, identity func(persistence.Task) string) error {
	steps := map[string]bool{}
	for _, task := range work {
		step := identity(task)
		if steps[step] {
			return patchError("copying the same title, output and first action in the same parent, material and execution context does not create distinct research steps; describe each step's real object, deliverable or independent round")
		}
		steps[step] = true
	}
	return nil
}

func newGoalUpdateTool(app *App) Tool {
	return proposalTool{app: app, kind: "goal_update", def: agent.ToolDefinition{
		Name:        "propose_goal_update",
		Description: "Propose editing an existing goal's title, description, acceptance criteria, or pause/resume status. Read list_goals first for the goal's current revision. User approval is required.",
		Parameters: objectSchema(map[string]any{
			"goal_id":       stringProp("An existing goal owned by this user."),
			"base_revision": integerProp("Current goal revision from list_goals.", 1, 1000000000),
			"instruction":   stringProp("Reason for the change."),
			"changes":       objectSchema(map[string]any{"title": stringProp("Updated title."), "description": stringProp("Updated context."), "success_criteria": stringProp("Updated observable outcome."), "status": stringProp("Pause or resume this goal.", "active", "paused")}),
		}, "goal_id", "base_revision", "changes"),
	}, build: func(ctx context.Context, a *App, tc ToolContext, args map[string]any) (PendingProposalRef, any, string, error) {
		goal, err := RequireLiveGoalTx(a.Store.DB.WithContext(ctx), tc.UserID, stringArg(args, "goal_id"))
		if err != nil {
			return PendingProposalRef{}, nil, "", patchError("goal not found or unavailable")
		}
		changes, ok := args["changes"].(map[string]any)
		if !ok || len(changes) == 0 {
			return PendingProposalRef{}, nil, "", patchError("changes must include a goal field")
		}
		for key, value := range changes {
			switch key {
			case "title", "success_criteria":
				if strings.TrimSpace(textValue(value)) == "" {
					return PendingProposalRef{}, nil, "", patchError(key + " cannot be empty")
				}
			case "description":
			case "status":
				if value != "active" && value != "paused" {
					return PendingProposalRef{}, nil, "", patchError("status must be active or paused")
				}
			default:
				return PendingProposalRef{}, nil, "", patchError("unsupported goal field")
			}
		}
		base := intValue(args["base_revision"])
		if base < 1 {
			return PendingProposalRef{}, nil, "", patchError("base_revision is required")
		}
		return PendingProposalRef{GoalID: goal.ID, BaseRevision: base, Summary: "修改科研目标：" + goal.Title, Diff: changes}, goalUpdatePayload{Changes: changes}, strings.TrimSpace(stringArg(args, "instruction")), nil
	}}
}

// ApplyGoalWithTasks commits the first-goal workflow as one transaction. A
// rejected or stale proposal cannot create even the goal shell or primary focus.
func (a *App) ApplyGoalWithTasks(ctx context.Context, userID string, proposal *persistence.Proposal, threadID string) (*persistence.Goal, int, error) {
	var goal persistence.Goal
	created := 0
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var current persistence.Proposal
		if err := tx.Where("id = ? AND user_id = ? AND kind = 'goal_with_tasks' AND status = 'pending' AND goal_id IS NULL", proposal.ID, userID).First(&current).Error; err != nil {
			return notFound(err)
		}
		var payload goalWithTasksPayload
		if err := json.Unmarshal([]byte(current.PatchJSON), &payload); err != nil {
			return ErrValidation
		}
		encoded, _ := json.Marshal(payload)
		var raw map[string]any
		if err := json.Unmarshal(encoded, &raw); err != nil {
			return ErrValidation
		}
		validated, err := normalizeGoalWithTasks(raw)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrValidation, err)
		}
		var thread persistence.AgentThread
		if err := tx.Where("id = ? AND user_id = ? AND status = 'regular'", threadID, userID).First(&thread).Error; err != nil {
			return notFound(err)
		}
		var activeGoals int64
		if err := tx.Model(&persistence.Goal{}).Where("user_id = ? AND deleted_at IS NULL AND status = 'active'", userID).Count(&activeGoals).Error; err != nil {
			return err
		}
		now := persistence.Now()
		goal = persistence.Goal{Title: validated.Title, Description: validated.Description, SuccessCriteria: validated.SuccessCriteria}
		if err := a.GoalService.createGoalTx(tx, userID, &goal); err != nil {
			return err
		}
		if err := tx.Model(&persistence.Proposal{}).Where("id = ? AND status = 'pending' AND goal_id IS NULL", current.ID).Update("goal_id", goal.ID).Error; err != nil {
			return err
		}
		current.GoalID = goal.ID
		created, err = a.GoalService.applyProposalTx(tx, userID, &current, validated.Nodes, 0)
		if err != nil {
			return err
		}
		if activeGoals == 0 {
			settings, err := planningSettingsTx(tx, userID)
			if err != nil {
				return err
			}
			if settings.Revision == 0 {
				settings.PrimaryGoalID, settings.Revision, settings.CreatedAt, settings.UpdatedAt = &goal.ID, 1, now, now
				if err := tx.Create(&settings).Error; err != nil {
					return err
				}
			} else if err := tx.Model(&persistence.PlanningSettings{}).Where("user_id = ? AND revision = ?", userID, settings.Revision).Updates(map[string]any{"primary_goal_id": goal.ID, "revision": settings.Revision + 1, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&thread).Updates(map[string]any{"goal_id": goal.ID, "updated_at": now}).Error; err != nil {
			return err
		}
		var user persistence.User
		if err := tx.First(&user, "id = ? AND status = 'active'", userID).Error; err != nil {
			return notFound(err)
		}
		loc, zone := resolveZone(user.Timezone)
		if _, err := a.PlanService.refreshScheduleTx(tx, user, now.In(loc).Format("2006-01-02"), loc, zone, now); err != nil {
			return err
		}
		return nil
	})
	return &goal, created, err
}

func (a *App) ApplyGoalUpdateProposal(ctx context.Context, userID string, proposal *persistence.Proposal) (*persistence.Goal, error) {
	var goal *persistence.Goal
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var current persistence.Proposal
		if err := tx.Where("id = ? AND user_id = ? AND kind = 'goal_update' AND status = 'pending'", proposal.ID, userID).First(&current).Error; err != nil {
			return notFound(err)
		}
		var payload goalUpdatePayload
		if json.Unmarshal([]byte(current.PatchJSON), &payload) != nil || len(payload.Changes) == 0 {
			return ErrValidation
		}
		var err error
		goal, err = a.GoalService.updateGoalTx(tx, userID, current.GoalID, current.BaseRevision, payload.Changes)
		if err != nil {
			return err
		}
		result := tx.Model(&current).Where("revision = ? AND status = 'pending'", current.Revision).Updates(map[string]any{"status": "applied", "revision": current.Revision + 1, "updated_at": persistence.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return nil
	})
	return goal, err
}
