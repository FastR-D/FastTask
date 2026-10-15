package application

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// Conservative text equality preserves word boundaries and case-sensitive
// material names. This is copied-work feedback, not semantic deduplication.
func taskWorkFields(task persistence.Task, parentScope any) []any {
	text := func(value string) string { return strings.Join(strings.Fields(value), " ") }
	deps := append([]string{}, task.DependsOn...)
	sort.Strings(deps)
	kind := task.Type
	if kind == "task" || kind == "action" {
		kind = "executable" // Labels alone do not make distinct leaf work.
	}
	return []any{kind, parentScope, text(task.Title), text(task.Description), text(task.SuccessCriteria), text(task.MinimumAction), deps}
}

func taskWorkIdentity(task persistence.Task, parentScope any) string {
	identity, _ := json.Marshal(taskWorkFields(task, parentScope))
	return string(identity)
}

// Existing parent IDs anchor real independent experiments. Only parents created
// in this draft use their content and ancestry, so cloned wrappers cannot make
// identical leaves distinct merely by receiving different temporary refs.
// This comparison never changes a task's actual ParentID or proposal payload.
func draftWorkIdentity(byID map[string]persistence.Task, created map[string]bool) func(persistence.Task) string {
	cache, visiting := map[string]any{}, map[string]bool{}
	var scope func(string) any
	scope = func(id string) any {
		if id == "" {
			return ""
		}
		if !created[id] {
			return []any{"existing", id}
		}
		if cached, ok := cache[id]; ok {
			return cached
		}
		task, ok := byID[id]
		if !ok || visiting[id] {
			// Apply's graph validation owns invalid ancestry; do not recurse
			// indefinitely while projecting an invalid draft for feedback.
			return []any{"invalid", id}
		}
		visiting[id] = true
		var parentScope any = ""
		if task.ParentID != nil {
			parentScope = scope(*task.ParentID)
		}
		// A draft parent is a scope wrapper, regardless of its type label.
		// task is a local copy; the actual type and payload stay unchanged.
		task.Type = "draft-parent"
		value := []any{"draft", taskWorkFields(task, parentScope)}
		delete(visiting, id)
		cache[id] = value
		return value
	}
	return func(task persistence.Task) string {
		var parentScope any = ""
		if task.ParentID != nil {
			parentScope = scope(*task.ParentID)
		}
		return taskWorkIdentity(task, parentScope)
	}
}

// Compare only a newly created parent chain with an existing leaf's chain.
// At the first real draft-side anchor, IDs must match; existing experiments are
// never merged by their wording. Content is cached after final-state projection.
func draftParentCloneMatcher(byID map[string]persistence.Task, created map[string]bool) func(*string, *string) bool {
	content := map[string]string{}
	parentContent := func(task persistence.Task) string {
		if identity, ok := content[task.ID]; ok {
			return identity
		}
		task.Type = "draft-parent" // Comparison copy only, as in draftWorkIdentity.
		identity := taskWorkIdentity(task, "")
		content[task.ID] = identity
		return identity
	}
	return func(next, current *string) bool {
		seen := map[[2]string]bool{}
		for next != nil && current != nil {
			if !created[*next] {
				return *next == *current
			}
			pair := [2]string{*next, *current}
			if seen[pair] {
				return false // Invalid cycles remain Apply's responsibility.
			}
			seen[pair] = true
			draft, draftOK := byID[*next]
			existing, existingOK := byID[*current]
			if !draftOK || !existingOK || parentContent(draft) != parentContent(existing) {
				return false
			}
			next, current = draft.ParentID, existing.ParentID
		}
		return next == nil && current == nil
	}
}

