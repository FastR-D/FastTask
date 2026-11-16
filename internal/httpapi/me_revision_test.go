package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

func TestMeConcurrentIfMatch(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "profile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", JWTSecret: strings.Repeat("j", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "admin", AdminPassword: "test-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err = auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	_, access, _, err := auth.Login(ctx, "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	server := New(application.New(store), auth, cfg, nil)
	request := func(method, body, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/me", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+access)
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		res := httptest.NewRecorder()
		server.Engine.ServeHTTP(res, req)
		return res
	}
	decode := func(res *httptest.ResponseRecorder) persistence.User {
		t.Helper()
		if res.Code != http.StatusOK {
			t.Fatalf("profile status %d: %s", res.Code, res.Body.String())
		}
		var user persistence.User
		if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
			t.Fatal(err)
		}
		if res.Header().Get("ETag") != application.StrongETag("user", user.ID, user.Revision) {
			t.Fatal("response ETag does not identify its user revision")
		}
		return user
	}
	beforeResponse := request("GET", "", "")
	before := decode(beforeResponse)
	const count = 16
	responses := make([]*httptest.ResponseRecorder, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range responses {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			responses[i] = request("PATCH", fmt.Sprintf(`{"display_name":"Winner %d"}`, i), beforeResponse.Header().Get("ETag"))
		}(i)
	}
	close(start)
	workers.Wait()
	winners := 0
	var winner persistence.User
	for _, response := range responses {
		switch response.Code {
		case http.StatusOK:
			winners++
			winner = decode(response)
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("unexpected concurrent PATCH status %d: %s", response.Code, response.Body.String())
		}
	}
	if winners != 1 {
		t.Fatalf("same If-Match produced %d successful writes, want one", winners)
	}
	afterResponse := request("GET", "", "")
	after := decode(afterResponse)
	if after.Revision != before.Revision+1 || after.DisplayName != winner.DisplayName {
		t.Fatal("stored profile does not match the single winning update")
	}
	if res := request("PATCH", `{"locale":"en-US"}`, beforeResponse.Header().Get("ETag")); res.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale version accepted: %d", res.Code)
	}
	if res := request("PATCH", `{"timezone":"Not/AZone"}`, afterResponse.Header().Get("ETag")); res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid timezone status: %d", res.Code)
	}
	unchanged := decode(request("GET", "", ""))
	if unchanged.Revision != after.Revision || unchanged.Timezone != after.Timezone {
		t.Fatal("invalid patch changed the profile")
	}
	next := decode(request("PATCH", `{"locale":"en-US"}`, afterResponse.Header().Get("ETag")))
	if next.Locale != "en-US" || next.DisplayName != after.DisplayName || next.Revision != after.Revision+1 {
		t.Fatal("fresh partial patch lost unrelated fields or revision")
	}
	if err := store.DB.Exec(`CREATE TRIGGER refuse_profile_update BEFORE UPDATE ON users BEGIN SELECT RAISE(ABORT, 'test storage failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if res := request("PATCH", `{"locale":"zh-CN"}`, application.StrongETag("user", next.ID, next.Revision)); res.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure status: %d", res.Code)
	}
	if err := store.DB.Exec(`DROP TRIGGER refuse_profile_update`).Error; err != nil {
		t.Fatal(err)
	}
	failed := decode(request("GET", "", ""))
	if failed.Revision != next.Revision || failed.Locale != next.Locale {
		t.Fatal("failed update changed the stored profile")
	}
}
