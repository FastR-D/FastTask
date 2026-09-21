package bootstrap

import (
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

// NewAgentService builds the agent runtime over the store. It is shared by the
// HTTP endpoints (submit/stream) and the Worker (execute) so both see the same
// run lifecycle.
func NewAgentService(store *persistence.Store) *application.AgentService {
	return application.NewAgentService(store)
}