// Project identity fields only, without writes. The ordered final state lets an
// explicit replacement, move or clarification differ from accidental re-adds.
// Apply remains responsible for ownership, cycles, evidence and valid changes.
func validateNewPatchSteps(patch []map[string]any, existing []persistence.Task) error {
	byID := make(map[string]persistence.Task, len(existing)+len(patch))
	for _, task := range existing {
		byID[task.ID] = task
	}
	refs := map[string]string{}
	created := []string{}
	createdIDs := map[string]bool{}
	under := func(id, root string) bool {
		seen := map[string]bool{}
		for id != "" && !seen[id] {
			if id == root {
				return true
			}
			seen[id] = true
			task, ok := byID[id]
			if !ok || task.ParentID == nil {
				break
			}
			id = *task.ParentID
		}
		return false
	}
	for i, node := range patch {
		op := textValue(node["op"])
		if op == "" {
			op = "create"
		}
		id := textValue(node["target_id"])
		task, exists := byID[id]
		if op == "create" {
			id = fmt.Sprintf("draft-%d", i)
			task = persistence.Task{ID: id, Status: "ready"}
			created = append(created, id)
			createdIDs[id] = true
		} else if !exists {
			continue // The existing apply validation reports invalid targets.
		}
		switch op {
		case "create", "update", "move":
			for field, target := range map[string]*string{"type": &task.Type, "title": &task.Title, "description": &task.Description, "success_criteria": &task.SuccessCriteria, "minimum_action": &task.MinimumAction, "status": &task.Status} {
				if value, supplied := node[field]; supplied {
					*target = textValue(value)
				}
			}
			_, hasParent := node["parent_id"]
			_, hasRef := node["parent_ref"]
			if hasParent || hasRef {
				parent := textValue(node["parent_id"])
				if hasRef {
					parent = refs[textValue(node["parent_ref"])]
				}
				task.ParentID = nil
				if parent != "" {
					task.ParentID = &parent
				}
			}
			if deps, changed, err := taskDependenciesFromPatch(node, refs); err != nil {
				return patchError("the draft prerequisites must reference earlier created tasks")
			} else if changed {
				task.DependsOn = deps
			}
			byID[id] = task
			if op == "create" {
				if ref := textValue(node["client_ref"]); ref != "" {
					refs[ref] = id
				}
			}
		case "supersede":
			task.Status = "superseded"
			byID[id] = task
		case "delete", "restore":
			// Match recoverable deletion batches: restoring a parent does not
			// restore descendants deleted independently before that parent.
			batch := task.DeletionID
			deleted := persistence.Now()
			for childID, child := range byID {
				if !under(childID, id) {
					continue
				}
				if op == "delete" && child.DeletedAt == nil {
					child.DeletedAt, child.DeletionID = &deleted, fmt.Sprintf("draft-deletion-%d", i)
				} else if op == "restore" && batch != "" && child.DeletionID == batch {
					child.DeletedAt, child.DeletionID = nil, ""
				}
				byID[childID] = child
			}
		}
	}
	children := map[string]bool{}
	for _, task := range byID {
		if task.ParentID != nil && task.DeletedAt == nil && task.Status != "cancelled" && task.Status != "superseded" {
			children[*task.ParentID] = true
		}
	}
	isOpenLeaf := func(task persistence.Task) bool {
		open := task.Status == "ready" || task.Status == "in_progress" || task.Status == "paused" || task.Status == "blocked"
		return open && task.DeletedAt == nil && !children[task.ID] && (task.Type == "task" || task.Type == "action")
	}
	work := []persistence.Task{}
	for _, id := range created {
		if task := byID[id]; isOpenLeaf(task) {
			work = append(work, task)
		}
	}
	identity := draftWorkIdentity(byID, createdIDs)
	if err := validateDistinctDraftSteps(work, identity); err != nil {
		return err
	}
	current := map[string]string{}
	currentByWork := map[string][]persistence.Task{}
	for _, original := range existing {
		if task := byID[original.ID]; isOpenLeaf(task) {
			current[identity(task)] = task.ID
			key := taskWorkIdentity(task, "")
			currentByWork[key] = append(currentByWork[key], task)
		}
	}
	parentClone := draftParentCloneMatcher(byID, createdIDs)
	for _, task := range work {
		id := current[identity(task)]
		if id == "" && task.ParentID != nil && createdIDs[*task.ParentID] {
			// Only exact leaf content reaches ancestry comparison. A direct
			// reference to a different existing parent keeps its real identity.
			for _, candidate := range currentByWork[taskWorkIdentity(task, "")] {
				if parentClone(task.ParentID, candidate.ParentID) {
					id = candidate.ID
					break
				}
			}
		}
		if id != "" {
			return patchError(fmt.Sprintf("this work already exists as open Task %s in the same parent, material and execution context; reuse or update that Task, or record another focus session. For a real independent round, describe its distinct material, output or context", id))
		}
	}
	return nil
}
