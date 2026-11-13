package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestExternalOutcomesUseOwnConnectionAndUserWeek(t *testing.T) {
	store, owner, other := panelTestStore(t)
	key := []byte(strings.Repeat("k", 32))
	service := NewRemoteToolService(store, key)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "fastread_session=owners-private-session" {
			t.Error("wrong source principal")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api/papers":
			body = map[string]any{"items": []any{map[string]any{"id": "p", "title": "P"}}, "total": 1, "limit": 5, "offset": 0}
		case "/api/papers/p":
			body = map[string]any{"id": "p", "title": "P"}
		case "/api/workspace-notes":
			body = map[string]any{"items": []any{map[string]any{"id": "current", "scope_id": "p", "content": "Monday Shanghai", "updated": time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC).Unix()}, map[string]any{"id": "previous", "scope_id": "p", "content": "Sunday Shanghai", "updated": time.Date(2026, 11, 8, 15, 59, 0, 0, time.UTC).Unix()}}}
		case "/api/papers/p/reports":
			body = map[string]any{"items": []any{}}
		default:
			t.Errorf("unexpected source read: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	cipher, err := encryptWithKey(key, "owners-private-session")
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	if err := store.DB.Create(&persistence.IntegrationConnection{ID: "connection", UserID: owner, System: "fastread", APIURL: server.URL, Profile: "auto", AuthKind: "cookie", CredentialCiphertext: cipher, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.ExternalOutcomes(context.Background(), owner, "fastread", "2026-W46", 5, 0)
	if err != nil || page.Status != "ready" || len(page.Items) != 1 || page.Items[0].Summary != "Monday Shanghai" {
		t.Fatalf("wrong local week %#v, %v", page, err)
	}
	before := calls.Load()
	page, err = service.ExternalOutcomes(context.Background(), other, "fastread", "2026-W46", 5, 0)
	if err != nil || page.Status != "not_configured" || len(page.Items) != 0 || calls.Load() != before {
		t.Fatalf("read another user's private connection %#v %v", page, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "owners-private-session") {
		t.Fatal("source credential was serialized")
	}
	var count int64
	if err := store.DB.Model(&persistence.ProgressEvent{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("source reads unexpectedly wrote progress: %d %v", count, err)
	}
}

func TestExternalOutcomesRejectInvalidSourceAndWeek(t *testing.T) {
	store, user, _ := panelTestStore(t)
	service := NewRemoteToolService(store, nil)
	for _, input := range []struct {
		system, week  string
		limit, offset int
	}{{"unknown", "all", 5, 0}, {"fastread", "bad-week", 5, 0}, {"fastread", "all", 11, 0}, {"fastread", "all", 5, -1}} {
		if _, err := service.ExternalOutcomes(context.Background(), user, input.system, input.week, input.limit, input.offset); err != ErrValidation {
			t.Errorf("invalid request did not fail: %#v %v", input, err)
		}
	}
}
