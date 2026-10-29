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

func TestHarnessHTTPAuthenticationAndRunScope(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PublicURL: "http://localhost", JWTSecret: strings.Repeat("j", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "test-admin", AdminPassword: "test-only-password", AdminName: "Admin", WebDist: t.TempDir()}
	auth := platformauth.New(store, cfg)
	if err = auth.EnsureAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	user, jwt, _, err := auth.Login(ctx, cfg.AdminIdentifier, cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	app := application.New(store)
	svc := application.NewAgentService(app, application.WithCredentialsResolver(func(context.Context) (*application.ModelCredentials, error) {
		return &application.ModelCredentials{Model: "fixture", BaseURL: "http://127.0.0.1:1", APIKey: "private-fixture"}, nil
	}))
	submitted, err := svc.SubmitCommands(ctx, user.ID, application.CommandsRequest{HarnessMode: application.HarnessModeWASM, Commands: []application.Command{{Type: "add-message", Message: &application.CommandMessage{Role: "user", Parts: []application.CommandPart{{Type: "text", Text: "hello"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := svc.IssueBrowserRunGrant(ctx, user.ID, submitted.RunID, "")
	if err != nil {
		t.Fatal(err)
	}
	server := New(app, auth, cfg, svc)
	request := func(method, path, body, access string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if access != "" {
			req.Header.Set("Authorization", "Bearer "+access)
		}
		res := httptest.NewRecorder()
		server.Engine.ServeHTTP(res, req)
		return res
	}
	for _, tc := range []struct{ method, path, body, access string }{
		{"POST", "/agent/runs/" + submitted.RunID + "/heartbeat", "{}", jwt},
		{"POST", "/agent/runs/" + submitted.RunID + "/cancellation", "{}", grant.HarnessToken},
		{"POST", "/agent/runs/" + submitted.RunID + "/heartbeat", "{}", ""},
		{"POST", "/agent/runs/some-other-run/heartbeat", "{}", grant.HarnessToken},
		{"GET", "/agent/threads/some-other-thread/checkpoint", "", grant.HarnessToken},
	} {
		res := request(tc.method, tc.path, tc.body, tc.access)
		if res.Code < 400 || res.Code >= 500 {
			t.Fatalf("identity boundary: %s %d %s", tc.path, res.Code, res.Body.String())
		}
	}
	res := request("POST", "/agent/runs/"+submitted.RunID+"/heartbeat", "{}", grant.HarnessToken)
	if res.Code != 200 {
		t.Fatalf("heartbeat %d %s", res.Code, res.Body.String())
	}
	res = request("GET", "/agent/tools", "", grant.HarnessToken)
	if res.Code != 200 || strings.Contains(res.Body.String(), "private-fixture") {
		t.Fatalf("manifest %d %s", res.Code, res.Body.String())
	}
	var manifest map[string]any
	if err = json.Unmarshal(res.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	res = request("POST", "/agent/runs/"+submitted.RunID+"/cancellation", "{}", jwt)
	if res.Code != 200 {
		t.Fatalf("cancel %d %s", res.Code, res.Body.String())
	}
	res = request("POST", "/agent/runs/"+submitted.RunID+"/tools/list_goals", `{"tool_call_id":"forged","input":{}}`, grant.HarnessToken)
	var outcome application.HarnessToolOutcome
	if err = json.Unmarshal(res.Body.Bytes(), &outcome); err != nil || !outcome.IsError {
		t.Fatalf("cancelled tool accepted %d %s", res.Code, res.Body.String())
	}
}
