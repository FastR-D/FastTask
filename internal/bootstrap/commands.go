package bootstrap

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// Migrate applies pending database migrations and closes the store. It is a
// one-shot command and intentionally does not use fx.
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
	summary := fmt.Sprintf("ok database=%s listen=%s web=%s", cfg.DatabasePath, cfg.Address(), cfg.WebDist)
	// The sidecar is optional; its checks are reported but only a disabled one is silent (§8.4).
	if cfg.SidecarEnabled {
		supervisor, err := NewSidecar(cfg)
		if err != nil {
			return "", err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		lines, err := supervisor.Doctor(checkCtx)
		summary += " " + strings.Join(lines, " ")
		if err != nil {
			return summary, err
		}
	}
	return summary, nil
}
