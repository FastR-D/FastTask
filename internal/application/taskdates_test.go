package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestTaskPlannedDatesPreserveScheduleSnapshotsAndRecovery(t *testing.T) {
	app, owner, other, _, items := deletionFixture(t, 4)
	ctx := context.Background()
	var task persistence.Task
	if err := app.Store.DB.First(&task, "id = ?", *items[0].TaskID).Error; err != nil {
		t.Fatal(err)
	}
	before, err := app.RefreshSchedule(ctx, owner, persistence.Now())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := app.UpdateTask(ctx, owner, task.ID, task.Revision, map[string]any{"planned_start": "2099-12-01", "planned_end": "2099-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateTask(ctx, owner, task.ID, task.Revision, map[string]any{"planned_end": "2100-01-01"}); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale dates accepted: %v", err)
	}
	if _, err := app.UpdateTask(ctx, other, task.ID, updated.Revision, map[string]any{"planned_start": nil, "planned_end": nil}); err == nil {
		t.Fatal("another user changed the dates")
	}
	after, err := app.RefreshSchedule(ctx, owner, persistence.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("editing long-term dates changed daily commitments")
	}
	updated, err = app.UpdateTask(ctx, owner, task.ID, updated.Revision, map[string]any{"description": "An unrelated edit"})
	if err != nil || updated.PlannedStart == nil || *updated.PlannedStart != "2099-12-01" || updated.PlannedEnd == nil || *updated.PlannedEnd != "2099-12-31" {
		t.Fatalf("unrelated edit lost dates: %+v %v", updated, err)
	}
	var snapshot persistence.TaskTreeRevision
	if err := app.Store.DB.Where("goal_id = ?", task.GoalID).Order("revision DESC").First(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	var tasks []persistence.Task
	if err := json.Unmarshal([]byte(snapshot.SnapshotJSON), &tasks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, recorded := range tasks {
		if recorded.ID == task.ID {
			found = recorded.PlannedStart != nil && *recorded.PlannedStart == "2099-12-01" && recorded.PlannedEnd != nil && *recorded.PlannedEnd == "2099-12-31"
		}
	}
	if !found {
		t.Fatal("task tree snapshot lost dates")
	}
	deleted, err := app.DeleteTask(ctx, owner, task.ID, updated.Revision)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := app.RestoreTask(ctx, owner, task.ID, deleted.Revision)
	if err != nil || restored.PlannedStart == nil || *restored.PlannedStart != "2099-12-01" || restored.PlannedEnd == nil || *restored.PlannedEnd != "2099-12-31" {
		t.Fatalf("restore lost dates: %+v %v", restored, err)
	}
	cleared, err := app.UpdateTask(ctx, owner, task.ID, restored.Revision, map[string]any{"planned_start": nil, "planned_end": nil})
	if err != nil || cleared.PlannedStart != nil || cleared.PlannedEnd != nil {
		t.Fatalf("cannot clear dates: %+v %v", cleared, err)
	}
}

func TestTaskPlannedDateValidationIsAtomic(t *testing.T) {
	app, owner, _, _, items := deletionFixture(t, 1)
	ctx := context.Background()
	var original persistence.Task
	if err := app.Store.DB.First(&original, "id = ?", *items[0].TaskID).Error; err != nil {
		t.Fatal(err)
	}
	for _, changes := range []map[string]any{
		{"planned_start": "2026-11-01"},
		{"planned_end": "2026-11-01"},
		{"planned_start": "2026-11-02", "planned_end": "2026-11-01"},
		{"planned_start": "2026-02-29", "planned_end": "2026-03-01"},
		{"planned_start": "2026-2-01", "planned_end": "2026-02-02"},
		{"planned_start": "0000-01-01", "planned_end": "0000-01-02"},
		{"planned_start": "", "planned_end": ""},
		{"planned_start": 20261101, "planned_end": true},
	} {
		if _, err := app.UpdateTask(ctx, owner, original.ID, original.Revision, changes); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid range accepted: %+v %v", changes, err)
		}
		var saved persistence.Task
		if err := app.Store.DB.First(&saved, "id = ?", original.ID).Error; err != nil || saved.Revision != original.Revision || saved.PlannedStart != nil || saved.PlannedEnd != nil {
			t.Fatalf("invalid edit partially applied: %+v %v", saved, err)
		}
	}
	for _, date := range []string{"2028-02-29", "0001-01-01", "9999-12-31"} {
		updated, err := app.UpdateTask(ctx, owner, original.ID, original.Revision, map[string]any{"planned_start": date, "planned_end": date})
		if err != nil {
			t.Fatalf("valid same-day range rejected: %s %v", date, err)
		}
		original = *updated
	}
}

func TestAgentProposalPreservesManualTaskDates(t *testing.T) {
	app, owner, _, _, items := deletionFixture(t, 1)
	ctx := context.Background()
	var original persistence.Task
	if err := app.Store.DB.First(&original, "id = ?", *items[0].TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateTask(ctx, owner, original.ID, original.Revision, map[string]any{"planned_start": "2026-11-01", "planned_end": "2026-11-30"}); err != nil {
		t.Fatal(err)
	}
	patch := []map[string]any{{"op": "update", "target_id": original.ID, "description": "Updated through an approved proposal"}}
	encoded, _ := json.Marshal(patch)
	now := persistence.Now()
	proposal := persistence.Proposal{ID: persistence.NewID("proposal"), UserID: owner, GoalID: original.GoalID, Status: "pending", PatchJSON: string(encoded), Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := app.Store.DB.Create(&proposal).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := app.ApplyProposal(ctx, owner, &proposal, patch); err != nil {
		t.Fatal(err)
	}
	var got persistence.Task
	if err := app.Store.DB.First(&got, "id = ?", original.ID).Error; err != nil || got.PlannedStart == nil || *got.PlannedStart != "2026-11-01" || got.PlannedEnd == nil || *got.PlannedEnd != "2026-11-30" || got.Description != "Updated through an approved proposal" {
		t.Fatalf("approved proposal lost date data: %+v %v", got, err)
	}
}
