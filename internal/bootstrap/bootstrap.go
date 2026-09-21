// Package bootstrap assembles the FastTask runtime from its building blocks.
//
// This package is step 1 of doc/wiring.md: the assembly that previously lived
// inline in cmd/fasttask/main.go is moved here verbatim so that main.go only
// contains Cobra command definitions. A later step rewrites this package on top
// of go.uber.org/fx; the exported entry points are designed to stay stable
// across that transition.
package bootstrap

import (
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/httpapi"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/FastR-D/FastTask/internal/scheduler"
)

// ServeOptions selects which long-running roles run alongside the HTTP server.
type ServeOptions struct {
	WithWorker    bool
	WithScheduler bool
}

// Serve assembles the full runtime and blocks until the context is cancelled or
// the HTTP server fails. It mirrors the previous inline serve command exactly,
// including the 15 second graceful shutdown budget and the default worker and
// scheduler roles.
func Serve(ctx context.Context, cfg config.Config, opts ServeOptions) error {
	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	authService := platformauth.New(store, cfg)
	if err := authService.EnsureAdmin(ctx); err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	}
	app := application.NewWithSecret(store, cfg.ProviderEncryptionKey)
	server := httpapi.New(app, authService, cfg)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	if opts.WithWorker || opts.WithScheduler {
		worker := application.NewWorker(app, cfg.WorkerInterval)
		worker.WithProviderResolver(providerResolver(app, cfg))
		go worker.Run(workerCtx)
	}
	if opts.WithScheduler {
		maintenance, err := scheduler.New(store)
		if err != nil {
			return err
		}
		maintenance.Start()
		defer maintenance.Shutdown()
	}
	httpServer := newHTTPServer(cfg, server.Engine)
	errors := make(chan error, 1)
	go func() {
		fmt.Printf("FastTask listening on %s\n", cfg.Address())
		errors <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cancelWorker()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errors:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Migrate applies pending database migrations and closes the store.
func Migrate(ctx context.Context, cfg config.Config) error {
	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.Migrate(ctx)
}

// Backup creates a consistent SQLite backup and returns its destination path.
// An empty output defaults to a timestamped file under backups/.
func Backup(ctx context.Context, cfg config.Config, output string) (string, error) {
	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	if output == "" {
		output = filepath.Join("backups", "fasttask-"+time.Now().Format("20060102-150405")+".db")
	}
	if err := store.Backup(ctx, output); err != nil {
		return "", err
	}
	return output, nil
}

// Doctor verifies runtime prerequisites and returns a one-line summary.
func Doctor(ctx context.Context, cfg config.Config) (string, error) {
	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return "", err
	}
	if err := store.Ready(ctx); err != nil {
		return "", err
	}
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		return "", err
	}
	return fmt.Sprintf("ok database=%s listen=%s web=%s", cfg.DatabasePath, cfg.Address(), cfg.WebDist), nil
}

// newHTTPServer builds the HTTP server with the production timeout budget.
// WriteTimeout is changed to 0 in wiring step 2 to support SSE streams; see
// doc/agent-impl.md §9.2.
func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// providerResolver returns the Worker's per-job model provider resolver. It
// prefers the admin-configured default provider and falls back to environment
// configuration, matching the previous inline behaviour.
func providerResolver(app *application.App, cfg config.Config) func(context.Context) (agent.Provider, agent.Transcriber, error) {
	return func(ctx context.Context) (agent.Provider, agent.Transcriber, error) {
		runtime, err := app.ActiveProviderRuntime(ctx)
		if err != nil || runtime != nil {
			if runtime != nil {
				return runtime.Provider, runtime.Transcriber, nil
			}
			return nil, nil, err
		}
		if cfg.HasLLM() {
			provider := agent.NewOpenAI(cfg)
			var transcriber agent.Transcriber
			if cfg.TranscriptionModel != "" {
				transcriber = provider
			}
			return provider, transcriber, nil
		}
		return nil, nil, nil
	}
}
