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

type Candidate struct {
	ID             string
	Status         string
	Priority       int
	Estimate       int
	MinimumAction  string
	GoalActive     bool
	DependencyDone bool
}

func SelectDailyCandidates(candidates []Candidate, availableMinutes int) []Candidate {
	selected := make([]Candidate, 0, 3)
	for _, candidate := range candidates {
		if !candidate.GoalActive || !candidate.DependencyDone || candidate.MinimumAction == "" {
			continue
		}
		if candidate.Status != "ready" && candidate.Status != "in_progress" {
			continue
		}
		if availableMinutes > 0 && candidate.Estimate > availableMinutes*2 {
			continue
		}
		selected = append(selected, candidate)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Priority != selected[j].Priority {
			return selected[i].Priority > selected[j].Priority
		}
		return selected[i].ID < selected[j].ID
	})
	if len(selected) > 3 {
		selected = selected[:3]
	}
	return selected
}

func ValidateGoalTransition(from, to string) error {
	if from == to {
		return nil
	}
	allowed := map[string]map[string]bool{
		"draft":     {"active": true, "abandoned": true},
		"active":    {"paused": true, "completed": true, "abandoned": true},
		"paused":    {"active": true, "abandoned": true},
		"completed": {"archived": true},
		"abandoned": {"archived": true},
	}
	if !allowed[from][to] {
		return ErrInvalidTransition
	}
	return nil
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
