package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Unauthenticated endpoints that cost the server something per call get a per-client token bucket.
// Sign-in has its own failure throttle (platform/auth); these are the rest: starting a FastCAS login
// writes a transaction row, completing one and a back-channel logout verify signed tokens, and a refresh
// does a database round trip for any string it is handed. The limits are generous for a person and a
// browser, and only stop a client that is looping.
type publicLimit struct {
	method, path string
	perMinute    float64
}

var publicLimits = []publicLimit{
	{http.MethodPost, "/api/v1/auth/refresh", 60},
	{http.MethodGet, "/api/v1/auth/fastcas/login", 20},
	{http.MethodGet, "/api/v1/auth/fastcas/callback", 30},
	{http.MethodPost, "/api/v1/auth/fastcas/complete", 30},
	{http.MethodPost, "/api/v1/auth/fastcas/backchannel-logout", 120},
}

type bucket struct {
	tokens float64
	seen   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	swept   time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

// allow takes one token from the bucket for key, refilling at perMinute with a burst of the same size.
// It returns how long to wait when the bucket is empty.
func (l *rateLimiter) allow(key string, perMinute float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) > time.Minute || len(l.buckets) > 50_000 {
		// A bucket idle for two minutes is full again; forgetting it changes nothing.
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 2*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: perMinute, seen: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(perMinute, b.tokens+now.Sub(b.seen).Minutes()*perMinute)
	b.seen = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / perMinute * float64(time.Minute))
	}
	b.tokens--
	return true, 0
}

func publicRateLimit(limiter *rateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, limit := range publicLimits {
			if c.Request.Method != limit.method || c.Request.URL.Path != limit.path {
				continue
			}
			ok, wait := limiter.allow(limit.path+"\x00"+c.ClientIP(), limit.perMinute)
			if !ok {
				c.Header("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"title": "Too Many Requests", "status": http.StatusTooManyRequests, "detail": "too many requests from this client; retry later"})
				return
			}
			break
		}
		c.Next()
	}
}
