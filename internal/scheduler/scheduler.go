package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/go-co-op/gocron/v2"
	"gorm.io/gorm"
)

// Sweeper is a periodic maintenance callback. The scheduler owns the timing so a new
// periodic task does not mean a new goroutine (doc/harness.md §10.4, §11).
type Sweeper func(ctx context.Context) error

type Scheduler struct {
	store     *persistence.Store
	scheduler gocron.Scheduler
	sweepers  []Sweeper

	mu           sync.Mutex
	lastSweepErr map[int]string
}

func New(store *persistence.Store, sweepers ...Sweeper) (*Scheduler, error) {
	inner, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	s := &Scheduler{store: store, scheduler: inner, sweepers: append([]Sweeper(nil), sweepers...)}
	if _, err := inner.NewJob(gocron.DurationJob(time.Minute), gocron.NewTask(s.maintain), gocron.WithSingletonMode(gocron.LimitModeReschedule)); err != nil {
		return nil, err
	}
	if len(s.sweepers) > 0 {
		if _, err := inner.NewJob(gocron.DurationJob(sweepInterval), gocron.NewTask(s.sweep), gocron.WithSingletonMode(gocron.LimitModeReschedule)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// sweepInterval is how often the registered sweepers run. It is well under the harness
// heartbeat-loss threshold so a host that disappears is noticed close to when the spec says
// it should be (doc/harness.md §10.4: 45 seconds).
const sweepInterval = 5 * time.Second

// sweep runs every registered sweeper on the scheduler's clock.
func (s *Scheduler) sweep() { s.SweepOnce(context.Background()) }

// SweepOnce runs every registered sweeper immediately, each with its own short timeout so one slow
// sweep cannot starve the others or hold a context open across intervals. It is exported so a test —
// or an operator command — can run maintenance without waiting for the ticker.
func (s *Scheduler) SweepOnce(ctx context.Context) {
	for i, sweeper := range s.sweepers {
		sweepCtx, cancel := context.WithTimeout(ctx, sweepInterval)
		err := sweeper(sweepCtx)
		cancel()
		s.reportSweep(i, err)
	}
}

// reportSweep logs a sweeper's failure when it starts and when it clears, not on every five-second
// pass: a database outage should be one line per sweeper, not seven hundred an hour.
func (s *Scheduler) reportSweep(index int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSweepErr == nil {
		s.lastSweepErr = map[int]string{}
	}
	previous, failing := s.lastSweepErr[index]
	switch {
	case err != nil && (!failing || previous != err.Error()):
		s.lastSweepErr[index] = err.Error()
		slog.Error("maintenance sweep failed", "sweeper", index, "err", err)
	case err == nil && failing:
		delete(s.lastSweepErr, index)
		slog.Info("maintenance sweep recovered", "sweeper", index)
	}
}

// SweeperCount reports how many periodic tasks are registered, so the composition root can be
// asserted to have wired the ones a role depends on (doc/harness.md §11).
func (s *Scheduler) SweeperCount() int { return len(s.sweepers) }

func (s *Scheduler) Start() { s.scheduler.Start() }

func (s *Scheduler) Shutdown() error { return s.scheduler.Shutdown() }

func (s *Scheduler) maintain() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := persistence.Now()
	// A lease lapses only when its worker stopped renewing it: the process died or hung. Requeue the job
	// while it has attempts left; one that has used them all is failed, so a job that kills its worker
	// every time (out of memory on a pathological input) cannot crash-loop the deployment forever.
	expired := "status = 'running' AND locked_until IS NOT NULL AND locked_until < ?"
	failed := s.store.DB.WithContext(ctx).Model(&persistence.AgentJob{}).
		Where(expired+" AND attempt_count >= max_attempts", now).
		Updates(map[string]any{"status": "failed", "error_code": "LEASE_EXPIRED", "error_message": "the worker stopped responding on the final attempt", "locked_by": "", "locked_until": nil, "run_token_hash": "", "finished_at": now, "revision": gorm.Expr("revision + 1"), "updated_at": now})
	requeued := s.store.DB.WithContext(ctx).Model(&persistence.AgentJob{}).
		Where(expired, now).
		Updates(map[string]any{"status": "queued", "locked_by": "", "locked_until": nil, "run_token_hash": "", "run_after": now, "revision": gorm.Expr("revision + 1"), "updated_at": now})
	for name, result := range map[string]*gorm.DB{"failed": failed, "requeued": requeued} {
		if result.Error != nil {
			slog.Error("maintenance: expired job leases not recovered", "action", name, "err", result.Error)
		} else if result.RowsAffected > 0 {
			slog.Warn("maintenance: recovered jobs whose worker stopped renewing its lease", "action", name, "jobs", result.RowsAffected)
		}
	}
	s.store.DB.WithContext(ctx).Model(&persistence.OutboxEvent{}).
		Where("status = 'pending'").
		Updates(map[string]any{"status": "processed", "processed_at": now})
	s.store.DB.WithContext(ctx).Where("status_code > 0 AND expires_at < ?", now).Delete(&persistence.IdempotencyRecord{})
	// A record left in progress by a process that died mid-request would answer 409 for that key until
	// it expired a month later; no API request runs for an hour.
	s.store.DB.WithContext(ctx).Where("status_code = 0 AND created_at < ?", now.Add(-time.Hour)).Delete(&persistence.IdempotencyRecord{})
	// Builds before the credential exclusion stored sign-in, refresh and device-token responses
	// verbatim. Purge any that remain rather than wait out their expiry.
	s.store.DB.WithContext(ctx).Where("path IN ? OR (path LIKE ? AND method = 'POST')", []string{"/api/v1/auth/login", "/api/v1/auth/refresh"}, "/api/v1/devices%").Where("response_json NOT LIKE ?", `%"withheld":true%`).Delete(&persistence.IdempotencyRecord{})
}
