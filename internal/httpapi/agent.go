package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
)

// Agent transport endpoints (doc/agent-impl.md §2, §9).
//
// These three routes are registered as BARE Gin handlers, not through Huma,
// because Huma's sse helper forces an `event:` line that breaks the
// assistant-transport strict decoder (§9.1, ADR-0002 §4). They are also kept off
// the /api/v1 group so the idempotency middleware — which buffers response
// bodies — never touches a stream (§9.3). Their OpenAPI descriptions are
// maintained by hand in registerAgentOpenAPI (§9.1).
const (
	agentPollInterval    = 25 * time.Millisecond
	agentStreamMaxAge    = 190 * time.Second // a little over the 180s run wall clock (§6)
	agentMaxCommandBytes = 1 << 20           // command payloads are small; reject oversized bodies
	agentHeartbeatEvery  = 15 * time.Second  // §9.3
)

// registerAgent mounts the three streaming endpoints on a dedicated group that
// inherits only the global middleware (request id, recovery, security headers,
// CORS) plus a body limit — never the idempotency middleware.
func (s *Server) registerAgent() {
	group := s.Engine.Group("/api/v1/agent")
	group.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, agentMaxCommandBytes)
		c.Next()
	})
	group.POST("/commands", s.handleAgentCommands)
	group.POST("/resume-state", s.handleAgentResumeState)
	group.POST("/resume", s.handleAgentResume)
	s.registerAgentOpenAPI()
}

// authenticateAgent resolves the caller from the Authorization header. user_id
// always comes from here, never from the request body (agent-impl.md §2.2, §4
// invariant 5).
func (s *Server) authenticateAgent(c *gin.Context) (platformauth.Principal, bool) {
	principal, err := s.auth.Authenticate(c.GetHeader("Authorization"))
	if err != nil {
		agentAbort(c, http.StatusUnauthorized, "authentication required")
		return platformauth.Principal{}, false
	}
	return principal, true
}

// handleAgentCommands implements POST /agent/commands (§2.2, §8): create the run
// transactionally, then stream it. The run is executed by the Worker; this
// handler only subscribes to the persisted chunk log and forwards it.
func (s *Server) handleAgentCommands(c *gin.Context) {
	principal, ok := s.authenticateAgent(c)
	if !ok {
		return
	}
	var req application.CommandsRequest
	if err := decodeAgentBody(c, &req); err != nil {
		agentAbort(c, http.StatusBadRequest, "invalid command payload")
		return
	}
	// §2.2: state/system/tools in the body are untrusted and ignored. SubmitCommands
	// reads none of them; the server is the sole source of authoritative state.
	// An add-tool-result command is routed to the approval flow (§7.2): it resolves
	// the pending proposal synchronously (§7.3) and resumes the same run, streaming
	// only the continuation from the run's checkpoint (result.FromSeq).
	result, err := s.agent.SubmitCommands(c.Request.Context(), principal.UserID, req)
	if err != nil {
		agentSubmitError(c, err)
		return
	}
	s.streamRun(c, principal.UserID, result.RunID, result.FromSeq)
}

// handleAgentResumeState implements POST /agent/resume-state (§2.8): 204 when no
// run is in flight, otherwise 200 with the retained state and a string runId.
func (s *Server) handleAgentResumeState(c *gin.Context) {
	principal, ok := s.authenticateAgent(c)
	if !ok {
		return
	}
	var body struct {
		ThreadID string `json:"threadId"`
	}
	if err := decodeAgentBody(c, &body); err != nil || strings.TrimSpace(body.ThreadID) == "" {
		agentAbort(c, http.StatusBadRequest, "threadId is required")
		return
	}
	result, found, err := s.agent.ResumeState(c.Request.Context(), principal.UserID, strings.TrimSpace(body.ThreadID))
	if err != nil {
		if persistence.IsNotFound(err) {
			agentAbort(c, http.StatusNotFound, "thread not found")
			return
		}
		agentAbort(c, http.StatusInternalServerError, "internal server error")
		return
	}
	if !found {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, result)
}

