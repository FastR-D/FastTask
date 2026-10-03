package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/gin-gonic/gin"
)

type requestIDKey struct{}

// RequestIDFromContext returns the request ID the HTTP layer assigned, for log correlation.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID accepts a caller's X-Request-ID only when it looks like an identifier: it is echoed into a
// response header and written into every log line for the request, so free text from the network must
// not reach either.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !validRequestID(id) {
			id = persistence.NewID("req")
		}
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Next()
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

// accessLog writes one line per API request and one per failed non-API request. Health probes and
// successfully served static files are left out: a container healthcheck every 30 seconds and every
// hashed asset would bury the lines an operator reads. Only the path is logged, never the query string:
// OAuth callbacks carry codes and state there.
func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		status := c.Writer.Status()
		path := c.Request.URL.Path
		if status < http.StatusBadRequest && !strings.HasPrefix(path, "/api/") {
			return
		}
		level := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		case status >= http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusUnauthorized && status != http.StatusNotModified:
			level = slog.LevelWarn
		}
		slog.Log(c.Request.Context(), level, "http request",
			"request_id", RequestIDFromContext(c.Request.Context()),
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"bytes", c.Writer.Size(),
			"client_ip", c.ClientIP(),
		)
	}
}

// recovery turns a handler panic into a 500 that reveals nothing, and records what the client is not
// told: the panic value and the stack, keyed by the request ID the client did receive.
func recovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		slog.Error("http handler panic",
			"request_id", RequestIDFromContext(c.Request.Context()),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"panic", recovered,
			"stack", string(debug.Stack()),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"title": "Internal Server Error", "status": 500, "detail": "unexpected server error"})
	})
}
