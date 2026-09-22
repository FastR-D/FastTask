package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// proposalTool is the shared shape for proposal-level tools: they validate the
// model's intent, create a pending Proposal, and write NOTHING to business
// tables (§5.2, invariant 1). The user-approved apply happens later, in the HTTP
// receipt handler (§7.3), routed by tool name to the right apply path.
//
// build returns the opaque payload stored as Proposal.PatchJSON (a task-tree op
// array, or a daily-plan envelope object), plus the client-facing ref. Each tool
// owns its own Diff/Summary so the approval card renders correctly.
type proposalTool struct {
	app   *App
	def   agent.ToolDefinition
	kind  string
	build func(ctx context.Context, app *App, tc ToolContext, args map[string]any) (PendingProposalRef, any, string, error)
}

func (p proposalTool) Definition() agent.ToolDefinition { return p.def }
func (p proposalTool) Level() ToolLevel                 { return ToolProposal }

// Execute creates the Proposal record and returns a PendingProposalRef so the
// loop can transition the run to awaiting_approval (§6, §7). It never touches
// goals/tasks/coords/plans — only the proposals staging table.
func (p proposalTool) Execute(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	ref, payload, instruction, err := p.build(ctx, p.app, tc, args)
	if err != nil {
		if _, ok := err.(patchValidationError); ok {
			return ToolResult{IsError: true, Text: err.Error()}, nil
		}
		return ToolResult{}, err
	}
	if ref.GoalID == "" {
		return ToolResult{IsError: true, Text: "goal_id is required and must be one of the user's goals"}, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ToolResult{}, err
	}
	now := persistence.Now()
	proposal := persistence.Proposal{
		ID: persistence.NewID("proposal"), UserID: tc.UserID, GoalID: ref.GoalID,
		Status: "pending", Instruction: instruction, PatchJSON: string(encoded),
		BaseRevision: ref.BaseRevision, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := p.app.Store.DB.WithContext(ctx).Create(&proposal).Error; err != nil {
		return ToolResult{}, err
	}
	ref.ProposalID = proposal.ID
	ref.Kind = p.kind
	return ToolResult{
		Proposal: &ref,
		Result: map[string]any{
			"proposal_id": proposal.ID, "status": "pending", "base_revision": proposal.BaseRevision,
			"summary": ref.Summary, "note": "需用户确认后才会落库",
		},
	}, nil
}

// NewProposalTools returns the proposal-level tools (agent.md §5.2): the two
// that change the user's plan. propose_task_tree_patch is backed by ApplyProposal
// (structural tree change); propose_daily_plan is backed by ApplyDailyPlanProposal
// (deterministic top-3 selection, §6). propose_task_coords is deferred — it needs
// a distinct non-structural apply path and must not be mis-routed through either.
func NewProposalTools(app *App) []Tool {
	return []Tool{
		proposalTool{
			app:  app,
			kind: "task_tree_patch",
			def: agent.ToolDefinition{
				Name:        "propose_task_tree_patch",
				Description: "Propose a task-tree change (create/update/move/supersede) for a goal. Creates a pending proposal the user must approve; nothing is written until then. Read the tree first with get_task_tree to obtain target_id values and the current tree_revision.",
				Parameters: objectSchema(map[string]any{
					"goal_id":       stringProp("The goal whose task tree to change."),
					"instruction":   stringProp("Short human-readable rationale for the change."),
					"base_revision": integerProp("The tree_revision from get_task_tree this patch is based on; omit to use the current revision.", 0, 0),
					"patch": map[string]any{
						"type":        "array",
						"description": "Ordered operations. create: {op?,client_ref?,parent_ref?,type,title,success_criteria,minimum_action,priority?,estimate_minutes?,uncertainty?,contribution?,coord_rationale?}. update/move/supersede: {op,target_id,...fields}. move: {op:move,target_id,parent_id|null}.",
						"items":       map[string]any{"type": "object"},
					},
				}, "goal_id", "patch"),
			},
			build: buildTaskTreePatchProposal(app),
		},
		proposalTool{
			app:  app,
			kind: "daily_plan",
			def: agent.ToolDefinition{
				Name:        "propose_daily_plan",
				Description: "Propose today's core plan (0..3 tasks) from the user's executable candidates. Each candidate MUST carry a concrete 5-15 minute minimum_action; candidates without one are invalid. The program — not you — does the final filter, sort, dedup and top-3 cap, so proposing more than three is allowed but only the deterministic selection lands. Creates a pending proposal the user must approve.",
				Parameters: objectSchema(map[string]any{
					"local_date":        stringProp("Plan date YYYY-MM-DD; omit for today in the user's timezone."),
					"available_minutes": integerProp("Focus minutes available that day (0..1440); omit to use the goal's default or 0.", 0, 1440),
					"instruction":       stringProp("Short human-readable rationale for the plan."),
					"candidates": map[string]any{
						"type":        "array",
						"description": "Candidate tasks to consider, in the order you recommend. Each: {task_id (from list_goals/get_task_tree), minimum_action (required, 5-15 min concrete next step), reason?}. The deterministic rule re-filters, re-sorts by priority and caps at 3.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"task_id":        stringProp("The candidate task id."),
								"minimum_action": stringProp("Concrete 5-15 minute first step for this candidate."),
								"reason":         stringProp("Why this candidate belongs in today's plan."),
							},
							"required":             []string{"task_id", "minimum_action"},
							"additionalProperties": false,
						},
					},
				}, "candidates"),
			},
			build: buildDailyPlanProposal(app),
		},
	}
}