// handleAgentResume implements POST /agent/resume (§2.8): replay the run's chunk
// log from its checkpoint and continue streaming. The body carries runId and an
// empty commands array; it must NOT carry state (§2.8).
func (s *Server) handleAgentResume(c *gin.Context) {
	principal, ok := s.authenticateAgent(c)
	if !ok {
		return
	}
	var body struct {
		Commands []json.RawMessage `json:"commands"`
		RunID    string            `json:"runId"`
	}
	if err := decodeAgentBody(c, &body); err != nil || strings.TrimSpace(body.RunID) == "" {
		agentAbort(c, http.StatusBadRequest, "runId is required")
		return
	}
	// Ownership check: a cross-user or missing runId is 404 (§7.4, §10).
	run, err := s.agent.Repository().GetRun(c.Request.Context(), principal.UserID, strings.TrimSpace(body.RunID))
	if err != nil {
		if persistence.IsNotFound(err) {
			agentAbort(c, http.StatusNotFound, "run not found")
			return
		}
		agentAbort(c, http.StatusInternalServerError, "internal server error")
		return
	}
	s.streamRun(c, principal.UserID, run.ID, run.CheckpointSeq)
}

// streamRun writes the SSE response for a run, replaying persisted chunks from
// fromSeq and then following the run until it reaches a stream-terminal state.
//
// The run is owned by the Worker, not this request: a client disconnect stops
// the stream but never cancels the run (§4.3). Cancellation is only via
// PUT /agent-jobs/{id}/cancellation.
func (s *Server) streamRun(c *gin.Context, userID, runID string, fromSeq int) {
	repo := s.agent.Repository()
	ctx := c.Request.Context()

	// Streaming endpoints must not inherit the 60s per-request write deadline set
	// by the server (§9.2); clear it so a long run is not severed.
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})

	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no") // reverse proxies must not buffer (§9.3)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	writer := protocol.NewStreamWriter(c.Writer, c.Writer.Flush)
	cursor := fromSeq
	deadline := time.Now().Add(agentStreamMaxAge)
	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(agentHeartbeatEvery)
	defer heartbeat.Stop()

	for {
		run, err := repo.GetRun(ctx, userID, runID)
		if err != nil {
			// The run vanished or is not ours; end the stream cleanly.
			break
		}
		// Drain every chunk the Worker has persisted so far. Chunks are committed
		// before the run status flips terminal, so a terminal read implies the log
		// is complete.
		if !s.drainChunks(ctx, writer, userID, runID, &cursor) {
			return // client went away mid-write
		}
		if agentStreamTerminal(run.Status) {
			break
		}
		if time.Now().After(deadline) {
			_ = writer.WriteChunk(protocol.ErrorChunk())
			break
		}
		select {
		case <-ctx.Done():
			// Client disconnected. Do NOT cancel the run (§4.3); just stop writing.
			return
		case <-heartbeat.C:
			if err := writer.WriteComment("ping"); err != nil {
				return
			}
		case <-ticker.C:
		}
	}
	_ = writer.WriteDone()
}

// drainChunks writes all persisted chunks with seq >= *cursor and advances the
// cursor. It returns false if the client connection is gone.
func (s *Server) drainChunks(ctx context.Context, writer *protocol.StreamWriter, userID, runID string, cursor *int) bool {
	repo := s.agent.Repository()
	for {
		chunks, err := repo.ListChunksFrom(ctx, userID, runID, *cursor)
		if err != nil || len(chunks) == 0 {
			return true
		}
		for _, chunk := range chunks {
			if err := writer.WriteRaw(chunk.ChunkJSON); err != nil {
				return false
			}
			*cursor = chunk.Seq + 1
		}
	}
}

// agentStreamTerminal reports whether a run status ends the current HTTP stream.
// awaiting_approval ends the stream (§4: the run yields control and the flow
// sends [DONE]) even though the run itself is not finished.
func agentStreamTerminal(status string) bool {
	switch status {
	case persistence.RunSucceeded, persistence.RunFailed, persistence.RunCancelled,
		persistence.RunInterrupted, persistence.RunAwaitingApproval:
		return true
	default:
		return false
	}
}

