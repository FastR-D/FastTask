package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/httpapi"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/FastR-D/FastTask/internal/scheduler"
	"github.com/spf13/cobra"
)

var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	root := &cobra.Command{Use: "fasttask", Short: "Long-term goal and daily focus system"}
	root.AddCommand(serveCommand(), migrateCommand(), backupCommand(), doctorCommand(), hardenCommand(), versionCommand())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serveCommand() *cobra.Command {
	var withWorker, withScheduler bool
	cmd := &cobra.Command{Use: "serve", Short: "Run the HTTP service", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store, err := persistence.Open(cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := store.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
		authService := platformauth.New(store, cfg)
		if err := authService.EnsureAdmin(ctx); err != nil {
			return fmt.Errorf("ensure admin: %w", err)
		}
		app := application.New(store)
		server := httpapi.New(app, authService, cfg)
		workerCtx, cancelWorker := context.WithCancel(ctx)
		defer cancelWorker()
		if withWorker || withScheduler {
			var providers []agent.Provider
			if cfg.HasLLM() {
				providers = append(providers, agent.NewOpenAI(cfg))
			}
			worker := application.NewWorker(app, cfg.WorkerInterval, providers...)
			if cfg.HasLLM() && cfg.TranscriptionModel != "" {
				worker.WithTranscriber(agent.NewOpenAI(cfg))
			}
			go worker.Run(workerCtx)
		}
		var maintenance *scheduler.Scheduler
		if withScheduler {
			maintenance, err = scheduler.New(store)
			if err != nil {
				return err
			}
			maintenance.Start()
			defer maintenance.Shutdown()
		}
		httpServer := &http.Server{Addr: cfg.Address(), Handler: server.Engine, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
		errors := make(chan error, 1)
		go func() { fmt.Printf("FastTask listening on %s\n", cfg.Address()); errors <- httpServer.ListenAndServe() }()
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
	}}
	cmd.Flags().BoolVar(&withWorker, "with-worker", true, "run persistent agent worker")
	cmd.Flags().BoolVar(&withScheduler, "with-scheduler", true, "run maintenance scheduler")
	return cmd
}

func migrateCommand() *cobra.Command {
	return &cobra.Command{Use: "migrate", Short: "Apply database migrations", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store, err := persistence.Open(cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		return store.Migrate(cmd.Context())
	}}
}

func backupCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{Use: "backup", Short: "Create a consistent SQLite backup", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store, err := persistence.Open(cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		if output == "" {
			output = filepath.Join("backups", "fasttask-"+time.Now().Format("20060102-150405")+".db")
		}
		if err := store.Backup(cmd.Context(), output); err != nil {
			return err
		}
		fmt.Println(output)
		return nil
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "", "backup destination")
	return cmd
}

func doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check runtime prerequisites", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store, err := persistence.Open(cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.Migrate(cmd.Context()); err != nil {
			return err
		}
		if err := store.Ready(cmd.Context()); err != nil {
			return err
		}
		if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
			return err
		}
		fmt.Printf("ok database=%s listen=%s web=%s\n", cfg.DatabasePath, cfg.Address(), cfg.WebDist)
		return nil
	}}
}

func versionCommand() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print build information", Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("fasttask %s commit=%s built=%s go=%s %s/%s\n", version, commit, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}}
}
