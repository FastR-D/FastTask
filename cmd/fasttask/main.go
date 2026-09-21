package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/FastR-D/FastTask/internal/bootstrap"
	"github.com/FastR-D/FastTask/internal/config"
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
		return bootstrap.Serve(cmd.Context(), cfg, bootstrap.ServeOptions{WithWorker: withWorker, WithScheduler: withScheduler})
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
		return bootstrap.Migrate(cmd.Context(), cfg)
	}}
}

func backupCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{Use: "backup", Short: "Create a consistent SQLite backup", RunE: func(cmd *cobra.Command, args []string) error {
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
		fmt.Printf("fasttask %s commit=%s built=%s go=%s %s/%s\n", version, commit, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}}
}
