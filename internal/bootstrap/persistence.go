package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"go.uber.org/fx"
)

// PersistenceModule provides the Store and owns its open/migrate/close
// lifecycle (wiring.md §3, §6).
var PersistenceModule = fx.Module("persistence",
	fx.Provide(NewStore),
	fx.Invoke(registerStoreLifecycle),
)

// NewStore opens the SQLite database. Migration and schema validation run in the
// OnStart hook so startup ordering is explicit and failures surface as fx start
// errors.
func NewStore(cfg config.Config) (*persistence.Store, error) {
	return persistence.Open(cfg.DatabasePath)
}

func registerStoreLifecycle(lc fx.Lifecycle, store *persistence.Store, obs LifecycleObserver) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			obs.Started("store")
			if err := store.Migrate(ctx); err != nil {
				return err
			}
			return store.Ready(ctx)
		},
		OnStop: func(context.Context) error {
			obs.Stopped("store")
			return store.Close()
		},
	})
}
