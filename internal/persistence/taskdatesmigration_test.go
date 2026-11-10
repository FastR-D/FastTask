package persistence

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskDateMigrationPreservesVersion23Data(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "dates-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	entries, err := embeddedMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "000024" || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		body, err := embeddedMigrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), noTransactionMarker) {
			err = execSchemaRewrite(ctx, store.DB, string(body))
		} else {
			err = store.DB.Exec(string(body)).Error
		}
		if err != nil {
			t.Fatalf("baseline %s: %v", entry.Name(), err)
		}
	}
	now := Now()
	user := User{ID: "dates-user", Identifier: "dates-user", PasswordHash: "fixture", DisplayName: "Legacy", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	goal := Goal{ID: "dates-goal", UserID: user.ID, Title: "Existing goal", SuccessCriteria: "Keep data", Status: "active", Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Create(&goal).Error; err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "dates-task", UserID: user.ID, GoalID: goal.ID, Type: "task", Title: "Existing task", Status: "in_progress", SuccessCriteria: "Original criterion", MinimumAction: "Original step", EstimateMinutes: 40, Revision: 7, CreatedAt: now, UpdatedAt: now, DependsOn: []string{}}
	if err := store.DB.Omit("PlannedStart", "PlannedEnd").Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal("migration is not repeatable", err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var got Task
	if err := store.DB.First(&got, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.PlannedStart != nil || got.PlannedEnd != nil || got.Revision != 7 || got.Status != task.Status || got.Title != task.Title || got.MinimumAction != task.MinimumAction || !got.CreatedAt.Equal(now) {
		t.Fatalf("upgrade rewrote existing task: %+v", got)
	}
}
