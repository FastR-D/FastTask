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
)

// NewWorker builds the worker with the per-job provider resolver and the agent
// runtime. The resolver prefers the admin-configured default provider and falls
// back to environment configuration, matching the historical inline behaviour.
func NewWorker(app *application.App, cfg config.Config, agent *application.AgentService) *application.Worker {
	worker := application.NewWorker(app, cfg.WorkerInterval)
	worker.WithProviderResolver(ProviderResolver(app, cfg))
	worker.WithAgentRunner(agent)
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

func registerWorkerLifecycle(lc fx.Lifecycle, worker *application.Worker, obs LifecycleObserver) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			done = make(chan struct{})
			obs.Started("worker")
			go func() {
				defer close(done)
				worker.Run(ctx)
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
