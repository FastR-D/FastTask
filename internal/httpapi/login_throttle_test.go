package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// A client that is not a trusted proxy cannot pick a fresh throttle key per guess by sending its own
// X-Forwarded-For, and a blocked client is told so with 429 rather than another 401.
func TestLoginThrottleIgnoresSpoofedForwardedFor(t *testing.T) {
	api := newTestAPI(t)
	api.access = ""
	for i := 0; i < 5; i++ {
		response := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": "admin", "password": "wrong-password-guess"}, map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i+1)})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	response := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": "admin", "password": "password-for-tests"}, map[string]string{"X-Forwarded-For": "198.51.100.99"})
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled login status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLoginRejectsOversizedPassword(t *testing.T) {
	api := newTestAPI(t)
	api.access = ""
	long := make([]byte, 2048)
	for i := range long {
		long[i] = 'a'
	}
	response := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": "admin", "password": string(long)}, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
