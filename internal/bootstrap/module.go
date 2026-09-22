// Package bootstrap assembles the FastTask runtime from its building blocks.
//
// This package is the composition root described by doc/wiring.md. It is the
// only place, together with cmd/, allowed to import go.uber.org/fx (wiring.md
// §2). Every constructor is a plain Go function that can be called without fx;
// fx only decides who calls them and in what order.
package bootstrap

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"go.uber.org/fx"
)

// Core holds the modules every role depends on: the lifecycle observer,
// persistence, auth and the application aggregate. It carries no long-running
// lifecycle of its own beyond the store and admin bootstrap hooks.
var Core = fx.Options(LifecycleModule, PersistenceModule, AuthModule, ApplicationModule)

// Role compositions (wiring.md §3). AgentRuntimeModule joins ServeRole and
// WorkerRole once the agent runtime lands (doc/agent-impl.md); until then the
// roles are HTTP/worker/scheduler over the shared Core.
var (
	ServeRole     = fx.Options(Core, HTTPModule)
	WorkerRole    = fx.Options(Core, WorkerModule)
	SchedulerRole = fx.Options(Core, SchedulerModule)
)

// ServeOptions selects which long-running roles run alongside the HTTP server.
// Both default to true to preserve the historical `serve` behaviour.
type ServeOptions struct {
	WithWorker    bool
	WithScheduler bool
}

// serveTimeout is the graceful start/stop budget. It preserves the 15 second
// shutdown window the pre-fx serve command used (wiring.md §6).
const serveTimeout = 15 * time.Second

// serveOptions assembles the fx option list for the `serve` command. Module
// order matters: lifecycle OnStart hooks run in the order their modules are
// declared, and OnStop runs in reverse. HTTPModule is appended last so it starts
// last and therefore stops first, ahead of the worker (wiring.md §6: "停机时
// HTTP 必须先于 Worker 停止").
//
// It is split out from Serve so tests can validate the dependency graph with
// fx.ValidateApp without binding a port (wiring.md §2, §8).
func serveOptions(cfg config.Config, opts ServeOptions) []fx.Option {
	options := []fx.Option{fx.Supply(cfg), Core}
	if opts.WithWorker {
		options = append(options, WorkerModule)
	}
	if opts.WithScheduler {
		options = append(options, SchedulerModule)
	}
	options = append(options, HTTPModule)
	return options
}

// Serve assembles the full runtime and blocks until the context is cancelled or
// a SIGINT/SIGTERM is received, then shuts down gracefully within serveTimeout.
func Serve(ctx context.Context, cfg config.Config, opts ServeOptions) error {
	return runRole(ctx, serveOptions(cfg, opts))
}

// RunWorker runs the worker role on its own — Core + WorkerModule, no HTTP
// listener and no scheduler — so a deployment can scale job execution separately
// from the API (wiring.md §3, §8: "fasttask worker ... 可独立运行"). It blocks
// until the context is cancelled or a signal arrives, then drains gracefully.
func RunWorker(ctx context.Context, cfg config.Config) error {
	return runRole(ctx, []fx.Option{fx.Supply(cfg), WorkerRole})
}

// RunScheduler runs the maintenance scheduler role on its own — Core +
// SchedulerModule (wiring.md §3, §8: "fasttask scheduler ... 可独立运行").
func RunScheduler(ctx context.Context, cfg config.Config) error {
	return runRole(ctx, []fx.Option{fx.Supply(cfg), SchedulerRole})
}

// runRole is the shared body of the serve/worker/scheduler commands: build the fx
// app from the role's options, start it within serveTimeout, block until the
// caller's context is done, a signal arrives, or a component requests shutdown,
// then stop gracefully (wiring.md §6: preserve the 15 second shutdown budget).
func runRole(ctx context.Context, options []fx.Option) error {
	app := fx.New(append(options,
		fx.StartTimeout(serveTimeout),
		fx.StopTimeout(serveTimeout),
		fx.NopLogger,
	)...)
	if err := app.Err(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, serveTimeout)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		// Startup was interrupted (e.g. SIGTERM during boot) or a hook failed.
		// Always Stop so hooks that did start release their goroutines, then treat
		// a caller cancellation as a clean abort rather than an error.
		stopCtx, cancelStop := context.WithTimeout(context.Background(), serveTimeout)
		defer cancelStop()
		_ = app.Stop(stopCtx)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	// Block until either the caller's context is done, a signal arrives, or a
	// component requests shutdown (e.g. the HTTP listener failed).
	select {
	case <-ctx.Done():
	case <-app.Done():
	case <-app.Wait():
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), serveTimeout)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
