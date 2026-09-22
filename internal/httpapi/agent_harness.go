package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
)

// The two harness endpoints that bypass Huma (doc/interface.md §20.5): the OpenAI-compatible
// model proxy, whose response format belongs to an external specification, and the approval
// long poll, whose segmented response Huma has no shape for. Both are bare Gin handlers on
// purpose — a streaming or held-open response must not pass through the idempotency
// middleware, which buffers bodies.
const (
	// agentProxyMaxBodyBytes bounds a proxied model request. It is larger than the command
	// limit because a request carries the whole conversation history, and because inline
	// image data has to be readable in order to be discarded (doc/harness.md §4.3).
	agentProxyMaxBodyBytes = 16 << 20
	// agentApprovalSegment is one long-poll segment (§7). Short enough that an intermediate
	// proxy does not cut the wait, long enough that a 15 minute wait is 30 requests.
	agentApprovalSegment = 30 * time.Second
)

// registerAgentHarness mounts the bypassing harness routes. Parameter names match the Huma
// registrations exactly, because Gin's router rejects two different names at the same
// position.
func (s *Server) registerAgentHarness() {
	proxy := s.Engine.Group("/api/v1/agent/runs/:run_id/openai")
	proxy.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, agentProxyMaxBodyBytes)
		c.Next()
	})
	proxy.POST("/chat/completions", s.handleAgentModelProxy)

	approvals := s.Engine.Group("/api/v1/agent/runs/:run_id/approvals")
	approvals.GET("/:proposal_id", s.handleAgentApprovalPoll)

	s.registerAgentHarnessOpenAPI()
}

// authenticateHarness resolves the harness principal from the Authorization header. A user JWT
// must fail here and a harness token must fail on the user endpoints: the two capabilities are
// deliberately not interchangeable (doc/harness.md §10.2, §14.16).
func (s *Server) authenticateHarness(c *gin.Context) (application.HarnessPrincipal, bool) {
	principal, err := s.agent.AuthenticateHarness(c.Request.Context(), c.GetHeader("Authorization"))
	if err != nil {
		agentAbort(c, http.StatusUnauthorized, "HARNESS_TOKEN_INVALID")
		return application.HarnessPrincipal{}, false
	}
	return principal, true
}

// handleAgentModelProxy implements POST /agent/runs/{run_id}/openai/chat/completions
// (doc/harness.md §4.3). It forwards the upstream stream to the host byte for byte; reading it
// is what writes the authoritative transcript, so the two can never disagree (§4.4).
func (s *Server) handleAgentModelProxy(c *gin.Context) {
	principal, ok := s.authenticateHarness(c)
	if !ok {
		return
	}
	if runID := strings.TrimSpace(c.Param("run_id")); runID != principal.RunID {
		// A token is bound to one run. Another run's id in the path is not a routing question,
		// it is a capability question, and the answer is the same as for an invalid token.
		agentAbort(c, http.StatusUnauthorized, "HARNESS_TOKEN_INVALID")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		agentAbort(c, http.StatusBadRequest, "the request body could not be read")
		return
	}

	// A streaming response must not inherit the server's per-request write deadline (§9.2).
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})

	stream, err := s.agent.OpenModelProxy(c.Request.Context(), principal, body)
	if err != nil {
		writeProxyError(c, err)
		return
	}
	defer stream.Body.Close()

	header := c.Writer.Header()
	header.Set("Content-Type", stream.ContentType)
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(stream.StatusCode)
	c.Writer.Flush()

	buffer := make([]byte, 32*1024)
	for {
		n, readErr := stream.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return // the host went away; closing the body ends the upstream call
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// writeProxyError answers a refused model call in the OpenAI error shape, so the host's SDK
// surfaces it as a provider error rather than an unparsable response.
func writeProxyError(c *gin.Context, err error) {
	var proxyErr *application.ProxyError
	if !errors.As(err, &proxyErr) {
		proxyErr = &application.ProxyError{Code: "INTERNAL", Status: http.StatusInternalServerError, Detail: "the model call could not be proxied"}
	}
	c.AbortWithStatusJSON(proxyErr.Status, gin.H{
		"error": gin.H{"message": proxyErr.Detail, "type": proxyErr.Code, "code": proxyErr.Code},
	})
}

// handleAgentApprovalPoll implements GET /agent/runs/{run_id}/approvals/{proposal_id}
// (doc/harness.md §7): one segment of the wait for a user decision. A "pending" answer is not
// a failure — the host polls again until the total wait limit, which the server enforces, is
// reached.
func (s *Server) handleAgentApprovalPoll(c *gin.Context) {
	principal, ok := s.authenticateHarness(c)
	if !ok {
		return
	}
	if runID := strings.TrimSpace(c.Param("run_id")); runID != principal.RunID {
		agentAbort(c, http.StatusUnauthorized, "HARNESS_TOKEN_INVALID")
		return
	}
	proposalID := strings.TrimSpace(c.Param("proposal_id"))
	if proposalID == "" {
		agentAbort(c, http.StatusBadRequest, "a proposal id is required")
		return
	}
	// The wait may hold the connection open for a whole segment; a write deadline would cut it.
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})

	wait, err := s.agent.WaitForApproval(c.Request.Context(), principal, proposalID, agentApprovalSegment)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The host disconnected. Nothing to answer, and the proposal stays pending (§7).
			return
		case persistence.IsNotFound(err):
			agentAbort(c, http.StatusNotFound, "proposal not found")
		case errors.Is(err, application.ErrRunNotActive):
			agentAbort(c, http.StatusConflict, "RUN_NOT_ACTIVE")
		case errors.Is(err, application.ErrHarnessToken):
			agentAbort(c, http.StatusUnauthorized, "HARNESS_TOKEN_INVALID")
		default:
			agentAbort(c, http.StatusInternalServerError, "the approval could not be read")
		}
		return
	}
	c.JSON(http.StatusOK, wait)
}

