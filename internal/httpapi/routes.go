package httpapi

import (
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/danielgtaylor/huma/v2"
)

// Route registration via a value group (wiring.md §5, §7 step 7).
//
// Each domain provides a RouteRegistrar holding the services it needs; the fx
// composition root collects them into the "routes" value group and the Server
// iterates the group at assembly time. Adding a domain no longer edits a central
// dispatcher. Registrars are distinct named types (not bare functions) so fx error
// messages identify the source (§5).
//
// The refactor is behaviour-preserving: every registrar body is the former
// registerX method verbatim, with the huma.API arriving as the RegisterRoutes
// argument instead of the Server field. Because huma marshals OpenAPI paths and
// component schemas as sorted maps, registration order does not affect the
// document — TestOpenAPIMatchesGolden asserts the output is byte-identical to the
// pre-refactor baseline (§7 step 7 acceptance: "OpenAPI 输出逐字节不变").

// RouteRegistrar registers one domain's HTTP routes onto the huma API.
type RouteRegistrar interface {
	RegisterRoutes(api huma.API)
}

// RouteDeps carries the application services route handlers need. Registrars embed
// it so their bodies reference s.app / s.auth / s.cfg exactly as the former
// *Server methods did.
type RouteDeps struct {
	app  *application.App
	auth *platformauth.Service
	// cfg is a pointer so registrars observe the live server config. The
	// historical handlers were *Server methods reading s.cfg at request time;
	// integration tests rely on that (they set FastReadURL/FastWriteURL after
	// construction, once the httptest listeners exist). A value copy would
	// snapshot cfg at registration and miss those writes.
	cfg *config.Config
}

// NewRouteDeps builds the shared route dependencies. cfg must point at the
// config the Server will read at request time (&server.cfg for the builtin
// path) so handlers see live values exactly as the old *Server methods did.
func NewRouteDeps(app *application.App, auth *platformauth.Service, cfg *config.Config) RouteDeps {
	return RouteDeps{app: app, auth: auth, cfg: cfg}
}

// The per-domain registrars. Each embeds RouteDeps and implements RegisterRoutes;
// the route definitions live in the corresponding registerX-derived method.
type (
	authRoutes              struct{ RouteDeps }
	meRoutes                struct{ RouteDeps }
	goalRoutes              struct{ RouteDeps }
	taskRoutes              struct{ RouteDeps }
	taskTreeRoutes          struct{ RouteDeps }
	lensRoutes              struct{ RouteDeps }
	planRoutes              struct{ RouteDeps }
	sessionRoutes           struct{ RouteDeps }
	conversationRoutes      struct{ RouteDeps }
	jobRoutes               struct{ RouteDeps }
	deviceRoutes            struct{ RouteDeps }
	panelRoutes             struct{ RouteDeps }
	importRoutes            struct{ RouteDeps }
	integrationStatusRoutes struct{ RouteDeps }
	adminRoutes             struct{ RouteDeps }
	// harnessRoutes needs the agent runtime in addition to the shared dependencies: the
	// harness endpoints are the agent service's HTTP surface (doc/interface.md §20.2).
	harnessRoutes struct {
		RouteDeps
		agent *application.AgentService
	}
)

// Constructors returning the interface, one per domain, for the fx "routes" group.
func NewAuthRoutes(d RouteDeps) RouteRegistrar              { return authRoutes{d} }
func NewMeRoutes(d RouteDeps) RouteRegistrar                { return meRoutes{d} }
func NewGoalRoutes(d RouteDeps) RouteRegistrar              { return goalRoutes{d} }
func NewTaskRoutes(d RouteDeps) RouteRegistrar              { return taskRoutes{d} }
func NewTaskTreeRoutes(d RouteDeps) RouteRegistrar          { return taskTreeRoutes{d} }
func NewLensRoutes(d RouteDeps) RouteRegistrar              { return lensRoutes{d} }
func NewPlanRoutes(d RouteDeps) RouteRegistrar              { return planRoutes{d} }
func NewSessionRoutes(d RouteDeps) RouteRegistrar           { return sessionRoutes{d} }
func NewConversationRoutes(d RouteDeps) RouteRegistrar      { return conversationRoutes{d} }
func NewJobRoutes(d RouteDeps) RouteRegistrar               { return jobRoutes{d} }
func NewDeviceRoutes(d RouteDeps) RouteRegistrar            { return deviceRoutes{d} }
func NewPanelRoutes(d RouteDeps) RouteRegistrar             { return panelRoutes{d} }
func NewImportRoutes(d RouteDeps) RouteRegistrar            { return importRoutes{d} }
func NewIntegrationStatusRoutes(d RouteDeps) RouteRegistrar { return integrationStatusRoutes{d} }
func NewAdminRoutes(d RouteDeps) RouteRegistrar             { return adminRoutes{d} }

// NewHarnessRoutes builds the harness registrar (doc/interface.md §20.2).
func NewHarnessRoutes(d RouteDeps, agent *application.AgentService) RouteRegistrar {
	return harnessRoutes{RouteDeps: d, agent: agent}
}

// BuiltinRouteRegistrars returns every domain registrar in the historical
// registration order. It is the default set used when a Server is built without an
// injected value group (unit tests construct the Server directly, wiring.md §2
// rule 4); the fx composition root supplies the same registrars via group:"routes".
func BuiltinRouteRegistrars(d RouteDeps, agent *application.AgentService) []RouteRegistrar {
	return append([]RouteRegistrar{
		authRoutes{d}, meRoutes{d}, goalRoutes{d}, taskRoutes{d}, taskTreeRoutes{d},
		lensRoutes{d}, planRoutes{d}, sessionRoutes{d}, conversationRoutes{d},
		jobRoutes{d}, deviceRoutes{d}, panelRoutes{d}, importRoutes{d},
		integrationStatusRoutes{d}, adminRoutes{d},
	}, NewHarnessRoutes(d, agent))
}
