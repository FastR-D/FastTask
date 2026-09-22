package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"go.uber.org/fx"
)

// WorkerModule provides the persistent agent worker and its run loop lifecycle
// (wiring.md §3, §6).
var WorkerModule = fx.Module("worker",
	fx.Provide(NewWorker),
	fx.Invoke(registerWorkerLifecycle),
	// job_handlers value group (wiring.md §5): each domain registers the handler
	// for its job types, replacing the Worker's central job-type switch. Adding a
	// job type no longer edits the Worker — a domain provides a handler here.
	fx.Provide(
		fx.Annotate(application.NewAgentRunHandler, fx.ResultTags(`group:"job_handlers"`)),
		fx.Annotate(application.NewTaskTreeHandler, fx.ResultTags(`group:"job_handlers"`)),
		fx.Annotate(application.NewDailyPlanHandler, fx.ResultTags(`group:"job_handlers"`)),
		fx.Annotate(application.NewConversationHandler, fx.ResultTags(`group:"job_handlers"`)),
		fx.Annotate(application.NewVoiceTranscriptionHandler, fx.ResultTags(`group:"job_handlers"`)),
		fx.Annotate(application.NewSupportGenerationHandler, fx.ResultTags(`group:"job_handlers"`)),
	),
)

// workerParams collects the Worker's dependencies, including the job_handlers
// value group (wiring.md §5).
type workerParams struct {
	fx.In
	App      *application.App
	Config   config.Config
	Agent    *application.AgentService
	Handlers []application.JobHandler `group:"job_handlers"`
}

// NewWorker builds the worker with the per-job provider resolver, the agent
// runtime, and the collected job handlers. The resolver prefers the
// admin-configured default provider and falls back to environment configuration,
// matching the historical inline behaviour.
func NewWorker(p workerParams) *application.Worker {
	worker := application.NewWorker(p.App, p.Config.WorkerInterval)
	worker.WithProviderResolver(ProviderResolver(p.App, p.Config))
	worker.WithAgentRunner(p.Agent)
	worker.WithJobHandlers(p.Handlers)
	return worker
}

// ProviderResolver returns the Worker's per-job model provider resolver.
func ProviderResolver(app *application.App, cfg config.Config) func(context.Context) (agent.Provider, agent.Transcriber, error) {
	return func(ctx context.Context) (agent.Provider, agent.Transcriber, error) {
		runtime, err := app.ActiveProviderRuntime(ctx)
		if err != nil || runtime != nil {
			if runtime != nil {
				return runtime.Provider, runtime.Transcriber, nil
			}
			return nil, nil, err
		}
		if cfg.HasLLM() {
			provider := agent.NewOpenAI(cfg)
			var transcriber agent.Transcriber
			if cfg.TranscriptionModel != "" {
				transcriber = provider
			}
			return provider, transcriber, nil
		}
		return nil, nil, nil
	}
}

func registerWorkerLifecycle(lc fx.Lifecycle, worker *application.Worker, agent *application.AgentService, obs LifecycleObserver) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// §4.2: v1 does not resume a run across a process restart. Runs left
			// queued/running by a crashed predecessor are marked interrupted BEFORE
			// the worker claims any job, so ExecuteRun short-circuits on them
			// (terminal status) instead of re-executing a stale run. Their partial
			// messages are preserved for the user to review and retry.
			if _, err := agent.Repository().MarkInterruptedRuns(ctx, "INTERRUPTED"); err != nil {
				return err
			}
			var runCtx context.Context
			runCtx, cancel = context.WithCancel(context.Background())
			done = make(chan struct{})
			obs.Started("worker")
			go func() {
				defer close(done)
				worker.Run(runCtx)
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			obs.Stopped("worker")
			if cancel != nil {
				cancel()
			}
			if done == nil {
				return nil
			}
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	})
}
