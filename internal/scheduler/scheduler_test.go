package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestMaintainRequeuesStalledJobsUntilAttemptsRunOut(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "u", PasswordHash: "x", DisplayName: "U", Timezone: "UTC", Locale: "en", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	lapsed := now.Add(-time.Minute)
	live := now.Add(time.Minute)
	jobs := map[string]persistence.AgentJob{
		"retry":  {AttemptCount: 1, MaxAttempts: 3, LockedUntil: &lapsed},
		"final":  {AttemptCount: 3, MaxAttempts: 3, LockedUntil: &lapsed},
		"active": {AttemptCount: 1, MaxAttempts: 3, LockedUntil: &live},
	}
	ids := map[string]string{}
	for name, job := range jobs {
		job.ID, job.UserID, job.Type, job.Status = persistence.NewID("job"), user.ID, "task_tree_generation", "running"
		job.InputJSON, job.RunTokenHash, job.LockedBy, job.Revision = "{}", "hash", "worker_x", 1
		job.RunAfter, job.CreatedAt, job.UpdatedAt = now, now, now
		if err := store.DB.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
		ids[name] = job.ID
	}
	s := &Scheduler{store: store}
	s.maintain()
	want := map[string]string{"retry": "queued", "final": "failed", "active": "running"}
	for name, status := range want {
		var job persistence.AgentJob
		if err := store.DB.First(&job, "id = ?", ids[name]).Error; err != nil {
			t.Fatal(err)
		}
		if job.Status != status {
			t.Fatalf("%s job status=%s, want %s", name, job.Status, status)
		}
		if name == "final" && job.ErrorCode != "LEASE_EXPIRED" {
			t.Fatalf("final job error code=%q", job.ErrorCode)
		}
	}
}
