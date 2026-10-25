package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/panelweather"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

type panelFailTransport struct{}

func (panelFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("offline test")
}

func TestPanelHTTPReadOnlyCapabilityAndCredentialReceipts(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "http-panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", DatabasePath: "unused", JWTSecret: strings.Repeat("j", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "test-admin", AdminPassword: "test-only-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err := auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	user, access, _, err := auth.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	previous := panelweather.Default
	panelweather.Default = panelweather.NewClient(&http.Client{Transport: panelFailTransport{}}, "http://weather.invalid", time.Now)
	t.Cleanup(func() { panelweather.Default = previous })
	app := application.New(store)
	goal := persistence.Goal{Title: "Panel integration test", SuccessCriteria: "Verified screen task state"}
	if err := app.CreateGoal(ctx, user.ID, &goal); err != nil {
		t.Fatal(err)
	}
	task := persistence.Task{GoalID: goal.ID, Title: "Real FastTask item", SuccessCriteria: "Task result", MinimumAction: "Read one page", EstimateMinutes: 25}
	if err := app.CreateTask(ctx, user.ID, &task); err != nil {
		t.Fatal(err)
	}
	server := New(app, auth, cfg, nil)
	request := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		server.Engine.ServeHTTP(response, req)
		return response
	}
	member := map[string]string{"Authorization": "Bearer " + access}
	for _, path := range []string{"/panel/profile", "/panel/snapshot", "/panel/pairings"} {
		if res := request("GET", path, "", nil); res.Code != 401 {
			t.Fatalf("%s exposed without authentication: %d %s", path, res.Code, res.Body.String())
		}
	}
	profile := request("GET", "/panel/profile", "", member)
	if profile.Code != 200 || profile.Header().Get("ETag") == "" {
		t.Fatalf("profile GET: %d %s", profile.Code, profile.Body.String())
	}
	profileBody := `{"display_name":"new","seat":"A01","title":"Today","avatar_data_url":"","weather_override":"","task_overrides":{"logical-1":{"selected":false}}}`
	if res := request("PUT", "/panel/profile", profileBody, member); res.Code != 428 {
		t.Fatalf("profile overwrite missing precondition: %d %s", res.Code, res.Body.String())
	}
	putHeaders := map[string]string{"Authorization": "Bearer " + access, "If-Match": profile.Header().Get("ETag")}
	if res := request("PUT", "/panel/profile", profileBody, putHeaders); res.Code != 200 {
		t.Fatalf("profile PUT: %d %s", res.Code, res.Body.String())
	}
	if res := request("PUT", "/panel/profile", profileBody, putHeaders); res.Code != 412 {
		t.Fatalf("stale profile accepted: %d %s", res.Code, res.Body.String())
	}
	createHeaders := map[string]string{"Authorization": "Bearer " + access, "Idempotency-Key": "new-panel-once"}
	created := request("POST", "/panel/pairings", `{"device_name":"WL14B00B1F"}`, createHeaders)
	if created.Code != 200 {
		t.Fatalf("pairing create: %d %s", created.Code, created.Body.String())
	}
	var pairing struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &pairing); err != nil || pairing.Token == "" {
		t.Fatalf("pairing response %s %v", created.Body.String(), err)
	}
	if !strings.Contains(created.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("capability may be cached")
	}
	var receipt persistence.IdempotencyRecord
	if err := store.DB.Where("idem_key = ?", "new-panel-once").First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt.ResponseJSON, pairing.Token) || !strings.Contains(receipt.ResponseJSON, "withheld") {
		t.Fatalf("credential saved to idempotency receipt: %s", receipt.ResponseJSON)
	}
	if res := request("POST", "/panel/pairings", `{"device_name":"WL14B00B1F"}`, createHeaders); res.Code != 409 || bytes.Contains(res.Body.Bytes(), []byte(pairing.Token)) {
		t.Fatalf("capability replayed: %d %s", res.Code, res.Body.String())
	}
	panelHeaders := map[string]string{"X-Panel-Token": pairing.Token}
	snapshot := request("GET", "/panel/snapshot", "", panelHeaders)
	if snapshot.Code != 200 || !strings.Contains(snapshot.Body.String(), `"display_name":"new"`) || !strings.Contains(snapshot.Body.String(), `"mode":"on_change"`) || strings.Contains(snapshot.Body.String(), `"schedule"`) || !strings.Contains(snapshot.Body.String(), `"task_overrides":{"logical-1":{"selected":false}}`) {
		t.Fatalf("snapshot: %d %s", snapshot.Code, snapshot.Body.String())
	}
	if strings.Contains(snapshot.Body.String(), user.Identifier) || strings.Contains(snapshot.Body.String(), user.PasswordHash) {
		t.Fatal("snapshot leaks full user authentication record")
	}
	var actual panelSnapshotBody
	if err := json.Unmarshal(snapshot.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual.Tasks.TodayItems) != 1 || actual.Tasks.TodayItems[0].Title != task.Title || actual.Tasks.TodayItems[0].TaskStatus != "ready" {
		t.Fatalf("snapshot did not use real FastTask tasks: %+v", actual.Tasks.TodayItems)
	}
	item := actual.Tasks.TodayItems[0]
	itemPath := "/daily-plans/" + actual.Tasks.DailyPlanID + "/items/" + item.ID
	mutationHeaders := map[string]string{"Authorization": "Bearer " + access, "If-Match": item.ItemETag}
	completed := request("POST", itemPath+"/completions", `{"type":"minimum_action","summary":"Read a page"}`, mutationHeaders)
	if completed.Code != 200 {
		t.Fatalf("task completion: %d %s", completed.Code, completed.Body.String())
	}
	afterComplete := request("GET", "/panel/snapshot", "", panelHeaders)
	if afterComplete.Code != 200 || json.Unmarshal(afterComplete.Body.Bytes(), &actual) != nil || len(actual.Tasks.TodayItems) != 1 || actual.Tasks.TodayItems[0].Status != "satisfied" || actual.Tasks.TodayItems[0].TaskStatus != "ready" {
		t.Fatalf("snapshot conflates daily completion with source task: %d %s", afterComplete.Code, afterComplete.Body.String())
	}
	mutationHeaders["If-Match"] = actual.Tasks.TodayItems[0].ItemETag
	reopened := request("POST", itemPath+"/reopenings", `{"summary":"Correct accidental completion"}`, mutationHeaders)
	if reopened.Code != 200 {
		t.Fatalf("task reopening: %d %s", reopened.Code, reopened.Body.String())
	}
	afterReopen := request("GET", "/panel/snapshot", "", panelHeaders)
	if afterReopen.Code != 200 || json.Unmarshal(afterReopen.Body.Bytes(), &actual) != nil || len(actual.Tasks.TodayItems) != 1 || actual.Tasks.TodayItems[0].Status != "planned" {
		t.Fatalf("snapshot reopening stale: %d %s", afterReopen.Code, afterReopen.Body.String())
	}
	for _, path := range []string{"/panel/profile", "/panel/pairings", "/panel/tasks", "/me"} {
		if res := request("GET", path, "", panelHeaders); res.Code != 401 {
			t.Fatalf("panel scope escaped to %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	if res := request("PUT", "/panel/profile", `{"display_name":"malicious"}`, panelHeaders); res.Code != 401 {
		t.Fatalf("panel capability writes profile: %d", res.Code)
	}
	if res := request("POST", "/panel/pairings", `{"device_name":"bad"}`, panelHeaders); res.Code != 401 {
		t.Fatalf("panel capability creates pairing: %d", res.Code)
	}
	if res := request("DELETE", "/panel/pairings/"+pairing.ID, "", member); res.Code != 200 {
		t.Fatalf("pairing revoke: %d %s", res.Code, res.Body.String())
	}
	if res := request("GET", "/panel/snapshot", "", panelHeaders); res.Code != 401 {
		t.Fatalf("revoked capability accepted: %d", res.Code)
	}
}
