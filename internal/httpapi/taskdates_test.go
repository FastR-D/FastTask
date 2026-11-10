package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestTaskPlannedDatesHTTPCreatePatchClearAndPreconditions(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "dates-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", DatabasePath: "unused", JWTSecret: strings.Repeat("s", 40), AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "date-admin", AdminPassword: "test-only-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err := auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	user, access, _, err := auth.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	app := application.New(store)
	goal := persistence.Goal{Title: "Date contract", SuccessCriteria: "Preserve planning"}
	if err := app.CreateGoal(ctx, user.ID, &goal); err != nil {
		t.Fatal(err)
	}
	server := New(app, auth, cfg, nil)
	sequence := 0
	request := func(method, path, body, etag string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		sequence++
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("dates-%d", sequence))
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+access)
		}
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		res := httptest.NewRecorder()
		server.Engine.ServeHTTP(res, req)
		return res
	}
	decode := func(res *httptest.ResponseRecorder) persistence.Task {
		t.Helper()
		if res.Code < 200 || res.Code >= 300 {
			t.Fatalf("unexpected status: %d %s", res.Code, res.Body.String())
		}
		var task persistence.Task
		if err := json.Unmarshal(res.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	body := fmt.Sprintf(`{"goal_id":%q,"type":"milestone","title":"Research phase","success_criteria":"A result","minimum_action":"Begin","estimate_minutes":40,"priority":70,"position":0,"planned_start":"2026-11-01","planned_end":"2026-11-30"}`, goal.ID)
	created := request("POST", "/tasks", body, "", true)
	task := decode(created)
	if task.PlannedStart == nil || *task.PlannedStart != "2026-11-01" || task.PlannedEnd == nil || *task.PlannedEnd != "2026-11-30" {
		t.Fatalf("create lost dates: %+v", task)
	}
	path := "/tasks/" + task.ID
	etag := created.Header().Get("ETag")
	if res := request("PATCH", path, `{"planned_end":"2026-12-01"}`, etag, false); res.Code != 401 {
		t.Fatalf("anonymous date edit: %d", res.Code)
	}
	if res := request("PATCH", path, `{"planned_end":"2026-12-01"}`, "", true); res.Code != 428 {
		t.Fatalf("missing precondition: %d %s", res.Code, res.Body.String())
	}
	updated := request("PATCH", path, `{"planned_end":"2026-12-01"}`, etag, true)
	task = decode(updated)
	if *task.PlannedStart != "2026-11-01" || *task.PlannedEnd != "2026-12-01" {
		t.Fatal("omitting a date did not preserve it")
	}
	if res := request("PATCH", path, `{"planned_end":"2026-12-02"}`, etag, true); res.Code != 412 {
		t.Fatalf("stale precondition: %d", res.Code)
	}
	etag = updated.Header().Get("ETag")
	for _, invalid := range []string{`{"planned_start":null}`, `{"planned_end":"2026-10-01"}`, `{"planned_start":"2026-02-30"}`, `{"planned_start":12}`} {
		if res := request("PATCH", path, invalid, etag, true); res.Code != 422 {
			t.Fatalf("bad dates accepted: %s %d %s", invalid, res.Code, res.Body.String())
		}
	}
	unrelated := request("PATCH", path, `{"title":"Updated research phase"}`, etag, true)
	task = decode(unrelated)
	if task.PlannedStart == nil || task.PlannedEnd == nil || *task.PlannedEnd != "2026-12-01" {
		t.Fatal("unrelated patch cleared omitted dates")
	}
	tree := request("GET", "/goals/"+goal.ID+"/task-tree", "", "", true)
	if tree.Code != 200 || !strings.Contains(tree.Body.String(), `"planned_start":"2026-11-01"`) {
		t.Fatalf("tree omitted dates: %d %s", tree.Code, tree.Body.String())
	}
	cleared := request("PATCH", path, `{"planned_start":null,"planned_end":null}`, unrelated.Header().Get("ETag"), true)
	task = decode(cleared)
	if task.PlannedStart != nil || task.PlannedEnd != nil {
		t.Fatal("explicit null did not clear dates")
	}
	decode(request("PATCH", path, `{"planned_start":"2028-02-29","planned_end":"2028-02-29"}`, cleared.Header().Get("ETag"), true))
	invalidCreate := strings.Replace(body, `"planned_start":"2026-11-01"`, `"planned_start":"2026-02-30"`, 1)
	if res := request("POST", "/tasks", invalidCreate, "", true); res.Code != 422 {
		t.Fatalf("create accepted an impossible date: %d %s", res.Code, res.Body.String())
	}
}
