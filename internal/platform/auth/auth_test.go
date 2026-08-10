package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password was not hashed")
	}
	if !VerifyPassword("correct horse battery staple", hash) {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword("wrong password", hash) {
		t.Fatal("invalid password accepted")
	}
}

func TestPasswordHashRejectsShortPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestRefreshRotationLogoutAndPanelClaims(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: "auth-test-secret-with-enough-characters", PanelJWTSecret: "panel-test-secret-with-enough-characters", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin"}
	service := New(store, cfg)
	if err := service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	user, access, refresh, err := service.Login(context.Background(), "admin", "password-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(access); err != nil {
		t.Fatal(err)
	}
	_, newAccess, newRefresh, err := service.Refresh(context.Background(), refresh)
	if err != nil {
		t.Fatal(err)
	}
	if newRefresh == refresh {
		t.Fatal("refresh token was not rotated")
	}
	if _, err := service.Authenticate(newAccess); err != nil {
		t.Fatal(err)
	}
	var session persistence.Session
	if err := store.DB.Where("refresh_hash = ?", persistence.Hash(newRefresh)).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(newAccess); err == nil {
		t.Fatal("logged out access token still valid")
	}
	panelToken, err := service.IssuePanelToken(user.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.AuthenticatePanel(panelToken)
	if err != nil {
		t.Fatal(err)
	}
	if principal.UserID != user.ID {
		t.Fatalf("panel user=%s", principal.UserID)
	}
	wrongScope, err := service.IssueServiceToken(user.ID, []string{"agent-jobs:write"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticatePanel(wrongScope); err == nil {
		t.Fatal("wrong service scope accepted")
	}
	importToken, err := service.IssueServiceToken(user.ID, []string{"imports:write", "imports:read"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	importPrincipal, err := service.AuthenticateServicePrincipal(importToken)
	if err != nil {
		t.Fatal(err)
	}
	if !HasScopes(importPrincipal, "imports:write", "imports:read") || HasScopes(importPrincipal, "agent-jobs:write") {
		t.Fatalf("unexpected import scopes: %#v", importPrincipal.Scopes)
	}
	clientToken, err := service.IssueServiceTokenForClient(user.ID, "fastread", []string{"imports:write"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clientPrincipal, err := service.AuthenticateServicePrincipal(clientToken)
	if err != nil {
		t.Fatal(err)
	}
	if clientPrincipal.ClientID != "fastread" {
		t.Fatalf("client id=%q", clientPrincipal.ClientID)
	}
}

func TestLoginBackoffAfterRepeatedFailures(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Config{JWTSecret: "rate-test-secret-with-enough-characters", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin"})
	if err := service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _, _, _ = service.Login(context.Background(), "admin", "wrong-password")
	}
	if _, _, _, err := service.Login(context.Background(), "admin", "password-for-tests"); err == nil {
		t.Fatal("login backoff was not enforced")
	}
}
