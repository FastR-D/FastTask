package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

func TestFastResearchTodayWireContract(t *testing.T) {
	var fixture struct {
		Request   json.RawMessage `json:"request"`
		Response  map[string]any  `json:"response"`
		AuthCases []struct {
			Name   string `json:"name"`
			Status int    `json:"status"`
		} `json:"auth_cases"`
	}
	raw, err := os.ReadFile("../../contracts/fastresearch-today/v1/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(strings.ReplaceAll(string(raw), "$today", persistence.Now().UTC().Format("2006-01-02"))), &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer *httptest.Server
	var grants atomic.Int32
	token := func(name string) string {
		claims := jwt.MapClaims{"iss": issuer.URL, "sub": "research-service", "aud": []string{"fasttask-api"}, "client_id": "research-service", "scope": "integrations:fastresearch", "service": true, "token_use": "access", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()}
		signingKey := key
		switch name {
		case "wrong-scope":
			claims["scope"] = "openid"
		case "wrong-audience":
			claims["aud"] = []string{"research-api"}
		case "wrong-issuer":
			claims["iss"] = "https://other.invalid"
		case "wrong-client":
			claims["sub"] = "other"
			claims["client_id"] = "other"
		case "non-service":
			claims["service"] = false
		case "wrong-subject":
			claims["sub"] = "user-subject"
		case "expired":
			claims["exp"] = time.Now().Add(-time.Minute).Unix()
		case "wrong-signature":
			signingKey = otherKey
		case "wrong-token-use":
			claims["token_use"] = "id"
		}
		value := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		value.Header["kid"] = "fixture"
		signed, signErr := value.SignedString(signingKey)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return signed
	}
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/jwks", "userinfo_endpoint": issuer.URL + "/userinfo", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}, "token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			grants.Add(1)
			id, secret, ok := r.BasicAuth()
			id, _ = url.QueryUnescape(id)
			secret, _ = url.QueryUnescape(secret)
			if err := r.ParseForm(); err != nil || !ok || id != "research-service" || secret != "disposable-secret" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "integrations:fastresearch" {
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": token("service"), "token_type": "Bearer", "expires_in": 60})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "today.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	seedFastResearchTodayContract(t, store)
	cfg := config.Config{PublicURL: "http://localhost", JWTSecret: strings.Repeat("s", 40), WebDist: t.TempDir(), FastCASServiceEnabled: true, FastCASServiceIssuer: issuer.URL, FastCASServiceClientID: "task", FastCASServiceAudience: "fasttask-api", FastCASServiceLoopbackHTTP: true, FastCASServiceAllowedClientIDs: []string{"research-service"}, FastCASServiceAllowedReceiverRefs: []string{"account-a", "missing-account"}, FastResearchLegacyIngestKeyEnabled: true, FastResearchIngestKey: "disposable-key"}
	server := New(application.New(store), platformauth.New(store, cfg), cfg, nil)
	for _, tc := range fixture.AuthCases {
		t.Run(tc.Name, func(t *testing.T) {
			body := string(fixture.Request)
			if tc.Name == "denied-receiver" {
				body = `{"receiver_refs":["outside"]}`
			}
			request := httptest.NewRequest("POST", "/api/v1/integrations/fastresearch/today", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if tc.Name != "legacy" && tc.Name != "missing" {
				request.Header.Set("Authorization", "Bearer "+token(tc.Name))
			}
			if tc.Name == "legacy" || tc.Name == "mixed" {
				request.Header.Set("X-API-Key", cfg.FastResearchIngestKey)
			}
			server.cfg.FastCASServiceEnabled = tc.Name != "service-disabled"
			if tc.Name == "service-disabled" {
				server.auth = platformauth.New(store, server.cfg)
			}
			defer func() { server.cfg = cfg; server.auth = platformauth.New(store, cfg) }()
			response := httptest.NewRecorder()
			server.Engine.ServeHTTP(response, request)
			if response.Code != tc.Status {
				t.Fatalf("status=%d want=%d: %s", response.Code, tc.Status, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if tc.Status == 200 {
				var actual map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || !reflect.DeepEqual(actual, fixture.Response) {
					t.Fatalf("response mismatch: %s (%v)", response.Body.String(), err)
				}
			}
		})
	}
	for _, body := range []string{`{"receiver_refs":[]}`, `{"receiver_refs":[" "]}`, `{"receiver_refs":42}`} {
		r := httptest.NewRequest("POST", "/api/v1/integrations/fastresearch/today", strings.NewReader(body))
		r.Header.Set("X-API-Key", cfg.FastResearchIngestKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Engine.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid refs status=%d", w.Code)
		}
	}
	t.Run("legacy-outside-service-allowlist", func(t *testing.T) {
		request := httptest.NewRequest("POST", "/api/v1/integrations/fastresearch/today", strings.NewReader(`{"receiver_refs":["outside"]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-API-Key", cfg.FastResearchIngestKey)
		response := httptest.NewRecorder()
		server.Engine.ServeHTTP(response, request)
		var actual struct {
			Members []fastResearchTodayMember `json:"members"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || response.Code != 200 || len(actual.Members) != 1 || !actual.Members[0].Found || actual.Members[0].UserID != "task-user-outside" {
			t.Fatalf("legacy authorization changed: %d %s (%v)", response.Code, response.Body.String(), err)
		}
	})
	// Optional local topology joins the real Go receiver to the actual Research
	// production client. Product-only CI still runs all wire cases above.
	if probe := os.Getenv("FASTRESEARCH_CONTRACT_PROBE"); probe != "" {
		task := httptest.NewServer(server.Engine)
		defer task.Close()
		command := exec.Command("node", probe)
		command.Env = append(os.Environ(), "FASTTASK_URL="+task.URL, "FASTTASK_SERVICE_ISSUER="+issuer.URL, "FASTTASK_SERVICE_CLIENT_ID=research-service", "FASTTASK_SERVICE_CLIENT_SECRET=disposable-secret", "FASTTASK_SERVICE_ALLOW_LOOPBACK_HTTP=true", "FASTTASK_API_KEY=disposable-key")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Research consumer: %v\n%s", err, output)
		} else {
			t.Log(string(output))
		}
		if grants.Load() != 2 {
			t.Fatalf("want one grant per Research client (probe and production server), got %d", grants.Load())
		}
	}
}

func seedFastResearchTodayContract(t *testing.T, store *persistence.Store) {
	t.Helper()
	// Seed domain rows, never a pre-serialized HTTP response or a mocked plan
	// projection. The real receiver must resolve the mapping and refresh the plan.
	stamp := time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC)
	user := persistence.User{ID: "task-user-a", Identifier: "today-member", PasswordHash: "unused", DisplayName: "Research member", Timezone: "UTC", Locale: "en", Role: "member", Status: "active", Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}
	outside := user
	outside.ID, outside.Identifier, outside.DisplayName = "task-user-outside", "outside-member", "Outside member"
	goal := persistence.Goal{ID: "today-goal-a", UserID: user.ID, Title: "Research evidence", SuccessCriteria: "Review findings", Status: "active", Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}
	plan := persistence.DailyPlan{ID: "today-plan-a", UserID: user.ID, LocalDate: persistence.Now().UTC().Format("2006-01-02"), Timezone: "UTC", Status: "active", AlgorithmVersion: "deterministic-v3", CurrentRevision: 3, Revision: 3, FrozenAt: &stamp, CreatedAt: stamp, UpdatedAt: stamp}
	for _, row := range []any{
		&user, &outside,
		&persistence.FastResearchAccount{AccountID: "account-a", UserID: user.ID, KeyID: "fixture-key", Person: user.DisplayName, CreatedAt: stamp, UpdatedAt: stamp},
		&persistence.FastResearchAccount{AccountID: "outside", UserID: outside.ID, KeyID: "outside-key", Person: outside.DisplayName, CreatedAt: stamp, UpdatedAt: stamp},
		&goal, &plan,
	} {
		if err := store.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for index, row := range []struct {
		id, title, commitment, minimum, status, completionType, completionSummary string
		minutes                                                                   int
	}{
		{"read", "fastread: Read evidence", "Extract two claims", "Read the abstract", "in_progress", "", "", 30},
		{"write", "fastwrite: Draft findings", "Save a findings draft", "Write one paragraph", "satisfied", "result", "Draft saved", 45},
		{"review", "Review evidence", "Check the claim list", "Open the checklist", "planned", "", "", 45},
	} {
		task := persistence.Task{ID: "task-" + row.id, UserID: user.ID, GoalID: goal.ID, Type: "action", Title: row.title, Status: "ready", SuccessCriteria: row.commitment, MinimumAction: row.minimum, EstimateMinutes: row.minutes, Position: index + 1, Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}
		item := persistence.DailyPlanItem{ID: "item-" + row.id, LogicalID: "item-" + row.id, UserID: user.ID, PlanID: plan.ID, TaskID: &task.ID, Kind: "core", Title: row.title, Commitment: row.commitment, MinimumAction: row.minimum, AllowedTypes: "result,step,time,minimum_action", Status: row.status, CompletionType: row.completionType, CompletionSummary: row.completionSummary, PlanRevision: 3, TargetMinutes: row.minutes, Position: index + 1, Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}
		for _, model := range []any{&task, &item} {
			if err := store.DB.Create(model).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
}