// registerAgentHarnessOpenAPI hand-writes the contract of the two bypassing endpoints, so the
// document stays complete for other FastResearch projects even though Huma does not serve them
// (doc/interface.md §20.5). Paths are relative to the /api/v1 server URL, matching Huma's own
// registration.
func (s *Server) registerAgentHarnessOpenAPI() {
	api := s.API.OpenAPI()
	if api.Paths == nil {
		api.Paths = map[string]*huma.PathItem{}
	}
	harness := []map[string][]string{{"harnessToken": {}}}
	jsonBody := func(desc string) *huma.RequestBody {
		return &huma.RequestBody{
			Description: desc,
			Required:    true,
			Content:     map[string]*huma.MediaType{"application/json": {}},
		}
	}
	api.Paths["/agent/runs/{run_id}/openai/chat/completions"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "proxy-agent-model",
		Summary:     "Proxy a model call for a harness host",
		Description: "OpenAI-compatible chat completions endpoint used by the host-side gateway shim. The server injects the real provider credentials, replaces the system prompt and the tool list with its own, discards inline image data, and writes the authoritative transcript while forwarding the stream unchanged (doc/harness.md §4.3, §4.4). The request body's own history is never persisted.",
		Security:    harness,
		RequestBody: jsonBody("an OpenAI-compatible chat.completions request; model, messages[0] and tools are replaced server-side"),
		Responses: map[string]*huma.Response{
			"200": {
				Description: "OpenAI-compatible SSE stream",
				Content:     map[string]*huma.MediaType{"text/event-stream": {}},
			},
			"401": {Description: "HARNESS_TOKEN_INVALID: the run capability is missing, expired, revoked or bound to another run"},
			"404": {Description: "the run does not exist"},
			"409": {Description: "RUN_TIMEOUT, MAX_TURNS or CANCELLED: the run may not make another model call"},
			"502": {Description: "PROVIDER_ERROR or PROVIDER_NO_TOOL_SUPPORT: the upstream model refused or failed"},
			"503": {Description: "HARNESS_UNAVAILABLE: no model is configured"},
		},
	}}
	api.Paths["/agent/runs/{run_id}/approvals/{proposal_id}"] = &huma.PathItem{Get: &huma.Operation{
		OperationID: "poll-agent-approval",
		Summary:     "Wait for an approval decision",
		Description: "One segment of the approval long poll that keeps a host's tool call open while the user decides (doc/harness.md §7). Returns status=pending when the segment elapsed without a decision, so the host polls again; approved, rejected, conflict, cancelled and timeout are final. Segments are 30 seconds and the total wait is capped at 15 minutes, which the server enforces and which does not count against the run wall clock.",
		Security:    harness,
		Responses: map[string]*huma.Response{
			"200": {
				Description: "the current state of the wait",
				Content:     map[string]*huma.MediaType{"application/json": {}},
			},
			"401": {Description: "HARNESS_TOKEN_INVALID: the run capability is not valid for this run"},
			"404": {Description: "the proposal does not exist (also returned for cross-user access)"},
			"409": {Description: "RUN_NOT_ACTIVE: the run already finished"},
		},
	}}
}
