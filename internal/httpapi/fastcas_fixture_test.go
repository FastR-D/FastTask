package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type testAPI struct {
	server *Server
	store  *persistence.Store
	worker *application.Worker
	agent  *application.AgentService
	access string
	user   persistence.User
}

func newTestAPI(t *testing.T) testAPI {
	t.Helper()
	return newTestAPIWithAgent(t)
}

// defaultTestConfig returns the standard httpapi test config. WebDist points at
// a non-existent dir so static() registers nothing; tests that exercise static
// file serving override it via newTestApiWithConfig.
func defaultTestConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{Listen: "127.0.0.1", Port: 10000, PublicURL: "http://127.0.0.1:10000", DatabasePath: filepath.Join(t.TempDir(), "http.db"), JWTSecret: "http-test-secret-with-enough-characters", PanelJWTSecret: "panel-test-secret-with-enough-characters", ProviderEncryptionKey: "provider-test-secret-with-enough-characters", TrustedProxies: []string{"127.0.0.1"}, AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin", WorkerInterval: time.Millisecond, WebDist: filepath.Join(t.TempDir(), "missing"), Environment: "test"}
}

// newTestAPIWithAgent builds the test server with the given AgentService options,
// so tests can inject a scripted ChatProvider (phase C/D loop coverage).
func newTestAPIWithAgent(t *testing.T, agentOpts ...application.AgentOption) testAPI {
	t.Helper()
	return newTestApiWithConfig(t, defaultTestConfig(t), agentOpts...)
}

// newTestApiWithConfig builds the test server from an explicit config, so tests
// can point WebDist at a real dist tree (static file serving) or vary other cfg.
func newTestApiWithConfig(t *testing.T, cfg config.Config, agentOpts ...application.AgentOption) testAPI {
	t.Helper()
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
	app := application.NewWithSecret(store, cfg.ProviderEncryptionKey)
	agentService := application.NewAgentService(app, agentOpts...)
	server := New(app, authService, cfg, agentService)
	api := testAPI{server: server, store: store, agent: agentService, worker: application.NewWorker(app, time.Millisecond).WithAgentRunner(agentService), user: user}
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

func decode(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatal(err)
	}
}
