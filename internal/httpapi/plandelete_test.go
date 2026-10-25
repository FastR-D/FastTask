package httpapi

import (
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

func TestPlanItemDeletionHTTPAuthenticationConcurrencyAndReplay(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "delete-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", DatabasePath: "unused", JWTSecret: strings.Repeat("s", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "deletion-admin", AdminPassword: "test-only-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err := auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	user, access, _, err := auth.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	app := application.New(store)
	goal := persistence.Goal{Title: "Batch deletion", SuccessCriteria: "Keep original research tasks"}
	if err := app.CreateGoal(ctx, user.ID, &goal); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		task := persistence.Task{GoalID: goal.ID, Title: fmt.Sprintf("Daily task %d", index), SuccessCriteria: "Result", MinimumAction: "First action", EstimateMinutes: 25, Position: index}
		if err := app.CreateTask(ctx, user.ID, &task); err != nil {
			t.Fatal(err)
		}
	}
	previousWeather := panelweather.Default
	panelweather.Default = panelweather.NewClient(&http.Client{Transport: panelFailTransport{}}, "http://weather.invalid", time.Now)
	t.Cleanup(func() { panelweather.Default = previousWeather })
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
		res := httptest.NewRecorder()
		server.Engine.ServeHTTP(res, req)
		return res
	}
	member := map[string]string{"Authorization": "Bearer " + access}
	current := request("GET", "/daily-plans/current", "", member)
	var view struct {
		Plan  persistence.DailyPlan      `json:"plan"`
		Items []application.PlanItemView `json:"today_items"`
	}
	if err := json.Unmarshal(current.Body.Bytes(), &view); current.Code != 200 || err != nil || len(view.Items) != 3 {
		t.Fatalf("current plan: %d %s %v", current.Code, current.Body.String(), err)
	}
	path := "/daily-plans/" + view.Plan.ID + "/item-deletions"
	selected := []map[string]string{{"id": view.Items[0].ID, "if_match": view.Items[0].ItemETag}, {"id": view.Items[1].ID, "if_match": view.Items[1].ItemETag}}
	body, _ := json.Marshal(map[string]any{"items": selected})
	headers := func(key string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + access, "If-Match": current.Header().Get("ETag"), "Idempotency-Key": key}
	}
	if res := request("POST", path, string(body), map[string]string{"If-Match": current.Header().Get("ETag"), "Idempotency-Key": "unauthenticated"}); res.Code != 401 {
		t.Fatalf("anonymous delete: %d %s", res.Code, res.Body.String())
	}
	_, panelToken, err := application.NewPanelService(store).CreatePairing(ctx, user.ID, "WL00000000")
	if err != nil {
		t.Fatal(err)
	}
	if res := request("POST", path, string(body), map[string]string{"X-Panel-Token": panelToken, "If-Match": current.Header().Get("ETag"), "Idempotency-Key": "panel-cannot-delete"}); res.Code != 401 {
		t.Fatalf("read-only capability deleted tasks: %d %s", res.Code, res.Body.String())
	}
	missingPlan := headers("missing-plan-precondition")
	delete(missingPlan, "If-Match")
	if res := request("POST", path, string(body), missingPlan); res.Code != 428 {
		t.Fatalf("missing plan If-Match: %d %s", res.Code, res.Body.String())
	}
	missingKey := headers("unused")
	delete(missingKey, "Idempotency-Key")
	if res := request("POST", path, string(body), missingKey); res.Code != 422 {
		t.Fatalf("missing idempotency key: %d %s", res.Code, res.Body.String())
	}
	wrongPlan := headers("wrong-plan-precondition")
	wrongPlan["If-Match"] = application.StrongETag("plan", "another-plan", view.Plan.Revision)
	if res := request("POST", path, string(body), wrongPlan); res.Code != 412 {
		t.Fatalf("wrong plan precondition: %d %s", res.Code, res.Body.String())
	}
	stale := []map[string]string{{"id": view.Items[0].ID, "if_match": view.Items[0].ItemETag}, {"id": view.Items[1].ID, "if_match": application.StrongETag("dpi", view.Items[1].ID, view.Items[1].Revision+1)}}
	staleBody, _ := json.Marshal(map[string]any{"items": stale})
	if res := request("POST", path, string(staleBody), headers("stale-one-item")); res.Code != 412 {
		t.Fatalf("stale item accepted: %d %s", res.Code, res.Body.String())
	}
	var unchanged persistence.DailyPlan
	if err := store.DB.First(&unchanged, "id = ?", view.Plan.ID).Error; err != nil || unchanged.Revision != view.Plan.Revision || unchanged.ManualItemsRemoved {
		t.Fatalf("failed batch changed plan: %+v %v", unchanged, err)
	}
	foreignID := persistence.NewID("plan")
	now := persistence.Now()
	foreignUser := persistence.User{ID: "foreign-delete-user", Identifier: "foreign-delete-user", PasswordHash: "not-used", DisplayName: "Other", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&foreignUser).Error; err != nil {
		t.Fatal(err)
	}
	foreignPlan := persistence.DailyPlan{ID: foreignID, UserID: foreignUser.ID, LocalDate: view.Plan.LocalDate, Timezone: view.Plan.Timezone, Status: "active", AlgorithmVersion: "fixture", CurrentRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&foreignPlan).Error; err != nil {
		t.Fatal(err)
	}
	foreignHeaders := headers("other-owner-plan")
	foreignHeaders["If-Match"] = application.StrongETag("plan", foreignID, 1)
	if res := request("POST", "/daily-plans/"+foreignID+"/item-deletions", string(body), foreignHeaders); res.Code != 404 {
		t.Fatalf("another user's plan accessible: %d %s", res.Code, res.Body.String())
	}
	deleteHeaders := headers("delete-two-items")
	deleted := request("POST", path, string(body), deleteHeaders)
	var result application.PlanItemDeletionResult
	if err := json.Unmarshal(deleted.Body.Bytes(), &result); deleted.Code != 200 || err != nil || len(result.DeletedIDs) != 2 || len(result.Items) != 1 || result.Items[0].ID != view.Items[2].ID || !result.Plan.ManualItemsRemoved {
		t.Fatalf("delete response: %d %s %v", deleted.Code, deleted.Body.String(), err)
	}
	replayed := request("POST", path, string(body), deleteHeaders)
	if replayed.Code != 200 || replayed.Header().Get("Idempotency-Replayed") != "true" || replayed.Header().Get("ETag") != deleted.Header().Get("ETag") || replayed.Body.String() != deleted.Body.String() {
		t.Fatalf("lost-response retry not replayed: %d %s", replayed.Code, replayed.Body.String())
	}
	changedBody, _ := json.Marshal(map[string]any{"items": selected[:1]})
	if res := request("POST", path, string(changedBody), deleteHeaders); res.Code != 409 {
		t.Fatalf("changed-body replay accepted: %d %s", res.Code, res.Body.String())
	}
	for _, route := range []string{"/daily-plans/current", "/daily-plans/" + view.Plan.ID, "/schedule", "/panel/tasks", "/panel/snapshot", "/panel/summary"} {
		res := request("GET", route, "", member)
		if res.Code != 200 {
			t.Fatalf("read after delete %s: %d %s", route, res.Code, res.Body.String())
		}
		for _, id := range result.DeletedIDs {
			if strings.Contains(res.Body.String(), id) {
				t.Fatalf("%s exposes deleted current item %s: %s", route, id, res.Body.String())
			}
		}
	}
	device := persistence.Device{Name: "Read-only fixture", Timezone: "Asia/Shanghai"}
	deviceToken, err := app.RegisterDevice(ctx, user.ID, &device)
	if err != nil {
		t.Fatal(err)
	}
	deviceResponse := request("GET", "/devices/self/poll", "", map[string]string{"X-Device-Token": deviceToken})
	if deviceResponse.Code != 200 {
		t.Fatalf("device read after delete: %d %s", deviceResponse.Code, deviceResponse.Body.String())
	}
	for _, id := range result.DeletedIDs {
		if strings.Contains(deviceResponse.Body.String(), id) {
			t.Fatalf("device poll exposes deleted current item %s", id)
		}
	}
	mapping := persistence.FastResearchAccount{AccountID: "delete-fixture-account", UserID: user.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&mapping).Error; err != nil {
		t.Fatal(err)
	}
	fastResearch := (RouteDeps{app: app, auth: auth, cfg: &cfg}).fastResearchTodayFor(ctx, mapping.AccountID)
	if !fastResearch.Found || len(fastResearch.Items) != 1 || fastResearch.Items[0].ID != view.Items[2].ID {
		t.Fatalf("FastResearch current-day projection differs after deletion: %+v", fastResearch)
	}
}