// buildTaskTreePatchProposal validates the patch shape and resolves the base
// revision. Deep field/cycle/ownership validation is deferred to ApplyProposal,
// which re-validates inside the apply transaction (§7.3, invariant 4).
func buildTaskTreePatchProposal(app *App) func(context.Context, *App, ToolContext, map[string]any) (PendingProposalRef, any, string, error) {
	return func(ctx context.Context, a *App, tc ToolContext, args map[string]any) (PendingProposalRef, any, string, error) {
		goalID := stringArg(args, "goal_id")
		if goalID == "" {
			return PendingProposalRef{}, nil, "", nil
		}
		// The goal must belong to the caller; a stranger's goal is not-found.
		var goal persistence.Goal
		if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", goalID, tc.UserID).First(&goal).Error; err != nil {
			if persistence.IsNotFound(err) {
				return PendingProposalRef{}, nil, "", nil // empty GoalID -> structured error
			}
			return PendingProposalRef{}, nil, "", err
		}
		patch, problem := normalizePatch(args["patch"])
		if problem != "" {
			return PendingProposalRef{}, nil, "", patchError(problem)
		}
		var current int
		a.Store.DB.WithContext(ctx).Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goalID).Select("COALESCE(MAX(revision),0)").Scan(&current)
		base := current
		if provided, ok := args["base_revision"].(float64); ok && provided > 0 {
			base = int(provided)
		}
		ref := PendingProposalRef{
			GoalID: goalID, BaseRevision: base,
			Diff: patch, Summary: summarizePatch(patch),
		}
		return ref, patch, strings.TrimSpace(stringArg(args, "instruction")), nil
	}
}

// buildDailyPlanProposal validates the candidate list and stages a daily-plan
// proposal. It performs NO deterministic selection here — that happens at apply
// time in ApplyDailyPlanProposal so the program (not the model) owns the top-3
// cap (§6, arch.md §9.2). goal_id is derived from the first candidate's task to
// satisfy the proposals table's goal scoping; the plan itself is per user+date.
func buildDailyPlanProposal(app *App) func(context.Context, *App, ToolContext, map[string]any) (PendingProposalRef, any, string, error) {
	return func(ctx context.Context, a *App, tc ToolContext, args map[string]any) (PendingProposalRef, any, string, error) {
		candidates, problem := normalizeCandidates(args["candidates"])
		if problem != "" {
			return PendingProposalRef{}, nil, "", patchError(problem)
		}
		// Derive the goal from the first candidate's task (ownership-checked).
		var task persistence.Task
		if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", candidates[0].TaskID, tc.UserID).First(&task).Error; err != nil {
			if persistence.IsNotFound(err) {
				return PendingProposalRef{}, nil, "", patchError(fmt.Sprintf("candidate task %q not found", candidates[0].TaskID))
			}
			return PendingProposalRef{}, nil, "", err
		}
		// Resolve the plan timezone from the user's profile (daily plans are keyed by
		// user+date+timezone). resolveZone falls back to the default when unset.
		var user persistence.User
		if err := a.Store.DB.WithContext(ctx).Where("id = ?", tc.UserID).First(&user).Error; err != nil {
			if persistence.IsNotFound(err) {
				return PendingProposalRef{}, nil, "", patchError("user profile not found")
			}
			return PendingProposalRef{}, nil, "", err
		}
		location, zone := resolveZone(user.Timezone)
		localDate := strings.TrimSpace(stringArg(args, "local_date"))
		if localDate == "" {
			localDate = persistence.Now().In(location).Format("2006-01-02")
		}
		available := 0
		if v, ok := args["available_minutes"].(float64); ok && v > 0 {
			available = int(v)
		}
		payload := dailyPlanPayload{
			LocalDate:        localDate,
			Timezone:         zone,
			AvailableMinutes: available,
			Candidates:       candidates,
		}
		ref := PendingProposalRef{
			GoalID:  task.GoalID,
			Diff:    candidatesDiff(candidates),
			Summary: fmt.Sprintf("今日计划提案：%d 项候选（程序确定性取前三）", len(candidates)),
		}
		return ref, payload, strings.TrimSpace(stringArg(args, "instruction")), nil
	}
}

