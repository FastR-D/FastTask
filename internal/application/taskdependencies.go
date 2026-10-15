package application

import (
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// taskDependencyGraphValid validates the reachable qualification graph, which
// includes parent edges because a child inherits its ancestors' prerequisites.
// Parent/child structure remains independent from completion dependencies.
func taskDependencyGraphValid(task persistence.Task, byID map[string]persistence.Task) bool {
	if task.ID == "" || task.UserID == "" || task.GoalID == "" {
		return false
	}
	lookup := func(id string) (persistence.Task, bool) {
		if id == task.ID {
			return task, true
		}
		node, ok := byID[id]
		return node, ok && node.ID == id
	}
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(id string) bool {
		if state[id] == 1 {
			return false
		}
		if state[id] == 2 {
			return true
		}
		node, found := lookup(id)
		if !found || node.UserID != task.UserID || node.GoalID != task.GoalID || node.DeletedAt != nil {
			return false
		}
		state[id] = 1
		if node.ParentID != nil && (*node.ParentID == "" || !visit(*node.ParentID)) {
			return false
		}
		seen := map[string]bool{}
		for _, dependency := range node.DependsOn {
			if dependency == "" || strings.TrimSpace(dependency) != dependency || seen[dependency] || !visit(dependency) {
				return false
			}
			seen[dependency] = true
		}
		state[id] = 2
		return true
	}
	if !visit(task.ID) {
		return false
	}
	// Every executable ancestor must stay open. A prerequisite requiring one
	// of those same nodes to be completed would make the leaf impossible to run,
	// even if the plain depends_on graph has no directed cycle.
	openAncestors := map[string]bool{}
	for node := task; ; {
		openAncestors[node.ID] = true
		if node.ParentID == nil {
			break
		}
		node, _ = lookup(*node.ParentID)
	}
	for id := range state {
		node, _ := lookup(id)
		for _, dependency := range node.DependsOn {
			if openAncestors[dependency] {
				return false
			}
		}
	}
	return true
}

// validateTaskDependenciesTx validates a complete candidate before it is saved.
// The caller assigns its owner, goal and stable ID first. Existing rows without
// dependencies remain compatible; no title-based or historical inference occurs.
func validateTaskDependenciesTx(tx *gorm.DB, task persistence.Task) error {
	if _, err := RequireLiveGoalTx(tx, task.UserID, task.GoalID); err != nil {
		return err
	}
	var tasks []persistence.Task
	if err := tx.Where("user_id = ? AND goal_id = ?", task.UserID, task.GoalID).Find(&tasks).Error; err != nil {
		return err
	}
	byID := make(map[string]persistence.Task, len(tasks)+1)
	for _, current := range tasks {
		byID[current.ID] = current
	}
	byID[task.ID] = task
	if !taskDependencyGraphValid(task, byID) {
		return fmt.Errorf("%w: prerequisites must be live tasks in the same goal, without self-reference, cycles or conflicting ancestry", ErrValidation)
	}
	// Changing a parent's prerequisites or moving a prerequisite may also create
	// an inherited deadlock in a dependent leaf. Check only roots reaching this
	// candidate; unrelated legacy graphs do not block an edit.
	for _, current := range tasks {
		if current.ID == task.ID || current.DeletedAt != nil {
			continue
		}
		seen := map[string]bool{}
		var reachesCandidate func(string) bool
		reachesCandidate = func(id string) bool {
			if id == task.ID {
				return true
			}
			if seen[id] {
				return false
			}
			seen[id] = true
			node, ok := byID[id]
			if !ok {
				return false
			}
			if node.ParentID != nil && reachesCandidate(*node.ParentID) {
				return true
			}
			for _, dependency := range node.DependsOn {
				if reachesCandidate(dependency) {
					return true
				}
			}
			return false
		}
		if reachesCandidate(current.ID) && !taskDependencyGraphValid(current, byID) {
			return fmt.Errorf("%w: this change creates an invalid inherited prerequisite graph", ErrValidation)
		}
	}
	return nil
}

// executableTaskEligibility is the shared scheduling/execution qualification.
// Daily-plan satisfaction never enters this decision: a prerequisite and its
// prerequisites need final Task completion, and all referenced rows remain live.
func executableTaskEligibility(task persistence.Task, byID map[string]persistence.Task) bool {
	if !taskDependencyGraphValid(task, byID) {
		return false
	}
	lookup := func(id string) persistence.Task {
		if id == task.ID {
			return task
		}
		return byID[id]
	}
	for node := task; ; {
		if node.Status != "ready" && node.Status != "in_progress" {
			return false
		}
		if node.ParentID == nil {
			break
		}
		node = lookup(*node.ParentID)
	}
	checked := map[string]bool{}
	var prerequisitesSatisfied func(persistence.Task) bool
	prerequisitesSatisfied = func(node persistence.Task) bool {
		if checked[node.ID] {
			return true
		}
		for _, id := range node.DependsOn {
			predecessor := lookup(id)
			if predecessor.Status != "completed" || !prerequisitesSatisfied(predecessor) {
				return false
			}
		}
		if node.ParentID != nil && !prerequisitesSatisfied(lookup(*node.ParentID)) {
			return false
		}
		checked[node.ID] = true
		return true
	}
	return prerequisitesSatisfied(task)
}
