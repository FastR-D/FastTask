package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func deletionFixture(t *testing.T, count int) (*App, string, string, persistence.DailyPlan, []persistence.DailyPlanItem) {
	t.Helper()
	store, owner, other := panelTestStore(t)
	app := New(store)
	ctx := context.Background()
	goal := persistence.Goal{Title: "Deletion fixture", SuccessCriteria: "Keep task evidence"}
	if err := app.CreateGoal(ctx, owner, &goal); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < count; index++ {
		task := persistence.Task{GoalID: goal.ID, Title: fmt.Sprintf("Core task %d", index+1), SuccessCriteria: "A real result", MinimumAction: "Do the first step", EstimateMinutes: 25, Position: index}
		if err := app.CreateTask(ctx, owner, &task); err != nil {
			t.Fatal(err)
		}
	}
	plan, items, err := app.EnsureDailyPlan(ctx, owner, "", "")
	if err != nil || len(items) != min(count, 3) {
		t.Fatalf("fixture plan: %+v %+v %v", plan, items, err)
	}
	return app, owner, other, *plan, items
}

func TestDeletePlanItemsPreservesEvidenceAndDoesNotRefill(t *testing.T) {
	app, owner, _, before, items := deletionFixture(t, 4)
	ctx := context.Background()
	for index, status := range []string{"satisfied", "skipped"} {
		if err := app.Store.DB.Model(&persistence.DailyPlanItem{}).Where("id = ?", items[index].ID).Updates(map[string]any{"status": status, "revision": 2}).Error; err != nil {
			t.Fatal(err)
		}
		items[index].Status, items[index].Revision = status, 2
	}
	now := persistence.Now()
	event := persistence.ProgressEvent{ID: "delete-evidence", UserID: owner, TaskID: items[0].TaskID, PlanItemID: &items[0].ID, Type: "step", Summary: "Already recorded", EvidenceJSON: "[]", OccurredAt: now, CreatedAt: now}
	if err := app.Store.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	session := persistence.WorkSession{TaskID: *items[2].TaskID, DailyPlanItemID: &items[2].ID, TargetMinutes: 25, StartedAt: now.Add(-time.Minute)}
	if err := app.StartSession(ctx, owner, &session); err != nil {
		t.Fatal(err)
	}
	result, err := app.DeletePlanItems(ctx, owner, before.ID, before.Revision, []PlanItemDeletion{{ID: items[0].ID, Revision: 2}, {ID: items[1].ID, Revision: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != items[2].ID || result.Items[0].Revision != items[2].Revision || result.Plan.CurrentRevision != before.CurrentRevision+1 || !result.Plan.ManualItemsRemoved {
		t.Fatalf("incorrect current snapshot: %+v", result)
	}
	for index := 0; index < 2; index++ {
		var original persistence.DailyPlanItem
		if err := app.Store.DB.First(&original, "id = ?", items[index].ID).Error; err != nil {
			t.Fatal(err)
		}
		if original.PlanRevision != before.CurrentRevision || original.Status != items[index].Status || original.Revision != items[index].Revision {
			t.Fatalf("historical evidence-bearing item changed: %+v", original)
		}
	}
	var savedEvent persistence.ProgressEvent
	if err := app.Store.DB.First(&savedEvent, "id = ?", event.ID).Error; err != nil || savedEvent.PlanItemID == nil || *savedEvent.PlanItemID != items[0].ID {
		t.Fatalf("progress lost: %+v %v", savedEvent, err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		days, err := app.RefreshSchedule(ctx, owner, persistence.Now())
		if err != nil || len(days[0].Items) != 1 || days[0].Items[0].ID != items[2].ID {
			t.Fatalf("a read refilled a manually cleared slot: %+v %v", days, err)
		}
	}
	var tasks []persistence.Task
	if err := app.Store.DB.Where("user_id = ?", owner).Find(&tasks).Error; err != nil || len(tasks) != 4 {
		t.Fatalf("source task rows lost: %+v %v", tasks, err)
	}
	for _, task := range tasks {
		if task.DeletedAt != nil || task.Status == "cancelled" {
			t.Fatalf("source task deleted or cancelled: %+v", task)
		}
	}
	var revision persistence.DailyPlanRevision
	if err := app.Store.DB.Where("plan_id = ? AND revision = ?", before.ID, result.Plan.CurrentRevision).First(&revision).Error; err != nil {
		t.Fatal(err)
	}
	var audit struct {
		DeletedItems []persistence.DailyPlanItem `json:"deleted_items"`
	}
	if err := json.Unmarshal([]byte(revision.InputSnapshot), &audit); err != nil || len(audit.DeletedItems) != 2 || revision.Reason != "manual item deletion" {
		t.Fatalf("missing immutable deletion audit: %+v %v", revision, err)
	}
	if _, err := app.TransitionSession(ctx, owner, session.ID, session.Revision, "stop", "", "Saved focus", persistence.Now()); err != nil {
		t.Fatalf("retained focus session cannot finish: %v", err)
	}
}

func TestDeleteAllPlanItemsRequiresExplicitReplanToRefill(t *testing.T) {
	app, owner, _, before, items := deletionFixture(t, 3)
	selected := make([]PlanItemDeletion, len(items))
	for index, item := range items {
		selected[index] = PlanItemDeletion{ID: item.ID, Revision: item.Revision}
	}
	result, err := app.DeletePlanItems(context.Background(), owner, before.ID, before.Revision, selected)
	if err != nil || len(result.Items) != 0 {
		t.Fatalf("delete all: %+v %v", result, err)
	}
	days, err := app.RefreshSchedule(context.Background(), owner, persistence.Now())
	if err != nil || len(days[0].Items) != 0 || !days[0].Plan.ManualItemsRemoved {
		t.Fatalf("empty day did not persist: %+v %v", days, err)
	}
	replanned, restored, err := app.ReplanDailyPlan(context.Background(), owner, before.LocalDate, before.Timezone, nil, 120, days[0].Plan.Revision)
	if err != nil || len(restored) != 3 || replanned.ManualItemsRemoved {
		t.Fatalf("explicit replan did not restore capacity: %+v %+v %v", replanned, restored, err)
	}
}

func TestDeletePlanItemsKeepsRunningDeletedSessionUsable(t *testing.T) {
	app, owner, _, before, items := deletionFixture(t, 3)
	session := persistence.WorkSession{TaskID: *items[0].TaskID, DailyPlanItemID: &items[0].ID, TargetMinutes: 25, StartedAt: persistence.Now().Add(-time.Minute)}
	if err := app.StartSession(context.Background(), owner, &session); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DeletePlanItems(context.Background(), owner, before.ID, before.Revision, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}}); err != nil {
		t.Fatal(err)
	}
	ended, err := app.TransitionSession(context.Background(), owner, session.ID, session.Revision, "stop", "", "Keep prior work", persistence.Now())
	if err != nil || ended.Status != "stopped" || ended.DailyPlanItemID == nil || *ended.DailyPlanItemID != items[0].ID {
		t.Fatalf("deleted commitment lost its live focus session: %+v %v", ended, err)
	}
}

func TestDeletePlanItemsValidatesEntireBatchAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*App, string, *persistence.DailyPlan, []persistence.DailyPlanItem) (string, []PlanItemDeletion)
		want   error
	}{
		{"stale item", func(_ *App, owner string, _ *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}, {ID: items[1].ID, Revision: items[1].Revision + 1}}
		}, ErrRevision},
		{"foreign or unknown item", func(_ *App, owner string, _ *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}, {ID: "someone-elses-item", Revision: 1}}
		}, ErrNotFound},
		{"duplicate selection", func(_ *App, owner string, _ *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}, {ID: items[0].ID, Revision: items[0].Revision}}
		}, ErrValidation},
		{"different owner", func(_ *App, _ string, _ *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			return "panel-user-b", []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}}
		}, ErrNotFound},
		{"closed plan", func(app *App, owner string, plan *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			app.Store.DB.Model(plan).Update("status", "closed")
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}}
		}, ErrConflict},
		{"future plan", func(app *App, owner string, plan *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			app.Store.DB.Model(plan).Update("local_date", "2099-01-01")
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}}
		}, ErrConflict},
		{"past plan", func(app *App, owner string, plan *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			app.Store.DB.Model(plan).Update("local_date", "2001-01-01")
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}}
		}, ErrConflict},
		{"non-core item", func(app *App, owner string, _ *persistence.DailyPlan, items []persistence.DailyPlanItem) (string, []PlanItemDeletion) {
			app.Store.DB.Model(&items[1]).Update("kind", "input")
			return owner, []PlanItemDeletion{{ID: items[0].ID, Revision: items[0].Revision}, {ID: items[1].ID, Revision: items[1].Revision}}
		}, ErrValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, owner, _, before, items := deletionFixture(t, 3)
			actor, selected := test.mutate(app, owner, &before, items)
			_, err := app.DeletePlanItems(context.Background(), actor, before.ID, before.Revision, selected)
			if !errors.Is(err, test.want) {
				t.Fatalf("want %v, got %v", test.want, err)
			}
			var current persistence.DailyPlan
			if err := app.Store.DB.First(&current, "id = ?", before.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.Revision != before.Revision || current.CurrentRevision != before.CurrentRevision || current.ManualItemsRemoved {
				t.Fatalf("failed batch mutated the plan: %+v", current)
			}
			var count int64
			if err := app.Store.DB.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND plan_revision = ?", before.ID, before.CurrentRevision).Count(&count).Error; err != nil || count != 3 {
				t.Fatalf("failed batch moved items: %d %v", count, err)
			}
		})
	}
}
