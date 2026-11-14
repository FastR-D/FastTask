package auth_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/FastR-D/FastTask/internal/platform/auth"
)

func TestLogoutRotationFamily(t *testing.T) {
	for _, order := range []string{"refresh-first", "logout-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "auth.db")
			store, err := persistence.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			otherStore, err := persistence.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer otherStore.Close()
			cfg := config.Config{JWTSecret: strings.Repeat("j", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "admin", AdminPassword: "test-password", AdminName: "Admin"}
			service, other := auth.New(store, cfg), auth.New(otherStore, cfg)
			if err := service.EnsureAdmin(ctx); err != nil {
				t.Fatal(err)
			}
			_, access, refresh, err := service.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
			if err != nil {
				t.Fatal(err)
			}
			principal, err := service.Authenticate(access)
			if err != nil {
				t.Fatal(err)
			}
			_, independentAccess, independentRefresh, err := other.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
			if err != nil {
				t.Fatal(err)
			}
			accesses, refreshes := []string{access}, []string{refresh}
			rotate := func() {
				_, a, r, err := other.Refresh(ctx, refresh)
				if err != nil {
					if order == "refresh-first" {
						t.Error(err)
					}
					return
				}
				accesses, refreshes = append(accesses, a), append(refreshes, r)
				refresh = r
			}
			logout := func() {
				if err := service.Logout(ctx, principal.SessionID); err != nil {
					t.Error(err)
				}
			}
			switch order {
			case "refresh-first":
				rotate()
				rotate()
				rotate()
				// A real SQLite failure must roll back and propagate, leaving the
				// family usable until a later successful logout.
				if err := store.DB.Exec(`CREATE TRIGGER reject_logout BEFORE UPDATE ON sessions
					WHEN NEW.status = 'revoked' BEGIN SELECT RAISE(ABORT, 'storage failure'); END`).Error; err != nil {
					t.Fatal(err)
				}
				if err := service.Logout(ctx, principal.SessionID); err == nil {
					t.Fatal("failed database write acknowledged")
				}
				if _, err := other.Authenticate(accesses[len(accesses)-1]); err != nil {
					t.Fatalf("failed logout changed family: %v", err)
				}
				if err := store.DB.Exec("DROP TRIGGER reject_logout").Error; err != nil {
					t.Fatal(err)
				}
				logout()
			case "logout-first":
				logout()
				rotate()
			case "concurrent":
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); rotate() }()
				go func() { defer wg.Done(); logout() }()
				wg.Wait()
			}
			// Check access before refresh: reuse detection must not hide a failed logout.
			for i, token := range accesses {
				if _, err := other.Authenticate(token); err == nil {
					t.Errorf("generation %d access accepted after logout", i)
				}
			}
			for i := len(refreshes) - 1; i >= 0; i-- {
				if _, _, _, err := other.Refresh(ctx, refreshes[i]); err == nil {
					t.Errorf("generation %d refresh/grace accepted after logout", i)
				}
			}
			if err := other.Logout(ctx, principal.SessionID); err != nil {
				t.Errorf("repeat logout: %v", err)
			}
			if _, err := service.Authenticate(independentAccess); err != nil {
				t.Errorf("other login revoked: %v", err)
			}
			if _, _, _, err := service.Refresh(ctx, independentRefresh); err != nil {
				t.Errorf("other login refresh revoked: %v", err)
			}
			if err := service.Logout(ctx, "missing"); err == nil {
				t.Error("unknown SID must return an error")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := service.Logout(cancelled, principal.SessionID); err == nil {
				t.Error("database cancellation swallowed")
			}
		})
	}
}
