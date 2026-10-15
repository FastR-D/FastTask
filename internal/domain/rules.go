package domain

import (
	"errors"
	"sort"
	"strings"
)

var (
	ErrCoreLimit             = errors.New("daily plan can contain at most three core items")
	ErrSupportBeforeCoreDone = errors.New("support items require all core items to be satisfied")
	ErrInvalidTransition     = errors.New("invalid state transition")
	ErrCycle                 = errors.New("task tree cycle")
)

var externalImportSources = map[string]bool{
	"fastinsight": true,
	"fastnews":    true,
	"fastread":    true,
	"fastwrite":   true,
	"fastppt":     true,
	"fastlab":     true,
}

var externalImportKinds = map[string]bool{
	"candidate_task":    true,
	"research_material": true,
	"progress_evidence": true,
	"review_issue":      true,
	"generated_report":  true,
}

type Candidate struct {
	ID             string
	Status         string
	Priority       int
	Position       int
	Estimate       int // Total task workload; never a promise to finish it today.
	MinimumAction  string
	GoalActive     bool
	DependencyDone bool
}

func SelectDailyCandidates(candidates []Candidate, availableMinutes int) []Candidate {
	selected := make([]Candidate, 0, 3)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.ID == "" || seen[candidate.ID] || !candidate.GoalActive || !candidate.DependencyDone || strings.TrimSpace(candidate.MinimumAction) == "" {
			continue
		}
		if candidate.Status != "ready" && candidate.Status != "in_progress" {
			continue
		}
		// A daily commitment is a bounded advancement slice, not a promise to
		// finish the entire task estimate. The planner applies the daily budget
		// to slot count and target minutes instead of starving long research work.
		selected = append(selected, candidate)
		seen[candidate.ID] = true
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Priority != selected[j].Priority {
			return selected[i].Priority > selected[j].Priority
		}
		if selected[i].Position != selected[j].Position {
			return selected[i].Position < selected[j].Position
		}
		return selected[i].ID < selected[j].ID
	})
	if len(selected) > 3 {
		selected = selected[:3]
	}
	return selected
}

func ValidateGoalTransition(from, to string) error {
	allowed := map[string]map[string]bool{
		"draft":     {"active": true, "abandoned": true, "archived": true},
		"active":    {"paused": true, "completed": true, "abandoned": true, "archived": true},
		"paused":    {"active": true, "abandoned": true, "archived": true},
		"completed": {"active": true, "archived": true},
		"abandoned": {"active": true, "archived": true},
		"archived":  {"active": true},
	}
	if _, ok := allowed[from]; ok && from == to {
		return nil
	}
	if !allowed[from][to] {
		return ErrInvalidTransition
	}
	return nil
}

// Completion is evidence-bearing and has its own endpoint. Ordinary edits can
// pause/resume or cancel/reconsider work, but cannot fabricate or undo a result.
func ValidateTaskTransition(from, to string) error {
	open := map[string]bool{"ready": true, "in_progress": true, "blocked": true, "paused": true, "cancelled": true, "superseded": true}
	if open[from] && open[to] {
		return nil
	}
	return ErrInvalidTransition
}

func ValidateCompletionType(value string, allowedCSV string) bool {
	for _, allowed := range strings.Split(allowedCSV, ",") {
		if strings.TrimSpace(allowed) == value {
			return true
		}
	}
	return false
}

func WouldCreateCycle(taskID, parentID string, parents map[string]string) bool {
	for current := parentID; current != ""; current = parents[current] {
		if current == taskID {
			return true
		}
	}
	return false
}

func ValidExternalImportSource(value string) bool {
	return externalImportSources[value]
}

func ValidExternalImportKind(value string) bool {
	return externalImportKinds[value]
}

func ValidateExternalImportTransition(from, to string) error {
	if from == "candidate" && (to == "converted" || to == "rejected") {
		return nil
	}
	return ErrInvalidTransition
}
