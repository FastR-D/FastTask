package scheduler

import (
	"context"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/go-co-op/gocron/v2"
	"gorm.io/gorm"
)

type Scheduler struct {
	store     *persistence.Store
	scheduler gocron.Scheduler
}

func New(store *persistence.Store) (*Scheduler, error) {
	inner, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	s := &Scheduler{store: store, scheduler: inner}
	if _, err := inner.NewJob(gocron.DurationJob(time.Minute), gocron.NewTask(s.maintain)); err != nil {
		return nil, err
	}
	return s, nil
}

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
