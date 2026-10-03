package httpapi

import (
	"net/http"
	"testing"
	"time"
)

func TestRateLimiterRefillsAndBounds(t *testing.T) {
	limiter := newRateLimiter()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return clock }
	for i := 0; i < 20; i++ {
		if ok, _ := limiter.allow("k", 20); !ok {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}
	ok, wait := limiter.allow("k", 20)
	if ok || wait <= 0 || wait > 3*time.Second+time.Millisecond {
		t.Fatalf("21st request: ok=%v wait=%s", ok, wait)
	}
	clock = clock.Add(3 * time.Second)
	if ok, _ := limiter.allow("k", 20); !ok {
		t.Fatal("bucket did not refill")
	}
	if ok, _ := limiter.allow("other", 20); !ok {
		t.Fatal("one client's bucket limited another")
	}
}

func TestRefreshEndpointIsRateLimitedPerClient(t *testing.T) {
	api := newTestAPI(t)
	api.access = ""
	limited := 0
	for i := 0; i < 70; i++ {
		response := api.do(t, http.MethodPost, "/api/v1/auth/refresh", map[string]any{"refresh_token": "not-a-real-refresh-token-value"}, map[string]string{"Idempotency-Key": "k"})
		if response.Code == http.StatusTooManyRequests {
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
			limited++
		}
	}
	if limited == 0 || limited > 15 {
		t.Fatalf("%d of 70 refreshes were limited, want the tail beyond 60", limited)
	}
}
