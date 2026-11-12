package httpapi

import (
	"github.com/FastR-D/FastTask/internal/persistence"
	"net/http"
	"strings"
	"testing"
)

func TestAdminAccountsHTTPCreateEditAndDelete(t *testing.T) {
	api := newTestAPI(t)
	created := api.do(t, http.MethodPost, "/api/v1/admin/users", map[string]any{"identifier": "managed-user", "display_name": "Managed", "password": "initial-password-long", "role": "member", "status": "active"}, nil)
	if created.Code < 200 || created.Code >= 300 {
		t.Fatal(created.Code, created.Body.String())
	}
	var user persistence.User
	decode(t, created, &user)
	get := api.do(t, http.MethodGet, "/api/v1/admin/users/"+user.ID, nil, nil)
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing revision")
	}
	updated := api.do(t, http.MethodPatch, "/api/v1/admin/users/"+user.ID, map[string]any{"identifier": "renamed-user", "password": "replacement-password-long", "display_name": "Renamed", "role": "admin", "status": "disabled"}, map[string]string{"If-Match": etag})
	if updated.Code != 200 {
		t.Fatal(updated.Code, updated.Body.String())
	}
	if strings.Contains(updated.Body.String(), "password") {
		t.Fatal("credential exposed")
	}
	stale := api.do(t, http.MethodDelete, "/api/v1/admin/users/"+user.ID, nil, map[string]string{"If-Match": etag})
	if stale.Code != 412 {
		t.Fatal("stale delete accepted", stale.Code)
	}
	current := api.do(t, http.MethodGet, "/api/v1/admin/users/"+user.ID, nil, nil)
	removed := api.do(t, http.MethodDelete, "/api/v1/admin/users/"+user.ID, nil, map[string]string{"If-Match": current.Header().Get("ETag")})
	if removed.Code != http.StatusNoContent {
		t.Fatal(removed.Code, removed.Body.String())
	}
	listed := api.do(t, http.MethodGet, "/api/v1/admin/users", nil, nil)
	if strings.Contains(listed.Body.String(), "renamed-user") {
		t.Fatal("deleted account still listed")
	}
	current = api.do(t, http.MethodGet, "/api/v1/admin/users/"+api.user.ID, nil, nil)
	self := api.do(t, http.MethodDelete, "/api/v1/admin/users/"+api.user.ID, nil, map[string]string{"If-Match": current.Header().Get("ETag")})
	if self.Code != 409 {
		t.Fatal("self deletion accepted", self.Code)
	}
	var count int64
	if err := api.store.DB.Table("admin_audit_events").Where("detail_json LIKE ?", "%replacement-password%").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("plaintext credential audited")
	}
}
