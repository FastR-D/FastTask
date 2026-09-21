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
)

// NewApp builds the application aggregate with the provider encryption key.
func NewApp(store *persistence.Store, cfg config.Config) *application.App {
	return application.NewWithSecret(store, cfg.ProviderEncryptionKey)
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
