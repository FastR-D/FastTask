package scheduler

import (
	"context"
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
}

func New(store *persistence.Store, sweepers ...Sweeper) (*Scheduler, error) {
	inner, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	s := &Scheduler{store: store, scheduler: inner, sweepers: append([]Sweeper(nil), sweepers...)}
	if _, err := inner.NewJob(gocron.DurationJob(time.Minute), gocron.NewTask(s.maintain)); err != nil {
		return nil, err
	}
	if len(s.sweepers) > 0 {
		if _, err := inner.NewJob(gocron.DurationJob(sweepInterval), gocron.NewTask(s.sweep)); err != nil {
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
	for _, sweeper := range s.sweepers {
		sweepCtx, cancel := context.WithTimeout(ctx, sweepInterval)
		_ = sweeper(sweepCtx)
		cancel()
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
	s.store.DB.WithContext(ctx).Model(&persistence.AgentJob{}).
		Where("status = 'running' AND locked_until IS NOT NULL AND locked_until < ?", now).
		Updates(map[string]any{"status": "queued", "locked_by": "", "locked_until": nil, "run_token_hash": "", "run_after": now, "revision": gorm.Expr("revision + 1"), "updated_at": now})
	s.store.DB.WithContext(ctx).Model(&persistence.OutboxEvent{}).
		Where("status = 'pending'").
		Updates(map[string]any{"status": "processed", "processed_at": now})
	s.store.DB.WithContext(ctx).Where("status_code > 0 AND expires_at < ?", now).Delete(&persistence.IdempotencyRecord{})
}
