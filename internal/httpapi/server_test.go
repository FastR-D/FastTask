package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
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

type testAPI struct {
	server *Server
	store  *persistence.Store
	worker *application.Worker
	access string
	user   persistence.User
}

func newTestAPI(t *testing.T) testAPI {
	t.Helper()
	cfg := config.Config{Listen: "127.0.0.1", Port: 10000, PublicURL: "http://127.0.0.1:10000", DatabasePath: filepath.Join(t.TempDir(), "http.db"), JWTSecret: "http-test-secret-with-enough-characters", PanelJWTSecret: "panel-test-secret-with-enough-characters", TrustedProxies: []string{"127.0.0.1"}, AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin", WorkerInterval: time.Millisecond, WebDist: filepath.Join(t.TempDir(), "missing"), Environment: "test"}
	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	authService := platformauth.New(store, cfg)
	if err := authService.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	var user persistence.User
	if err := store.DB.Where("identifier = ?", "admin").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	app := application.New(store)
	server := New(app, authService, cfg)
	api := testAPI{server: server, store: store, worker: application.NewWorker(app, time.Millisecond), user: user}
	login := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": "admin", "password": "password-for-tests"}, nil)
	if login.Code != 200 {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	decode(t, login, &tokens)
	api.access = tokens.AccessToken
	return api
}

func (a testAPI) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if a.access != "" {
		request.Header.Set("Authorization", "Bearer "+a.access)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	a.server.Engine.ServeHTTP(response, request)
	return response
}

func TestOpenAPIAndHealth(t *testing.T) {
	api := newTestAPI(t)
	for _, path := range []string{"/health/live", "/health/ready", "/api/v1/openapi.json", "/api/v1/docs"} {
		response := api.do(t, http.MethodGet, path, nil, nil)
		if response.Code != 200 {
			t.Errorf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	openapi := api.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if !strings.Contains(openapi.Body.String(), "/daily-plans/{plan_id}/items/{item_id}/completions") {
		t.Fatal("OpenAPI missing daily completion endpoint")
	}
	if !strings.Contains(openapi.Body.String(), "userBearer") {
		t.Fatal("OpenAPI missing bearer security scheme")
	}
}

func TestRealUserScenario(t *testing.T) {
	api := newTestAPI(t)
	goalResponse := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文初稿", "description": "形成组内评审版本", "success_criteria": "结构完整并通过一次组内评审"}, map[string]string{"Idempotency-Key": "goal-1"})
	if goalResponse.Code != 201 {
		t.Fatalf("create goal=%d %s", goalResponse.Code, goalResponse.Body.String())
	}
	var goal persistence.Goal
	decode(t, goalResponse, &goal)
	for i := 0; i < 4; i++ {
		response := api.do(t, http.MethodPost, "/api/v1/tasks", map[string]any{"goal_id": goal.ID, "type": "task", "title": fmt.Sprintf("研究任务 %d", i+1), "description": "真实场景任务", "success_criteria": "形成可验证结果", "estimate_minutes": 25, "minimum_action": "写下一条可验证结论", "priority": 90 - i, "position": i}, map[string]string{"Idempotency-Key": fmt.Sprintf("task-%d", i)})
		if response.Code != 201 {
			t.Fatalf("create task %d=%d %s", i, response.Code, response.Body.String())
		}
	}
	date := time.Now().In(mustZone(t, "Asia/Shanghai")).Format("2006-01-02")
	planResponse := api.do(t, http.MethodPost, "/api/v1/daily-plans", map[string]any{"local_date": date, "timezone": "Asia/Shanghai", "available_minutes": 120}, map[string]string{"Idempotency-Key": "plan-1"})
	if planResponse.Code != 201 {
		t.Fatalf("create plan=%d %s", planResponse.Code, planResponse.Body.String())
	}
	var planEnvelope struct {
		Plan  persistence.DailyPlan       `json:"plan"`
		Items []persistence.DailyPlanItem `json:"items"`
	}
	decode(t, planResponse, &planEnvelope)
	if len(planEnvelope.Items) != 3 {
		t.Fatalf("core items=%d", len(planEnvelope.Items))
	}
	item := planEnvelope.Items[0]
	complete := api.do(t, http.MethodPost, fmt.Sprintf("/api/v1/daily-plans/%s/items/%s/completions", planEnvelope.Plan.ID, item.ID), map[string]any{"type": "minimum_action", "summary": "完成一条可验证结论"}, map[string]string{"If-Match": application.StrongETag("dpi", item.ID, item.Revision), "Idempotency-Key": "complete-1"})
	if complete.Code != 200 {
		t.Fatalf("complete item=%d %s", complete.Code, complete.Body.String())
	}
	var underlying persistence.Task
	if err := api.store.DB.First(&underlying, "id = ?", *item.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if underlying.Status == "completed" {
		t.Fatal("daily completion completed task")
	}
	deviceResponse := api.do(t, http.MethodPost, "/api/v1/devices", map[string]any{"name": "办公室墨水屏", "kind": "eink_panel", "timezone": "Asia/Shanghai", "capabilities": map[string]any{"width": 800, "height": 480}}, map[string]string{"Idempotency-Key": "device-1"})
	if deviceResponse.Code != 201 {
		t.Fatalf("device=%d %s", deviceResponse.Code, deviceResponse.Body.String())
	}
	var device struct {
		Device      persistence.Device `json:"device"`
		DeviceToken string             `json:"device_token"`
	}
	decode(t, deviceResponse, &device)
	poll := api.do(t, http.MethodGet, "/api/v1/devices/self/poll", nil, map[string]string{"X-Device-Token": device.DeviceToken})
	if poll.Code != 200 {
		t.Fatalf("poll=%d %s", poll.Code, poll.Body.String())
	}
	etag := poll.Header().Get("ETag")
	if etag == "" {
		t.Fatal("poll did not return ETag")
	}
	poll304 := api.do(t, http.MethodGet, "/api/v1/devices/self/poll", nil, map[string]string{"X-Device-Token": device.DeviceToken, "If-None-Match": etag})
	if poll304.Code != 304 {
		t.Fatalf("conditional poll=%d body=%s", poll304.Code, poll304.Body.String())
	}
	panel := api.do(t, http.MethodGet, "/api/v1/panel/summary", nil, nil)
	if panel.Code != 200 {
		t.Fatalf("panel summary=%d %s", panel.Code, panel.Body.String())
	}
}

func TestAgentConversationVoiceAndOwnership(t *testing.T) {
	api := newTestAPI(t)
	goalResponse := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "Agent goal", "success_criteria": "proposal exists"}, map[string]string{"Idempotency-Key": "goal-agent"})
	var goal persistence.Goal
	decode(t, goalResponse, &goal)
	jobResponse := api.do(t, http.MethodPost, fmt.Sprintf("/api/v1/goals/%s/task-tree/generation-jobs", goal.ID), map[string]any{"instruction": "split"}, map[string]string{"Idempotency-Key": "job-agent"})
	if jobResponse.Code != 202 {
		t.Fatalf("job=%d %s", jobResponse.Code, jobResponse.Body.String())
	}
	var job persistence.AgentJob
	decode(t, jobResponse, &job)
	if err := api.worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	getJob := api.do(t, http.MethodGet, "/api/v1/agent-jobs/"+job.ID, nil, nil)
	decode(t, getJob, &job)
	if job.Status != "succeeded" {
		t.Fatalf("job status=%s", job.Status)
	}
	var proposalCount int64
	api.store.DB.Model(&persistence.Proposal{}).Where("job_id = ?", job.ID).Count(&proposalCount)
	if proposalCount != 1 {
		t.Fatalf("proposal count=%d", proposalCount)
	}
	convResponse := api.do(t, http.MethodPost, "/api/v1/conversations", map[string]any{"title": "研究推进"}, map[string]string{"Idempotency-Key": "conv"})
	var conv persistence.Conversation
	decode(t, convResponse, &conv)
	messageResponse := api.do(t, http.MethodPost, fmt.Sprintf("/api/v1/conversations/%s/messages", conv.ID), map[string]any{"content": "任务太大，请给最小行动"}, map[string]string{"Idempotency-Key": "msg"})
	if messageResponse.Code != 202 {
		t.Fatalf("message=%d %s", messageResponse.Code, messageResponse.Body.String())
	}
	if err := api.worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages := api.do(t, http.MethodGet, fmt.Sprintf("/api/v1/conversations/%s/messages", conv.ID), nil, nil)
	var list struct {
		Items []persistence.ConversationMessage `json:"items"`
	}
	decode(t, messages, &list)
	if len(list.Items) != 2 {
		t.Fatalf("message count=%d", len(list.Items))
	}
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	part, err := writer.CreateFormFile("audio", "test.webm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("fake audio bytes"))
	writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/voice-transcription-jobs", &multipartBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+api.access)
	request.Header.Set("Idempotency-Key", "voice")
	response := httptest.NewRecorder()
	api.server.Engine.ServeHTTP(response, request)
	if response.Code != 202 {
		t.Fatalf("voice=%d %s", response.Code, response.Body.String())
	}
	var voiceJob persistence.AgentJob
	decode(t, response, &voiceJob)
	if err := api.worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.store.DB.First(&voiceJob, "id = ?", voiceJob.ID)
	if !strings.Contains(voiceJob.OutputJSON, "transcript") {
		t.Fatalf("voice output=%s", voiceJob.OutputJSON)
	}
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "other", PasswordHash: "hash", DisplayName: "Other", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := api.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	private := persistence.Goal{ID: persistence.NewID("goal"), UserID: other.ID, Title: "Private", SuccessCriteria: "hidden", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := api.store.DB.Create(&private).Error; err != nil {
		t.Fatal(err)
	}
	hidden := api.do(t, http.MethodGet, "/api/v1/goals/"+private.ID, nil, nil)
	if hidden.Code != 404 {
		t.Fatalf("cross-user access=%d", hidden.Code)
	}
}

func TestRevisionAndSecondActiveSessionAreRejected(t *testing.T) {
	api := newTestAPI(t)
	goalResponse := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "Revision", "success_criteria": "checked"}, map[string]string{"Idempotency-Key": "goal-r"})
	var goal persistence.Goal
	decode(t, goalResponse, &goal)
	missing := api.do(t, http.MethodPatch, "/api/v1/goals/"+goal.ID, map[string]any{"title": "Changed"}, nil)
	if missing.Code != 422 && missing.Code != 428 {
		t.Fatalf("missing If-Match=%d %s", missing.Code, missing.Body.String())
	}
	stale := api.do(t, http.MethodPatch, "/api/v1/goals/"+goal.ID, map[string]any{"title": "Changed"}, map[string]string{"If-Match": application.StrongETag("goal", goal.ID, 999)})
	if stale.Code != 412 {
		t.Fatalf("stale If-Match=%d %s", stale.Code, stale.Body.String())
	}
	taskResponse := api.do(t, http.MethodPost, "/api/v1/tasks", map[string]any{"goal_id": goal.ID, "type": "task", "title": "Timer", "success_criteria": "timer done", "estimate_minutes": 25, "minimum_action": "start timer", "priority": 50, "position": 0}, map[string]string{"Idempotency-Key": "timer-task"})
	var task persistence.Task
	decode(t, taskResponse, &task)
	first := api.do(t, http.MethodPost, "/api/v1/work-sessions", map[string]any{"task_id": task.ID, "session_type": "pomodoro", "target_minutes": 25}, map[string]string{"Idempotency-Key": "timer-1"})
	if first.Code != 201 {
		t.Fatalf("first session=%d", first.Code)
	}
	second := api.do(t, http.MethodPost, "/api/v1/work-sessions", map[string]any{"task_id": task.ID, "session_type": "pomodoro", "target_minutes": 25}, map[string]string{"Idempotency-Key": "timer-2"})
	if second.Code != 409 {
		t.Fatalf("second session=%d %s", second.Code, second.Body.String())
	}
}

func TestIdempotencyReplayAndReuseConflict(t *testing.T) {
	api := newTestAPI(t)
	headers := map[string]string{"Idempotency-Key": "same-goal-request"}
	body := map[string]any{"title": "Idempotent goal", "description": "same request", "success_criteria": "created once"}
	first := api.do(t, http.MethodPost, "/api/v1/goals", body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first request=%d %s", first.Code, first.Body.String())
	}
	second := api.do(t, http.MethodPost, "/api/v1/goals", body, headers)
	if second.Code != first.Code || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d replay=%q", second.Code, second.Header().Get("Idempotency-Replayed"))
	}
	if second.Body.String() != first.Body.String() {
		t.Fatal("idempotent replay returned a different body")
	}
	var count int64
	api.store.DB.Model(&persistence.Goal{}).Where("user_id = ? AND title = ?", api.user.ID, "Idempotent goal").Count(&count)
	if count != 1 {
		t.Fatalf("created %d goals, want one", count)
	}
	conflict := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "Different", "description": "changed", "success_criteria": "conflict"}, headers)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("reuse conflict=%d %s", conflict.Code, conflict.Body.String())
	}
}

