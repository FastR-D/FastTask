package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

func TestLogoutHTTPRevokesOnlyCurrentLogin(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "logout.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", JWTSecret: strings.Repeat("j", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "admin", AdminPassword: "test-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err := auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	server := New(application.New(store), auth, cfg, nil)
	request := func(method, path, body, access string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", persistence.NewID("request"))
		if access != "" {
			req.Header.Set("Authorization", "Bearer "+access)
		}
		res := httptest.NewRecorder()
		server.Engine.ServeHTTP(res, req)
		return res
	}
	type tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	login := func() tokens {
		res := request("POST", "/auth/login", `{"identifier":"admin","password":"test-password"}`, "")
		if res.Code != 200 {
			t.Fatalf("login status: %d", res.Code)
		}
		var token tokens
		if err := json.Unmarshal(res.Body.Bytes(), &token); err != nil {
			t.Fatal(err)
		}
		if token.Access == "" || token.Refresh == "" {
			t.Fatal("login missing credentials")
		}
		return token
	}
	current, independent := login(), login()
	res := request("POST", "/auth/refresh", `{"refresh_token":"`+current.Refresh+`"}`, "")
	if res.Code != 200 {
		t.Fatalf("refresh status: %d", res.Code)
	}
	var successor tokens
	if err := json.Unmarshal(res.Body.Bytes(), &successor); err != nil {
		t.Fatal(err)
	}
	if res := request("POST", "/auth/logout", "", successor.Access); res.Code < 200 || res.Code >= 300 {
		t.Fatalf("logout status: %d", res.Code)
	}
	for _, token := range []string{current.Access, successor.Access} {
		if res := request("GET", "/auth/session", "", token); res.Code != 401 {
			t.Errorf("session status after logout: %d", res.Code)
		}
	}
	for _, token := range []string{successor.Refresh, current.Refresh} {
		if res := request("POST", "/auth/refresh", `{"refresh_token":"`+token+`"}`, ""); res.Code != 401 {
			t.Errorf("refresh status after logout: %d", res.Code)
		}
	}
	if res := request("GET", "/auth/session", "", independent.Access); res.Code != 200 {
		t.Errorf("independent login status: %d", res.Code)
	}
}