// decodeAgentBody parses an agent request body. Unknown fields are tolerated,
// not rejected: the client legitimately sends callSettings/config and the
// untrusted state/system/tools, all of which the server ignores (§2.2).
func decodeAgentBody(c *gin.Context, target any) error {
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// agentSubmitError maps SubmitCommands / ResolveApproval failures to HTTP
// statuses. A cross-user or missing thread/tool-call is 404 (§2.2, §7.4); a busy
// thread or duplicate approval receipt is 409 (§9.3, §7.4); a proposal whose base
// revision moved is 412 (§7.4); an empty/invalid command is 422.
func agentSubmitError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, application.ErrEmptyCommand), errors.Is(err, application.ErrValidation):
		agentAbort(c, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, application.ErrApprovalDuplicate), errors.Is(err, application.ErrApprovalNotAwaiting), errors.Is(err, application.ErrConflict):
		agentAbort(c, http.StatusConflict, err.Error())
	case errors.Is(err, application.ErrRevision):
		agentAbort(c, http.StatusPreconditionFailed, "the task tree changed before this approval; the proposal was marked conflict")
	case errors.Is(err, persistence.ErrActiveRunExists):
		agentAbort(c, http.StatusConflict, "an agent run is already active")
	case persistence.IsNotFound(err):
		agentAbort(c, http.StatusNotFound, "thread not found")
	default:
		agentAbort(c, http.StatusInternalServerError, "internal server error")
	}
}

func agentAbort(c *gin.Context, status int, detail string) {
	c.AbortWithStatusJSON(status, gin.H{"title": http.StatusText(status), "status": status, "detail": detail})
}

// registerAgentOpenAPI hand-writes the OpenAPI description for the three
// streaming endpoints so other FastResearch projects still get a complete
// contract even though these routes bypass Huma (§9.1). Paths are relative to the
// /api/v1 server URL, matching Huma's own registration.
func (s *Server) registerAgentOpenAPI() {
	api := s.API.OpenAPI()
	if api.Paths == nil {
		api.Paths = map[string]*huma.PathItem{}
	}
	bearer := []map[string][]string{{"userBearer": {}}}
	jsonBody := func(desc string) *huma.RequestBody {
		return &huma.RequestBody{
			Description: desc,
			Required:    true,
			Content:     map[string]*huma.MediaType{"application/json": {}},
		}
	}
	sseResponses := map[string]*huma.Response{
		"200": {
			Description: "assistant-transport SSE stream: one `data:` line per chunk, terminated by `data: [DONE]`. No `event:` lines are emitted.",
			Content:     map[string]*huma.MediaType{"text/event-stream": {}},
		},
		"401": {Description: "authentication required"},
		"404": {Description: "thread or run not found (also returned for cross-user access)"},
	}
	api.Paths["/agent/commands"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "agent-commands",
		Summary:     "Submit agent commands and stream a run",
		Description: "assistant-transport `api` endpoint. Creates a run from add-message commands and streams assistant-transport chunks. Request `state`, `system` and `tools` are untrusted and ignored (doc/agent-impl.md §2.2).",
		Security:    bearer,
		RequestBody: jsonBody("commands, threadId, parentId; state/system/tools are ignored"),
		Responses:   withConflict(sseResponses),
	}}
	api.Paths["/agent/resume-state"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "agent-resume-state",
		Summary:     "Fetch retained state and runId for a thread",
		Description: "assistant-transport `resumeStateApi` endpoint. Returns 204 when no run is in flight, otherwise 200 with {state, runId} (doc/agent-impl.md §2.8).",
		Security:    bearer,
		RequestBody: jsonBody("{threadId}"),
		Responses: map[string]*huma.Response{
			"200": {
				Description: "an active run exists",
				Content:     map[string]*huma.MediaType{"application/json": {}},
			},
			"204": {Description: "no active run for the thread"},
			"401": {Description: "authentication required"},
			"404": {Description: "thread not found (also returned for cross-user access)"},
		},
	}}
	api.Paths["/agent/resume"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "agent-resume",
		Summary:     "Resume streaming an in-progress run",
		Description: "assistant-transport `resumeApi` endpoint. Replays the run's chunk log from its checkpoint and continues. Body carries runId and an empty commands array, and must not carry state (doc/agent-impl.md §2.8).",
		Security:    bearer,
		RequestBody: jsonBody("{runId, commands: []}"),
		Responses:   sseResponses,
	}}
}

func withConflict(responses map[string]*huma.Response) map[string]*huma.Response {
	out := make(map[string]*huma.Response, len(responses)+2)
	for k, v := range responses {
		out[k] = v
	}
	out["409"] = &huma.Response{Description: "an agent run is already active for this user"}
	out["422"] = &huma.Response{Description: "command carried no usable message text"}
	return out
}
