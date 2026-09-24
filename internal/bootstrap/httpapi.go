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
	fx.Provide(NewRouteDeps),
	fx.Provide(NewAPI),
	fx.Provide(NewHTTPServer),
	fx.Invoke(registerHTTPLifecycle),
	// routes value group (wiring.md §5, §7 step 7): each domain provides its own
	// RouteRegistrar; NewAPI collects the group and iterates it at assembly, so
	// adding a domain never edits a central dispatcher. huma marshals OpenAPI
	// paths/schemas as sorted maps, so the group's nondeterministic order does not
	// perturb the document (asserted byte-identical by TestOpenAPIMatchesGolden).
	fx.Provide(
		fx.Annotate(httpapi.NewAuthRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewMeRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewGoalRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewTaskRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewTaskTreeRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewLensRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewPlanRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewSessionRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewConversationRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewJobRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewDeviceRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewPanelRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewImportRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewIntegrationStatusRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewAdminRoutes, fx.ResultTags(`group:"routes"`)),
		fx.Annotate(httpapi.NewNotificationRoutes, fx.ResultTags(`group:"routes"`)),
		// The harness registrar needs the agent runtime as well as the shared route
		// dependencies (doc/interface.md §20.2).
		fx.Annotate(httpapi.NewHarnessRoutes, fx.ResultTags(`group:"routes"`)),
	),
)

// NewRouteDeps assembles the shared dependencies every route registrar needs.
// cfg is supplied by fx from config.Load (or a test config), which already
// guarantees AudioDir/IntegrationTimeout are set, so registrars read proper
// values. Production never mutates cfg after construction; the only live-cfg
// mutation is a test that uses the builtin &server.cfg path inside httpapi.New.
func NewRouteDeps(app *application.App, auth *platformauth.Service, cfg config.Config) httpapi.RouteDeps {
	return httpapi.NewRouteDeps(app, auth, &cfg)
}

// apiParams collects the Server's dependencies, including the routes value group.
type apiParams struct {
	fx.In
	App    *application.App
	Auth   *platformauth.Service
	Cfg    config.Config
	Agent  *application.AgentService
	Routes []httpapi.RouteRegistrar `group:"routes"`
}

// NewAPI builds the HTTP API surface (Gin engine + Huma registry), registering the
// collected route registrars.
func NewAPI(p apiParams) *httpapi.Server {
	return httpapi.New(p.App, p.Auth, p.Cfg, p.Agent, p.Routes...)
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
