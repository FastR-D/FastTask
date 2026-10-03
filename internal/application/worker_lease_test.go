package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

type scriptedHandler struct {
	run func(ctx context.Context, job persistence.AgentJob) (any, error)
}

func (scriptedHandler) JobTypes() []string { return []string{"task_tree_generation"} }
func (h scriptedHandler) HandleJob(ctx context.Context, job persistence.AgentJob, _ JobRuntime) (any, error) {
	return h.run(ctx, job)
}

func lockedUntil(t *testing.T, f fixture, id string) time.Time {
	t.Helper()
	var job persistence.AgentJob
	if err := f.store.DB.First(&job, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if job.LockedUntil == nil {
		return time.Time{}
	}
	return *job.LockedUntil
}

// An attempt that outlives one lease keeps it, so the scheduler does not hand the job to a second worker.
func TestWorkerRenewsTheLeaseWhileAJobRuns(t *testing.T) {
	f := newFixture(t)
	job, err := f.app.CreateJob(context.Background(), f.user.ID, "task_tree_generation", "goal", f.goal.ID, 4, map[string]any{"instruction": "split"})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(f.app, time.Millisecond).WithJobHandlers([]JobHandler{scriptedHandler{run: func(ctx context.Context, running persistence.AgentJob) (any, error) {
		first := lockedUntil(t, f, running.ID)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if lockedUntil(t, f, running.ID).After(first) {
				return nil, errors.New("renewed; ending the attempt")
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil, errors.New("lease was never renewed")
	}}})
	worker.leaseRenewEvery = 10 * time.Millisecond
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.First(job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.ErrorMessage != "renewed; ending the attempt" {
		t.Fatalf("attempt ended with %q", job.ErrorMessage)
	}
}

// When the claim is taken away mid-attempt (the job was fenced by a newer lease), the attempt is told
// to stop instead of running to completion for a result that will be discarded.
func TestWorkerStopsAnAttemptWhoseLeaseWasLost(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.CreateJob(context.Background(), f.user.ID, "task_tree_generation", "goal", f.goal.ID, 4, map[string]any{"instruction": "split"}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	worker := NewWorker(f.app, time.Millisecond).WithJobHandlers([]JobHandler{scriptedHandler{run: func(ctx context.Context, running persistence.AgentJob) (any, error) {
		if err := f.store.DB.Model(&persistence.AgentJob{}).Where("id = ?", running.ID).UpdateColumn("lease_version", running.LeaseVersion+1).Error; err != nil {
			t.Error(err)
		}
		select {
		case <-ctx.Done():
			stopped <- ctx.Err()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			stopped <- nil
			return nil, errors.New("attempt was not cancelled")
		}
	}}})
	worker.leaseRenewEvery = 10 * time.Millisecond
	if err := worker.RunOnce(context.Background()); !errors.Is(err, ErrStaleAgentAttempt) {
		t.Fatalf("RunOnce=%v, want the stale-attempt error", err)
	}
	if err := <-stopped; err == nil {
		t.Fatal("attempt kept running after its lease was lost")
	}
}