func TestPlanItemPatchCannotForgeCompletionAndPanelServiceJWTWorks(t *testing.T) {
	api := newTestAPI(t)
	goalResponse := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "Guarded", "description": "guard", "success_criteria": "done"}, map[string]string{"Idempotency-Key": "guard-goal"})
	var goal persistence.Goal
	decode(t, goalResponse, &goal)
	taskResponse := api.do(t, http.MethodPost, "/api/v1/tasks", map[string]any{"goal_id": goal.ID, "type": "task", "title": "Guard task", "description": "", "success_criteria": "done", "estimate_minutes": 25, "minimum_action": "start", "priority": 80, "position": 0}, map[string]string{"Idempotency-Key": "guard-task"})
	var task persistence.Task
	decode(t, taskResponse, &task)
	planResponse := api.do(t, http.MethodPost, "/api/v1/daily-plans", map[string]any{"local_date": "2026-08-20", "timezone": "Asia/Shanghai", "task_ids": []string{task.ID}, "available_minutes": 60}, map[string]string{"Idempotency-Key": "guard-plan"})
	var envelope struct {
		Plan  persistence.DailyPlan       `json:"plan"`
		Items []persistence.DailyPlanItem `json:"items"`
	}
	decode(t, planResponse, &envelope)
	item := envelope.Items[0]
	forged := api.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/daily-plans/%s/items/%s", envelope.Plan.ID, item.ID), map[string]any{"status": "satisfied"}, map[string]string{"If-Match": application.StrongETag("dpi", item.ID, item.Revision)})
	if forged.Code != 422 {
		t.Fatalf("forged completion=%d %s", forged.Code, forged.Body.String())
	}
	panelToken, err := api.server.auth.IssuePanelToken(api.user.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	panel := api.do(t, http.MethodGet, "/api/v1/panel/summary", nil, map[string]string{"Authorization": "Bearer " + panelToken})
	if panel.Code != 200 {
		t.Fatalf("panel service jwt=%d %s", panel.Code, panel.Body.String())
	}
}

