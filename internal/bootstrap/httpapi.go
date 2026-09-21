package bootstrap

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/httpapi"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"go.uber.org/fx"
)

// defaultWriteTimeout is the per-request write deadline applied to ordinary JSON
// endpoints. The server-level WriteTimeout is 0 so that SSE streams are not cut
// off; each request sets its own deadline through http.ResponseController.
// Streaming handlers clear or extend it. See doc/agent-impl.md §9.2 and
// doc/wiring.md §6.
const defaultWriteTimeout = 60 * time.Second

// HTTPModule provides the Gin/Huma server and the net/http server lifecycle.
var HTTPModule = fx.Module("httpapi",
	fx.Provide(NewAPI),
	fx.Provide(NewHTTPServer),
	fx.Invoke(registerHTTPLifecycle),
)

// NewAPI builds the HTTP API surface (Gin engine + Huma registry).
func NewAPI(app *application.App, auth *platformauth.Service, cfg config.Config) *httpapi.Server {
	return httpapi.New(app, auth, cfg)
}

// NewHTTPServer builds the net/http server. WriteTimeout is deliberately 0: the
// pre-fx 60 second value would sever any SSE stream longer than a minute. The
// equivalent protection is restored per request by writeDeadlineHandler, which
// uses http.ResponseController so streaming endpoints can opt out.
func NewHTTPServer(api *httpapi.Server, cfg config.Config) *http.Server {
	return &http.Server{
		Addr:              cfg.Address(),
		Handler:           writeDeadlineHandler(api.Engine, defaultWriteTimeout),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// writeDeadlineHandler wraps next so every request gets a write deadline of d,
// restoring the protection the removed server-level WriteTimeout provided. A
// handler that streams (SSE) can clear the deadline by calling
// http.NewResponseController(w).SetWriteDeadline(time.Time{}) during the
// request; because the deadline is set before next runs, streaming handlers
// simply override it.
func writeDeadlineHandler(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d > 0 {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
		}
		next.ServeHTTP(w, r)
	})
}

func registerHTTPLifecycle(lc fx.Lifecycle, server *http.Server, cfg config.Config, shutdowner fx.Shutdowner, obs LifecycleObserver) {
	var listener net.Listener
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", server.Addr)
			if err != nil {
				return err
			}
			listener = ln
			obs.Started("http")
			fmt.Printf("FastTask listening on %s\n", cfg.Address())
			go func() {
				if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
					_ = shutdowner.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			obs.Stopped("http")
			err := server.Shutdown(ctx)
			if listener != nil {
				_ = listener.Close()
			}
			return err
		},
	})
}
