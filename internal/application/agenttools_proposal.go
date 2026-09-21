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
// receipt handler, via ApplyProposal (§7.3).
type proposalTool struct {
	app   *App
	def   agent.ToolDefinition
	kind  string
	build func(ctx context.Context, app *App, tc ToolContext, args map[string]any) (PendingProposalRef, []map[string]any, string, error)
}

func (p proposalTool) Definition() agent.ToolDefinition { return p.def }
func (p proposalTool) Level() ToolLevel                 { return ToolProposal }

// Execute creates the Proposal record and returns a PendingProposalRef so the
// loop can transition the run to awaiting_approval (§6, §7). It never touches
// goals/tasks/coords/plans — only the proposals staging table.
func (p proposalTool) Execute(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
	ref, patch, instruction, err := p.build(ctx, p.app, tc, args)
	if err != nil {
		if _, ok := err.(patchValidationError); ok {
			return ToolResult{IsError: true, Text: err.Error()}, nil
		}
		return ToolResult{}, err
	}
	if ref.GoalID == "" {
		return ToolResult{IsError: true, Text: "goal_id is required and must be one of the user's goals"}, nil
	}
	encoded, err := json.Marshal(patch)
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
	ref.Diff = patch
	ref.Summary = summarizePatch(patch)
	return ToolResult{
		Proposal: &ref,
		Result: map[string]any{
			"proposal_id": proposal.ID, "status": "pending", "base_revision": proposal.BaseRevision,
			"summary": ref.Summary, "note": "结构变更需用户确认后才会落库",
		},
	}, nil
}

// NewProposalTools returns the proposal-level tools (agent.md §5.2). Phase D
// ships propose_task_tree_patch, the ApplyProposal-backed structural change tool.
// propose_task_coords needs a distinct non-structural apply path and
// propose_daily_plan is phase E; both are deferred rather than mis-routed through
// ApplyProposal (which snapshots the tree and bumps task revisions).
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
	}
}

// buildTaskTreePatchProposal validates the patch shape and resolves the base
// revision. Deep field/cycle/ownership validation is deferred to ApplyProposal,
// which re-validates inside the apply transaction (§7.3, invariant 4).
func buildTaskTreePatchProposal(app *App) func(context.Context, *App, ToolContext, map[string]any) (PendingProposalRef, []map[string]any, string, error) {
	return func(ctx context.Context, a *App, tc ToolContext, args map[string]any) (PendingProposalRef, []map[string]any, string, error) {
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
		ref := PendingProposalRef{GoalID: goalID, BaseRevision: base}
		return ref, patch, strings.TrimSpace(stringArg(args, "instruction")), nil
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