func TestExternalWorkerCallbackRejectsStaleLease(t *testing.T) {
	api := newTestAPI(t)
	job, err := application.New(api.store).CreateJob(context.Background(), api.user.ID, "external_test", "test", "subject", 1, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runToken := "callback-secret"
	now := persistence.Now()
	if err := api.store.DB.Model(job).Updates(map[string]any{"status": "running", "attempt_count": 1, "lease_version": 2, "run_token_hash": persistence.Hash(runToken), "locked_until": now.Add(time.Minute), "revision": 2}).Error; err != nil {
		t.Fatal(err)
	}
	serviceToken, err := api.server.auth.IssueServiceToken(api.user.ID, []string{"agent-jobs:write"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale := api.do(t, http.MethodPost, "/api/v1/agent-jobs/"+job.ID+"/callbacks", map[string]any{"attempt_no": 1, "lease_version": 1, "run_token": runToken, "status": "succeeded", "result": map[string]any{"ok": true}}, map[string]string{"Authorization": "Bearer " + serviceToken, "Idempotency-Key": "callback-stale"})
	if stale.Code != 409 {
		t.Fatalf("stale callback=%d %s", stale.Code, stale.Body.String())
	}
	success := api.do(t, http.MethodPost, "/api/v1/agent-jobs/"+job.ID+"/callbacks", map[string]any{"attempt_no": 1, "lease_version": 2, "run_token": runToken, "status": "succeeded", "result": map[string]any{"ok": true}}, map[string]string{"Authorization": "Bearer " + serviceToken, "Idempotency-Key": "callback-good"})
	if success.Code != 200 {
		t.Fatalf("callback=%d %s", success.Code, success.Body.String())
	}
}

func TestExternalTaskTreeCallbackMaterializesProposal(t *testing.T) {
	api := newTestAPI(t)
	goalResponse := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "External proposal", "description": "callback", "success_criteria": "proposal"}, map[string]string{"Idempotency-Key": "external-goal"})
	var goal persistence.Goal
	decode(t, goalResponse, &goal)
	job, err := application.New(api.store).CreateJob(context.Background(), api.user.ID, "task_tree_generation", "goal", goal.ID, 0, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	runToken := "external-run"
	now := persistence.Now()
	if err := api.store.DB.Model(job).Updates(map[string]any{"status": "running", "attempt_count": 1, "lease_version": 1, "run_token_hash": persistence.Hash(runToken), "locked_until": now.Add(time.Minute), "revision": 2}).Error; err != nil {
		t.Fatal(err)
	}
	serviceToken, err := api.server.auth.IssueServiceToken(api.user.ID, []string{"agent-jobs:write"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proposal := []map[string]any{{"op": "create", "type": "task", "title": "External task", "success_criteria": "done", "minimum_action": "start", "priority": 80, "estimate_minutes": 25}}
	response := api.do(t, http.MethodPost, "/api/v1/agent-jobs/"+job.ID+"/callbacks", map[string]any{"attempt_no": 1, "lease_version": 1, "run_token": runToken, "status": "succeeded", "result": map[string]any{"proposal": proposal}}, map[string]string{"Authorization": "Bearer " + serviceToken, "Idempotency-Key": "external-proposal"})
	if response.Code != 200 {
		t.Fatalf("callback=%d %s", response.Code, response.Body.String())
	}
	var count int64
	api.store.DB.Model(&persistence.Proposal{}).Where("job_id = ?", job.ID).Count(&count)
	if count != 1 {
		t.Fatalf("proposal count=%d", count)
	}
}

func decode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %d %q: %v", response.Code, response.Body.String(), err)
	}
}
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}
