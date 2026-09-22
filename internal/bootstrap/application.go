package bootstrap

import (
	"context"

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

// NewAgentService builds the agent runtime over the App. It is shared by the HTTP endpoints
// (submit/stream/proxy) and the Worker (sidecar runs) so both see the same run lifecycle.
func NewAgentService(app *application.App, cfg config.Config, sidecar *SidecarSupervisor) *application.AgentService {
	options := []application.AgentOption{
		application.WithCredentialsResolver(ModelCredentialsResolver(app, cfg)),
		application.WithReasoningLevel(cfg.AgentReasoning),
		application.WithReasoningPersistence(cfg.AgentReasoningPersist),
		application.WithAttachmentDir(cfg.AttachmentDir),
	}
	// A sidecar that is not configured leaves the driver nil, and SubmitCommands then refuses sidecar mode
	// with HARNESS_UNAVAILABLE instead of queueing a job nobody will run (§1.2).
	if driver := sidecar.Driver(); driver != nil {
		options = append(options, application.WithSidecarDriver(driver))
	}
	return application.NewAgentService(app, options...)
}

// ModelCredentialsResolver resolves the upstream model the harness proxy injects
// (doc/harness.md §4.3). It prefers the admin-configured default provider and falls back to
// environment configuration, returning nil when no model is set — which makes the harness
// unavailable rather than silently degrading, and leaves the deterministic reply to the
// server-driven path (§3.2, §1.2).
//
// The decrypted key stops here: it is handed to the proxy, which puts it on an upstream
// request and never returns it to a caller (§4.5).
func ModelCredentialsResolver(app *application.App, cfg config.Config) application.CredentialsResolver {
	return func(ctx context.Context) (*application.ModelCredentials, error) {
		runtime, err := app.ActiveProviderRuntime(ctx)
		if err != nil {
			return nil, err
		}
		if runtime != nil {
			key, err := app.DecryptProviderKey(runtime.Record.APIKeyCiphertext)
			if err != nil {
				return nil, err
			}
			return &application.ModelCredentials{
				BaseURL: runtime.Record.BaseURL, Model: runtime.Record.ModelName, APIKey: key,
			}, nil
		}
		if cfg.HasLLM() {
			return &application.ModelCredentials{
				BaseURL: cfg.OpenAIBaseURL, Model: cfg.OpenAIModel, APIKey: cfg.OpenAIAPIKey,
			}, nil
		}
		return nil, nil
	}
}
