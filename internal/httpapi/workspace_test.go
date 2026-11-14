package httpapi

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

func TestGenericWorkspaceTasksUseVerifiedCASAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Bool
	active.Store(true)
	var linkState atomic.Value
	linkState.Store("active")
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/jwks", "userinfo_endpoint": issuer.URL + "/userinfo", "introspection_endpoint": issuer.URL + "/introspect", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "workspace", Use: "sig", Algorithm: "RS256"}}})
		case "/introspect":
			if id, secret, ok := r.BasicAuth(); !ok || id != "task" || secret != "task-client-secret" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"active": active.Load(), "sub": "fastresearch", "client_id": "fastresearch", "scope": "workspace:read", "aud": []string{"task-api", "task"}})
		case "/api/v1/account-links/resolve":
			if r.URL.Query().Get("subject") != "central-subject" {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "task-link", "client_id": "task", "local_account_ref": "task-user-a", "subject": "central-subject", "state": linkState.Load(), "version": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	cfg := defaultTestConfig(t)
	cfg.FastCASIssuer = issuer.URL
	cfg.FastCASClientID = "task"
	cfg.FastCASClientSecret = "task-client-secret"
	cfg.FastCASRedirectURI = cfg.PublicURL + "/api/v1/auth/fastcas/callback"
	cfg.FastCASLoopbackHTTP = true
	cfg.FastCASServiceEnabled = true
	cfg.FastCASServiceIssuer = issuer.URL
	cfg.FastCASServiceClientID = "task"
	cfg.FastCASServiceAudience = "task-api"
	cfg.FastCASServiceLoopbackHTTP = true
	cfg.FastCASServiceAllowedClientIDs = []string{"fastresearch"}
	api := newTestApiWithConfig(t, cfg)
	seedFastResearchTodayContract(t, api.store)
	// Prove this does not depend on the retired Research-account mapping.
	if err := api.store.DB.Where("account_id = ?", "account-a").Delete(&persistence.FastResearchAccount{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := api.store.DB.Create(&persistence.FastCASLink{ID: "task-link", Issuer: issuer.URL, ClientID: "task", UserID: "task-user-a", Subject: "central-subject", State: "active", Version: 1, VerifiedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	token := func(scope string, service bool) string {
		value := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer.URL, "sub": "fastresearch", "client_id": "fastresearch", "aud": []string{"task-api", "task"}, "scope": scope, "service": service, "token_use": "access", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
		value.Header["kid"] = "workspace"
		raw, err := value.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	path := "/api/integrations/fastresearch/workspace"
	headers := map[string]string{"Authorization": "Bearer " + token("workspace:read", true)}
	result := api.do(t, "POST", path, map[string]string{"subject": "central-subject"}, headers)
	if result.Code != 200 {
		t.Fatalf("workspace: %d %s", result.Code, result.Body.String())
	}
	var response struct {
		Schema string `json:"schemaVersion"`
		State  string `json:"accountState"`
		Tasks  struct {
			Items    []any                          `json:"items"`
			Progress struct{ Total, Completed int } `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Schema != "fastresearch.workspace.v1" || response.State != "active" || len(response.Tasks.Items) != 3 || response.Tasks.Progress.Total != 3 || response.Tasks.Progress.Completed != 1 {
		t.Fatalf("task projection: %s", result.Body.String())
	}
	if strings.Contains(result.Body.String(), "task-user-a") || strings.Contains(result.Body.String(), "central-subject") {
		t.Fatal("identity identifiers leaked")
	}
	for _, auth := range []map[string]string{{}, {"X-API-Key": "legacy"}, {"Authorization": "Bearer " + token("integrations:fastresearch", true)}, {"Authorization": "Bearer " + token("workspace:read", false)}} {
		if r := api.do(t, "POST", path, map[string]string{"subject": "central-subject"}, auth); r.Code != 401 {
			t.Fatalf("invalid credential accepted: %d", r.Code)
		}
	}
	if r := api.do(t, "POST", path, map[string]string{"subject": "unknown-subject"}, headers); r.Code != 200 || !strings.Contains(r.Body.String(), `"accountState":"missing"`) {
		t.Fatalf("missing identity: %d %s", r.Code, r.Body.String())
	}
	linkState.Store("revoked")
	if r := api.do(t, "POST", path, map[string]string{"subject": "central-subject"}, headers); r.Code != 200 || !strings.Contains(r.Body.String(), `"accountState":"inactive"`) || strings.Contains(r.Body.String(), `"tasks"`) {
		t.Fatalf("revoked binding returned data: %s", r.Body.String())
	}
	active.Store(false)
	if r := api.do(t, "POST", path, map[string]string{"subject": "central-subject"}, headers); r.Code != 401 {
		t.Fatalf("revoked service accepted: %d", r.Code)
	}
}
