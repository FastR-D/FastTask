package bootstrap

import (
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"go.uber.org/fx"
)

// ApplicationModule provides the application aggregate. wiring.md §4 splits App
// into per-aggregate services in later steps; step 2 keeps the single App so the
// fx migration is behaviour-preserving.
var ApplicationModule = fx.Module("application",
	fx.Provide(NewApp),
)

// NewApp builds the application aggregate with the provider encryption key.
func NewApp(store *persistence.Store, cfg config.Config) *application.App {
	return application.NewWithSecret(store, cfg.ProviderEncryptionKey)
}
