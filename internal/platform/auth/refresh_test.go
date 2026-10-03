package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func newRefreshTestService(t *testing.T) (*Service, *persistence.Store) {
	t.Helper()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Config{JWTSecret: "refresh-test-secret-with-enough-characters", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin"})
	if err := service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	return service, store
}

// A client whose refresh response was lost retries with the token it still holds. That must not be
// treated as theft.
func TestRefreshRetryWithinGraceRotatesTheUnusedSuccessor(t *testing.T) {
	service, _ := newRefreshTestService(t)
	ctx := context.Background()
	_, _, first, err := service.Login(ctx, "admin", "password-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	_, _, lost, err := service.Refresh(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	_, access, retried, err := service.Refresh(ctx, first)
	if err != nil {
		t.Fatalf("retry inside the grace window was refused: %v", err)
	}
	if retried == lost || retried == first {
		t.Fatal("retry did not mint a fresh refresh token")
	}
	if _, err := service.Authenticate(access); err != nil {
		t.Fatalf("access token from the retry is not valid: %v", err)
	}
	// A second retry of the same original token still resolves.
	_, _, again, err := service.Refresh(ctx, first)
	if err != nil {
		t.Fatalf("second retry refused: %v", err)
	}
	if _, _, _, err := service.Refresh(ctx, again); err != nil {
		t.Fatalf("newest token cannot be used: %v", err)
	}
}

// Outside the window, presenting a rotated token is reuse: the whole family is revoked, including the
// legitimate holder's newest token.
func TestRefreshReuseAfterGraceRevokesTheFamily(t *testing.T) {
	service, store := newRefreshTestService(t)
	ctx := context.Background()
	_, _, stolen, err := service.Login(ctx, "admin", "password-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	_, access, current, err := service.Refresh(ctx, stolen)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Model(&persistence.Session{}).Where("refresh_hash = ?", persistence.Hash(stolen)).UpdateColumn("updated_at", persistence.Now().Add(-refreshReuseGrace-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Refresh(ctx, stolen); err == nil {
		t.Fatal("reused refresh token was accepted after the grace window")
	}
	if _, _, _, err := service.Refresh(ctx, current); err == nil {
		t.Fatal("family was not revoked after reuse")
	}
	if _, err := service.Authenticate(access); err == nil {
		t.Fatal("access token of a revoked family still authenticates")
	}
}

// A grace retry must not extend its own window.
func TestRefreshGraceIsMeasuredFromTheFirstRotation(t *testing.T) {
	service, store := newRefreshTestService(t)
	ctx := context.Background()
	_, _, first, err := service.Login(ctx, "admin", "password-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Refresh(ctx, first); err != nil {
		t.Fatal(err)
	}
	old := persistence.Now().Add(-refreshReuseGrace + 2*time.Second)
	if err := store.DB.Model(&persistence.Session{}).Where("refresh_hash = ?", persistence.Hash(first)).UpdateColumn("updated_at", old).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Refresh(ctx, first); err != nil {
		t.Fatal(err)
	}
	var session persistence.Session
	if err := store.DB.Where("refresh_hash = ?", persistence.Hash(first)).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if !session.UpdatedAt.Equal(old) {
		t.Fatalf("grace retry moved the window: updated_at %s, want %s", session.UpdatedAt, old)
	}
}
