package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestSelectDailyCandidatesIsDeterministicAndLimited(t *testing.T) {
	input := []Candidate{
		{ID: "task_c", Status: "ready", Priority: 70, Estimate: 25, MinimumAction: "c", GoalActive: true, DependencyDone: true},
		{ID: "task_b", Status: "ready", Priority: 90, Estimate: 25, MinimumAction: "b", GoalActive: true, DependencyDone: true},
		{ID: "task_a", Status: "in_progress", Priority: 90, Estimate: 25, MinimumAction: "a", GoalActive: true, DependencyDone: true},
		{ID: "task_d", Status: "ready", Priority: 60, Estimate: 25, MinimumAction: "d", GoalActive: true, DependencyDone: true},
		{ID: "blocked", Status: "blocked", Priority: 100, Estimate: 25, MinimumAction: "x", GoalActive: true, DependencyDone: true},
		{ID: "no-action", Status: "ready", Priority: 100, Estimate: 25, GoalActive: true, DependencyDone: true},
	}
	want := []string{"task_a", "task_b", "task_c"}
	for run := 0; run < 10; run++ {
		selected := SelectDailyCandidates(input, 120)
		got := []string{selected[0].ID, selected[1].ID, selected[2].ID}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: got %v want %v", run, got, want)
		}
	}
}

func TestSelectDailyCandidatesAllowsEmptyAndFewerThanThree(t *testing.T) {
	if got := SelectDailyCandidates(nil, 120); len(got) != 0 {
		t.Fatalf("empty input returned %d candidates", len(got))
	}
	got := SelectDailyCandidates([]Candidate{{ID: "one", Status: "ready", Priority: 10, Estimate: 10, MinimumAction: "start", GoalActive: true, DependencyDone: true}}, 20)
	if len(got) != 1 || got[0].ID != "one" {
		t.Fatalf("unexpected result: %#v", got)
	}
}

func TestGoalTransitions(t *testing.T) {
	valid := [][2]string{{"draft", "active"}, {"active", "paused"}, {"paused", "active"}, {"active", "completed"}, {"completed", "archived"}, {"active", "abandoned"}, {"abandoned", "archived"}}
	for _, pair := range valid {
		if err := ValidateGoalTransition(pair[0], pair[1]); err != nil {
			t.Errorf("%s -> %s rejected: %v", pair[0], pair[1], err)
		}
	}
	invalid := [][2]string{{"active", "archived"}, {"completed", "active"}, {"draft", "archived"}}
	for _, pair := range invalid {
		if !errors.Is(ValidateGoalTransition(pair[0], pair[1]), ErrInvalidTransition) {
			t.Errorf("%s -> %s should be invalid", pair[0], pair[1])
		}
	}
}

func TestWouldCreateCycle(t *testing.T) {
	parents := map[string]string{"child": "parent", "grandchild": "child"}
	if !WouldCreateCycle("parent", "grandchild", parents) {
		t.Fatal("expected cycle")
	}
	if WouldCreateCycle("new", "grandchild", parents) {
		t.Fatal("unexpected cycle")
	}
}
