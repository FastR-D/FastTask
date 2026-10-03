package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

// Every administrative operation the API publishes refuses an ordinary member. The list comes from the
// generated OpenAPI document rather than a hand-kept table, so an admin route added later is covered
// without anyone remembering to add it here.
func TestEveryAdminOperationRefusesAMember(t *testing.T) {
	api := newTestAPI(t)
	spec := api.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if spec.Code != http.StatusOK {
		t.Fatalf("openapi=%d", spec.Code)
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}

	hash, err := platformauth.HashPassword("member-password")
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	member := persistence.User{ID: persistence.NewID("user"), Identifier: "boundary-member", PasswordHash: hash, DisplayName: "Member", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := api.store.DB.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	login := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": member.Identifier, "password": "member-password"}, map[string]string{"Authorization": ""})
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	decode(t, login, &tokens)
	api.access = tokens.AccessToken

	param := regexp.MustCompile(`\{[^}]+\}`)
	checked := 0
	for path, operations := range document.Paths {
		if !strings.HasPrefix(path, "/admin/") && path != "/integrations/status" {
			continue
		}
		// A real id makes the request reach the authorization check rather than stop at a 404.
		concrete := param.ReplaceAllString(path, api.user.ID)
		for method := range operations {
			method = strings.ToUpper(method)
			headers := map[string]string{"If-Match": `"x"`, "Idempotency-Key": "boundary-" + method + path}
			response := api.do(t, method, "/api/v1"+concrete, map[string]any{}, headers)
			if response.Code < 400 || (response.Code != http.StatusForbidden && response.Code != http.StatusUnprocessableEntity && response.Code != http.StatusBadRequest) {
				t.Errorf("%s %s answered a member with %d: %s", method, path, response.Code, response.Body.String())
			}
			checked++
		}
	}
	if checked < 25 {
		t.Fatalf("only %d admin operations found; the document walk is broken", checked)
	}
}
