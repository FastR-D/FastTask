package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// No idempotency record may hold a bearer credential: the database keeps refresh and device tokens
// only as digests, and a stored response would undo that.
func TestIdempotencyNeverStoresCredentials(t *testing.T) {
	api := newTestAPI(t)
	access := api.access
	api.access = ""
	login := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": "admin", "password": "password-for-tests"}, map[string]string{"Idempotency-Key": "login-key"})
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d %s", login.Code, login.Body.String())
	}
	var tokens struct {
		RefreshToken string `json:"refresh_token"`
	}
	decode(t, login, &tokens)
	refreshed := api.do(t, http.MethodPost, "/api/v1/auth/refresh", map[string]any{"refresh_token": tokens.RefreshToken}, map[string]string{"Idempotency-Key": "refresh-key"})
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh status=%d %s", refreshed.Code, refreshed.Body.String())
	}
	var rotated struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decode(t, refreshed, &rotated)

	api.access = access
	headers := map[string]string{"Idempotency-Key": "device-key"}
	device := map[string]any{"name": "Desk panel", "kind": "eink_panel", "timezone": "Asia/Shanghai"}
	created := api.do(t, http.MethodPost, "/api/v1/devices", device, headers)
	if created.Code != http.StatusCreated {
		t.Fatalf("device status=%d %s", created.Code, created.Body.String())
	}
	var deviceBody struct {
		DeviceToken string `json:"device_token"`
	}
	decode(t, created, &deviceBody)
	if deviceBody.DeviceToken == "" {
		t.Fatal("device token missing from the first response")
	}
	// The retry is still exactly-once, but does not get the secret back.
	replay := api.do(t, http.MethodPost, "/api/v1/devices", device, headers)
	if replay.Code != http.StatusConflict || !strings.Contains(replay.Body.String(), "IDEMPOTENCY_RESPONSE_WITHHELD") {
		t.Fatalf("replay status=%d %s", replay.Code, replay.Body.String())
	}
	var devices int64
	api.store.DB.Model(&persistence.Device{}).Count(&devices)
	if devices != 1 {
		t.Fatalf("devices=%d, want 1", devices)
	}

	var records []persistence.IdempotencyRecord
	if err := api.store.DB.Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		for name, secret := range map[string]string{"refresh token": rotated.RefreshToken, "access token": rotated.AccessToken, "login refresh token": tokens.RefreshToken, "device token": deviceBody.DeviceToken} {
			if strings.Contains(record.ResponseJSON, secret) {
				t.Fatalf("idempotency record for %s %s stores the %s", record.Method, record.Path, name)
			}
		}
	}
}
