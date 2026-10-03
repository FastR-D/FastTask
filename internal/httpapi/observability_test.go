package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDRejectsFreeText(t *testing.T) {
	for id, valid := range map[string]bool{
		"req_0190abc":                true,
		"3f2c1a9e-77aa-4b4e-9c11-aa": true,
		"":                           false,
		"has space":                  false,
		"line\nbreak":                false,
		strings.Repeat("a", 129):     false,
		"<script>alert(1)</script>":  false,
	} {
		if got := validRequestID(id); got != valid {
			t.Errorf("validRequestID(%q)=%v, want %v", id, got, valid)
		}
	}
}

func TestVersionEndpointReportsBuild(t *testing.T) {
	api := newTestAPI(t)
	response := api.do(t, http.MethodGet, "/health/version", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var body map[string]any
	decode(t, response, &body)
	for _, key := range []string{"version", "commit", "build_time", "go"} {
		if value, _ := body[key].(string); value == "" {
			t.Fatalf("version response lacks %q: %v", key, body)
		}
	}
	if !strings.HasPrefix(body["go"].(string), "go1.") {
		t.Fatalf("go=%v is not the runtime version", body["go"])
	}
}

// A panic is logged with its stack and the request ID the client received; the client gets nothing.
func TestPanicIsLoggedWithRequestIDAndHiddenFromClient(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(requestID(), accessLog(), recovery())
	engine.GET("/api/v1/boom", func(*gin.Context) { panic("secret internal detail") })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/boom?token=should-not-be-logged", nil)
	request.Header.Set("X-Request-ID", "req-test-1")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret internal detail") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Request-ID") != "req-test-1" {
		t.Fatalf("request id header=%q", response.Header().Get("X-Request-ID"))
	}
	var sawPanic, sawAccess bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if entry["request_id"] != "req-test-1" {
			t.Fatalf("log line without request id: %s", line)
		}
		switch entry["msg"] {
		case "http handler panic":
			sawPanic = strings.Contains(line, "secret internal detail") && strings.Contains(line, "stack")
		case "http request":
			sawAccess = entry["status"] == float64(500)
		}
	}
	if !sawPanic || !sawAccess {
		t.Fatalf("panic logged=%v access logged=%v\n%s", sawPanic, sawAccess, logs.String())
	}
	if strings.Contains(logs.String(), "should-not-be-logged") {
		t.Fatal("query string reached the log")
	}
}

func TestUntrustedPrivateProxyIsReportedOnce(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	if err := engine.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	engine.Use(clientAddress())
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
	for i := 0; i < 3; i++ {
		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		request.RemoteAddr = "172.18.0.1:40000"
		request.Header.Set("X-Forwarded-For", "203.0.113.50")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Body.String() != "172.18.0.1" {
			t.Fatalf("an untrusted peer chose its own client address: %s", response.Body.String())
		}
	}
	if got := strings.Count(logs.String(), "not a trusted proxy"); got != 1 {
		t.Fatalf("warned %d times, want once:\n%s", got, logs.String())
	}
}
