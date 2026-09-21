package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/FastR-D/FastTask/internal/scheduler"
	"go.uber.org/fx"
)

// SchedulerModule provides the gocron maintenance scheduler and its lifecycle
// (wiring.md §3, §6).
var SchedulerModule = fx.Module("scheduler",
	fx.Provide(NewScheduler),
	fx.Invoke(registerSchedulerLifecycle),
)

// NewScheduler builds the maintenance scheduler.
func NewScheduler(store *persistence.Store) (*scheduler.Scheduler, error) {
	return scheduler.New(store)
}

func registerSchedulerLifecycle(lc fx.Lifecycle, maintenance *scheduler.Scheduler, obs LifecycleObserver) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { obs.Started("scheduler"); maintenance.Start(); return nil },
		OnStop: func(context.Context) error { obs.Stopped("scheduler"); return maintenance.Shutdown() },
	})
}
