package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/application"
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

// NewScheduler builds the maintenance scheduler with the harness sweepers registered.
//
// The reaper lives here rather than in its own goroutine (doc/harness.md §10.4, §11): the
// periodic scan is the second half of run recovery, whose first half is MarkInterruptedRuns at
// process start, and both are lifecycle concerns the scheduler already owns.
func NewScheduler(store *persistence.Store, agent *application.AgentService) (*scheduler.Scheduler, error) {
	return scheduler.New(store,
		// Interrupt the runs whose host stopped beating, and finish the cancellations no host
		// confirmed.
		func(ctx context.Context) error { _, err := agent.ReapHarnessRuns(ctx); return err },
		// Drop capability tokens that can no longer be used.
		func(ctx context.Context) error { _, err := agent.PruneHarnessTokens(ctx); return err },
	)
}

func registerSchedulerLifecycle(lc fx.Lifecycle, maintenance *scheduler.Scheduler, obs LifecycleObserver) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { obs.Started("scheduler"); maintenance.Start(); return nil },
		OnStop:  func(context.Context) error { obs.Stopped("scheduler"); return maintenance.Shutdown() },
	})
}
