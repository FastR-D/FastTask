package bootstrap

import (
	"context"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"go.uber.org/fx"
)

// AuthModule provides the auth service and seeds the bootstrap administrator on
// start. Its OnStart is appended after the store's, so EnsureAdmin always runs
// against a migrated database (wiring.md §6).
var AuthModule = fx.Module("auth",
	fx.Provide(NewAuthService),
	fx.Invoke(registerAuthLifecycle),
)

// NewAuthService is a plain constructor; it can be called without fx.
func NewAuthService(store *persistence.Store, cfg config.Config) *platformauth.Service {
	return platformauth.New(store, cfg)
}

func registerAuthLifecycle(lc fx.Lifecycle, auth *platformauth.Service, obs LifecycleObserver) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			obs.Started("auth")
			return auth.EnsureAdmin(ctx)
		},
		OnStop: func(context.Context) error { obs.Stopped("auth"); return nil },
	})
}