// patchError is a sentinel the proposal Execute path converts to a structured
// tool error rather than a fatal run error.
type patchValidationError struct{ msg string }

func (e patchValidationError) Error() string { return e.msg }
func patchError(msg string) error            { return patchValidationError{msg: msg} }

// normalizePatch coerces the model's patch argument into []map[string]any and
// shape-validates each op so the model gets fast feedback. It mirrors the
// op vocabulary ApplyProposal accepts.
func normalizePatch(raw any) ([]map[string]any, string) {
	list, ok := raw.([]any)
	if !ok {
		return nil, "patch must be an array of operations"
	}
	if len(list) == 0 {
		return nil, "patch must contain at least one operation"
	}
	if len(list) > 40 {
		return nil, "patch has too many operations (max 40)"
	}
	patch := make([]map[string]any, 0, len(list))
	refs := map[string]bool{}
	for i, item := range list {
		op, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("patch[%d] must be an object", i)
		}
		kind := strings.TrimSpace(textValue(op["op"]))
		if kind == "" {
			kind = "create"
		}
		switch kind {
		case "create":
			nodeType := textValue(op["type"])
			if nodeType != "milestone" && nodeType != "task" && nodeType != "action" {
				return nil, fmt.Sprintf("patch[%d] create requires type in milestone|task|action", i)
			}
			for _, field := range []string{"title", "success_criteria", "minimum_action"} {
				if textValue(op[field]) == "" {
					return nil, fmt.Sprintf("patch[%d] create requires %s", i, field)
				}
			}
			if ref := textValue(op["client_ref"]); ref != "" {
				refs[ref] = true
			}
			if parentRef := textValue(op["parent_ref"]); parentRef != "" && !refs[parentRef] {
				return nil, fmt.Sprintf("patch[%d] parent_ref %q must reference an earlier client_ref", i, parentRef)
			}
		case "update", "move", "supersede":
			if textValue(op["target_id"]) == "" {
				return nil, fmt.Sprintf("patch[%d] %s requires target_id from get_task_tree", i, kind)
			}
		default:
			return nil, fmt.Sprintf("patch[%d] has unknown op %q", i, kind)
		}
		patch = append(patch, op)
	}
	return patch, ""
}

// normalizeCandidates coerces the model's candidate list into []PlanCandidate,
// enforcing the §6 constraint that every candidate carries a concrete minimum
// action ("缺失则该候选无效"). Duplicate task_ids are dropped (dedup is also
// re-applied deterministically at selection time).
func normalizeCandidates(raw any) ([]PlanCandidate, string) {
	list, ok := raw.([]any)
	if !ok {
		return nil, "candidates must be an array"
	}
	if len(list) == 0 {
		return nil, "candidates must contain at least one task"
	}
	if len(list) > 20 {
		return nil, "too many candidates (max 20)"
	}
	out := make([]PlanCandidate, 0, len(list))
	seen := map[string]bool{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("candidates[%d] must be an object", i)
		}
		taskID := strings.TrimSpace(textValue(m["task_id"]))
		minAction := strings.TrimSpace(textValue(m["minimum_action"]))
		if taskID == "" {
			return nil, fmt.Sprintf("candidates[%d] requires task_id", i)
		}
		if minAction == "" {
			return nil, fmt.Sprintf("candidates[%d] (%s) requires a concrete minimum_action", i, taskID)
		}
		if seen[taskID] {
			continue
		}
		seen[taskID] = true
		out = append(out, PlanCandidate{TaskID: taskID, MinimumAction: minAction, Reason: strings.TrimSpace(textValue(m["reason"]))})
	}
	if len(out) == 0 {
		return nil, "candidates must contain at least one valid task"
	}
	return out, ""
}

// candidatesDiff renders the candidate list as the approval-card diff array.
func candidatesDiff(candidates []PlanCandidate) []map[string]any {
	diff := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		entry := map[string]any{"task_id": c.TaskID, "minimum_action": c.MinimumAction}
		if c.Reason != "" {
			entry["reason"] = c.Reason
		}
		diff = append(diff, entry)
	}
	return diff
}

// summarizePatch renders a one-line human summary for the approval card.
func summarizePatch(patch []map[string]any) string {
	counts := map[string]int{}
	for _, op := range patch {
		kind := strings.TrimSpace(textValue(op["op"]))
		if kind == "" {
			kind = "create"
		}
		counts[kind]++
	}
	order := []string{"create", "update", "move", "supersede"}
	labels := map[string]string{"create": "新建", "update": "更新", "move": "移动", "supersede": "取代"}
	parts := make([]string, 0, len(counts))
	for _, kind := range order {
		if counts[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", labels[kind], counts[kind]))
		}
	}
	if len(parts) == 0 {
		return "任务树变更"
	}
	return "任务树提案：" + strings.Join(parts, "、")
}
