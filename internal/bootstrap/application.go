package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"go.uber.org/fx"
)

// ApplicationModule provides the application aggregate. wiring.md §4 splits App
// into per-aggregate services in later steps; step 2 keeps the single App so the
// fx migration is behaviour-preserving. The AgentService is provided separately
// so the HTTP layer and the Worker share one instance (doc/agent-impl.md §8).
var ApplicationModule = fx.Module("application",
	fx.Provide(NewApp),
	fx.Provide(NewAgentService),
	// job_materializers value group (wiring.md §5, §4.1): each domain registers the
	// materializer that folds its job types' output into business tables. NewApp
	// collects the group and hands it to JobService, replacing the old central
	// switch in MaterializeJobResult. Provided in Core so every role that builds an
	// App (serve, worker, scheduler) can satisfy the group.
	fx.Provide(
		fx.Annotate(application.NewTaskTreeMaterializer, fx.ResultTags(`group:"job_materializers"`)),
		fx.Annotate(application.NewConversationMaterializer, fx.ResultTags(`group:"job_materializers"`)),
		fx.Annotate(application.NewSupportMaterializer, fx.ResultTags(`group:"job_materializers"`)),
	),
)

// appParams collects the App's dependencies, including the job_materializers
// value group (wiring.md §5).
type appParams struct {
	fx.In
	Store         *persistence.Store
	Config        config.Config
	Materializers []application.JobMaterializer `group:"job_materializers"`
}

// NewApp builds the application aggregate with the provider encryption key and
// the collected job materializers.
func NewApp(p appParams) *application.App {
	return application.NewWithSecret(p.Store, p.Config.ProviderEncryptionKey, p.Materializers...)
}

// NewAgentService builds the agent runtime over the App. It is shared by the
// HTTP endpoints (submit/stream) and the Worker (execute) so both see the same
// run lifecycle. The chat resolver mirrors the Worker's provider resolver: it
// prefers the admin-configured default provider and falls back to environment
// configuration, returning nil (deterministic fallback) when no model is set.
func NewAgentService(app *application.App, cfg config.Config) *application.AgentService {
	return application.NewAgentService(app, application.WithChatResolver(ChatResolver(app, cfg)))
}

// ChatResolver returns a resolver for the tool-calling model. It resolves the
// active provider the same way ProviderResolver does and adapts it to the
// agent.ChatProvider port. A configured provider that cannot do tool calling
// still resolves here; the loop surfaces PROVIDER_NO_TOOL_SUPPORT at call time
// rather than silently degrading (agent-impl.md §5.1.1).
func ChatResolver(app *application.App, cfg config.Config) application.ChatResolver {
	return func(ctx context.Context) (agent.ChatProvider, error) {
		runtime, err := app.ActiveProviderRuntime(ctx)
		if err != nil {
			return nil, err
		}
		if runtime != nil {
			if chat, ok := runtime.Provider.(agent.ChatProvider); ok {
				return chat, nil
			}
			// Admin-configured provider predates tool calling; do not fabricate.
			return nil, nil
		}
		if cfg.HasLLM() {
			return agent.NewOpenAI(cfg), nil
		}
		return nil, nil
	}
}
