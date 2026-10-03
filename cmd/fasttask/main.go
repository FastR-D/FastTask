package main

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/FastR-D/FastTask/internal/bootstrap"
	"github.com/FastR-D/FastTask/internal/buildinfo"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/spf13/cobra"
)

func main() {
	setupLogging()
	// A failed migration or a refused configuration is a runtime error, not a usage error: print it
	// once, without the flag listing burying it.
	root := &cobra.Command{Use: "fasttask", Short: "Long-term goal and daily focus system", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(serveCommand(), workerCommand(), schedulerCommand(), migrateCommand(), backupCommand(), doctorCommand(), hardenCommand(), versionCommand())
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
		return bootstrap.Serve(cmd.Context(), cfg, bootstrap.ServeOptions{WithWorker: withWorker, WithScheduler: withScheduler})
	}}
	cmd.Flags().BoolVar(&withWorker, "with-worker", true, "run persistent agent worker")
	cmd.Flags().BoolVar(&withScheduler, "with-scheduler", true, "run maintenance scheduler")
	return cmd
}

// workerCommand runs the persistent agent worker on its own, with no HTTP
// listener, so job execution can be scaled independently of the API (wiring.md
// §3, §8, §9: fasttask worker runs independently).
func workerCommand() *cobra.Command {
	return &cobra.Command{Use: "worker", Short: "Run the persistent agent worker", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return bootstrap.RunWorker(cmd.Context(), cfg)
	}}
}

// schedulerCommand runs the maintenance scheduler on its own (wiring.md §3, §8,
// §9: fasttask scheduler runs independently).
func schedulerCommand() *cobra.Command {
	return &cobra.Command{Use: "scheduler", Short: "Run the maintenance scheduler", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return bootstrap.RunScheduler(cmd.Context(), cfg)
	}}
}

func migrateCommand() *cobra.Command {
	return &cobra.Command{Use: "migrate", Short: "Apply database migrations", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return bootstrap.Migrate(cmd.Context(), cfg)
	}}
}

func backupCommand() *cobra.Command {
	var output, verify string
	cmd := &cobra.Command{Use: "backup", Short: "Create a consistent SQLite backup", RunE: func(cmd *cobra.Command, args []string) error {
		if verify != "" {
			// Checking a backup before restoring it needs no server configuration and must not touch the
			// live database.
			if err := persistence.VerifyBackup(cmd.Context(), verify); err != nil {
				return err
			}
			fmt.Printf("ok %s\n", verify)
			return nil
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		destination, err := bootstrap.Backup(cmd.Context(), cfg, output)
		if err != nil {
			return err
		}
		fmt.Println(destination)
		return nil
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "", "backup destination")
	cmd.Flags().StringVar(&verify, "verify", "", "verify an existing backup file (integrity and schema version) instead of creating one")
	return cmd
}

func doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check runtime prerequisites", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		summary, err := bootstrap.Doctor(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		fmt.Println(summary)
		return nil
	}}
}

func versionCommand() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print build information", Run: func(cmd *cobra.Command, args []string) {
		info := buildinfo.Get()
		fmt.Printf("fasttask %s commit=%s built=%s go=%s %s/%s\n", info.Version, info.Commit, info.BuildTime, info.Go, runtime.GOOS, runtime.GOARCH)
	}}
}

// setupLogging configures the process-wide structured logger. Text by default, because the usual reader
// is `docker compose logs`; FASTTASK_LOG_FORMAT=json for a log shipper. FASTTASK_LOG_LEVEL=debug|info|
// warn|error. Logs go to stderr so stdout stays the command's own output (backup paths, doctor summary).
func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FASTTASK_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, options)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FASTTASK_LOG_FORMAT")), "json") {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}
	slog.SetDefault(slog.New(handler))
}
