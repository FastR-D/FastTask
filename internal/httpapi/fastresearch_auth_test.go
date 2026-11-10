package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FastR-D/FastTask/internal/config"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/gin-gonic/gin"
)

func TestFastResearchIntegrationDoesNotFallBackToLegacyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Config{}
	server := &Server{cfg: cfg, auth: platformauth.New(nil, cfg)}
	router := gin.New()
	router.POST("/today", server.fastResearchToday)

	cases := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{name: "no credential", status: http.StatusUnauthorized},
		{name: "legacy key disabled", headers: map[string]string{"X-API-Key": "old-key"}, status: http.StatusUnauthorized},
		{name: "mixed credentials", headers: map[string]string{"Authorization": "Bearer token", "X-API-Key": "old-key"}, status: http.StatusBadRequest},
		{name: "service auth not configured", headers: map[string]string{"Authorization": "Bearer token"}, status: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/today", nil)
			for key, value := range tc.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}
}

func TestFastResearchServiceReceiverAllowlistIsExact(t *testing.T) {
	allowed := []string{"account-a", "account-b"}
	if !fastResearchServiceAllowsReceiverRef(allowed, "account-a") || !fastResearchServiceAllowsReceiverRef(allowed, "account-b") {
		t.Fatal("configured receiver was rejected")
	}
	for _, ref := range []string{"", "account-c", "ACCOUNT-A", "account-a "} {
		if fastResearchServiceAllowsReceiverRef(allowed, ref) {
			t.Fatalf("unexpected receiver access: %q", ref)
		}
	}
}
