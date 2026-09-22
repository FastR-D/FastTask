package httpapi

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
)

// The harness endpoints that go through Huma (doc/interface.md §20.2, §20.5). Only the model
// proxy and the approval long poll bypass it; everything here is an ordinary request/response
// with a documented DTO, so it is registered like every other domain and shows up in the
// generated OpenAPI.
//
// Two authentication subjects appear, and mixing them is an implementation error (§20.1):
// the user's own JWT for the operations a user performs, and the run capability token for the
// ones a host performs.

// harnessSecurity marks an operation as requiring a harness token. The middleware resolves it
// into a principal the handler reads with harnessPrincipal.
func harnessSecurity() []map[string][]string { return []map[string][]string{{"harnessToken": {}}} }

type harnessPrincipalKey struct{}

// harnessPrincipal returns the capability a harness token resolved to. UserID always comes from
// here, never from a request body (doc/harness.md §5).
func harnessPrincipal(ctx context.Context) application.HarnessPrincipal {
	value, _ := ctx.Value(harnessPrincipalKey{}).(application.HarnessPrincipal)
	return value
}

type toolManifestBody struct {
	Tools []application.ToolDescriptor `json:"tools"`
}

type toolManifestResponse struct {
	ETag string `header:"ETag"`
	Body toolManifestBody
}

type runCancellationBody struct {
	RunID           string `json:"run_id"`
	Status          string `json:"status"`
	CancelRequested bool   `json:"cancel_requested"`
}

type checkpointInput struct {
	ThreadID string `path:"thread_id"`
	Body     struct {
		// Checkpoint is opaque versioned bytes; the server never interprets them
		// (doc/harness.md §6.1).
		Checkpoint   []byte `json:"checkpoint"`
		LibfxVersion string `json:"libfx_version"`
		// StopReason makes this write the run's terminal signal (§5.1): one user message is one
		// run is one turn, so the checkpoint lands exactly once, when the turn ends.
		StopReason   string         `json:"stop_reason,omitempty"`
		ErrorMessage string         `json:"error_message,omitempty"`
		Usage        map[string]any `json:"usage,omitempty"`
	}
}

type checkpointBody struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

func (s harnessRoutes) RegisterRoutes(api huma.API) {
	type emptyInput struct{}
	s.registerThreadRoutes(api)
	s.registerAttachmentRoutes(api)

	// The manifest is the single source of truth a host builds its tools from
	// (doc/harness.md §3.4). The etag is what makes a mid-run manifest change detectable
	// (§10.3).
	register(api, "list-agent-tools", http.MethodGet, "/agent/tools", "List agent tools", userSecurity(), func(ctx context.Context, input *emptyInput) (*toolManifestResponse, error) {
		return &toolManifestResponse{
			ETag: s.agent.ToolsETag(),
			Body: toolManifestBody{Tools: s.agent.ToolManifest()},
		}, nil
	})

	type grantInput struct {
		Body struct {
			RunID     string `json:"run_id" required:"true"`
			ToolsETag string `json:"tools_etag,omitempty"`
		}
	}
	register(api, "create-agent-run-grant", http.MethodPost, "/agent/runs", "Issue a harness run capability", userSecurity(), func(ctx context.Context, input *grantInput) (*itemResponse[application.RunGrant], error) {
		grant, err := s.agent.IssueRunGrant(ctx, principal(ctx).UserID, strings.TrimSpace(input.Body.RunID), input.Body.ToolsETag)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[application.RunGrant]{Body: grant}, nil
	})

	type toolInput struct {
		RunID string `path:"run_id"`
		Name  string `path:"name"`
		Body  application.HarnessToolCall
	}
	register(api, "execute-agent-tool", http.MethodPost, "/agent/runs/{run_id}/tools/{name}", "Execute an agent tool for a harness host", harnessSecurity(), func(ctx context.Context, input *toolInput) (*itemResponse[application.HarnessToolOutcome], error) {
		principal := harnessPrincipal(ctx)
		if strings.TrimSpace(input.RunID) != principal.RunID {
			return nil, huma.NewError(http.StatusUnauthorized, "HARNESS_TOKEN_INVALID: the token is not valid for this run")
		}
		outcome, err := s.agent.ExecuteHarnessTool(ctx, principal, strings.TrimSpace(input.Name), input.Body)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[application.HarnessToolOutcome]{Body: outcome}, nil
	})

	type heartbeatInput struct {
		RunID string `path:"run_id"`
	}
	register(api, "beat-agent-run", http.MethodPost, "/agent/runs/{run_id}/heartbeat", "Report a harness host is alive", harnessSecurity(), func(ctx context.Context, input *heartbeatInput) (*itemResponse[application.HeartbeatReply], error) {
		principal := harnessPrincipal(ctx)
		if strings.TrimSpace(input.RunID) != principal.RunID {
			return nil, huma.NewError(http.StatusUnauthorized, "HARNESS_TOKEN_INVALID: the token is not valid for this run")
		}
		reply, err := s.agent.HarnessHeartbeat(ctx, principal)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[application.HeartbeatReply]{Body: reply}, nil
	})

	// Cancellation is the user's power, not the host's: this one deliberately requires the
	// user's JWT and refuses a harness token (doc/harness.md §10.2, §14.16).
	register(api, "cancel-agent-run", http.MethodPost, "/agent/runs/{run_id}/cancellation", "Cancel an agent run", userSecurity(), func(ctx context.Context, input *heartbeatInput) (*itemResponse[runCancellationBody], error) {
		runID := strings.TrimSpace(input.RunID)
		if err := s.agent.RequestCancellation(ctx, principal(ctx).UserID, runID); err != nil {
			return nil, mapHarnessError(err)
		}
		run, err := s.agent.Repository().GetRun(ctx, principal(ctx).UserID, runID)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[runCancellationBody]{Body: runCancellationBody{
			RunID: run.ID, Status: run.Status, CancelRequested: run.CancelRequested,
		}}, nil
	})

	register(api, "get-agent-thread-checkpoint", http.MethodGet, "/agent/threads/{thread_id}/checkpoint", "Read a thread's harness checkpoint", harnessSecurity(), func(ctx context.Context, input *heartbeatInput2) (*itemResponse[application.ThreadCheckpoint], error) {
		principal := harnessPrincipal(ctx)
		if strings.TrimSpace(input.ThreadID) != principal.ThreadID {
			return nil, huma.NewError(http.StatusUnauthorized, "HARNESS_TOKEN_INVALID: the token is not valid for this thread")
		}
		checkpoint, err := s.agent.GetThreadCheckpoint(ctx, principal)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[application.ThreadCheckpoint]{Body: checkpoint}, nil
	})

	register(api, "put-agent-thread-checkpoint", http.MethodPut, "/agent/threads/{thread_id}/checkpoint", "Store a thread's harness checkpoint", harnessSecurity(), func(ctx context.Context, input *checkpointInput) (*itemResponse[checkpointBody], error) {
		principal := harnessPrincipal(ctx)
		if strings.TrimSpace(input.ThreadID) != principal.ThreadID {
			return nil, huma.NewError(http.StatusUnauthorized, "HARNESS_TOKEN_INVALID: the token is not valid for this thread")
		}
		if strings.TrimSpace(input.Body.StopReason) == "" {
			// A plain checkpoint write: the run keeps going (§6.1).
			if err := s.agent.PutThreadCheckpoint(ctx, principal, input.Body.Checkpoint, input.Body.LibfxVersion); err != nil {
				return nil, mapHarnessError(err)
			}
			return &itemResponse[checkpointBody]{Body: checkpointBody{RunID: principal.RunID, Status: "running"}}, nil
		}
		// The turn ended: store the checkpoint and move the run to its terminal state (§5.1).
		completion := application.Completion{
			StopReason: input.Body.StopReason, ErrorMessage: input.Body.ErrorMessage, Usage: input.Body.Usage,
			Checkpoint: input.Body.Checkpoint, LibfxVersion: input.Body.LibfxVersion,
		}
		if err := s.agent.CompleteRun(ctx, principal, completion); err != nil {
			return nil, mapHarnessError(err)
		}
		run, err := s.agent.Repository().GetRun(ctx, principal.UserID, principal.RunID)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[checkpointBody]{Body: checkpointBody{RunID: run.ID, Status: run.Status}}, nil
	})
}

// heartbeatInput2 is the thread-scoped path input. It is a distinct type from heartbeatInput
// because the path parameter differs, and Huma derives the OpenAPI schema from the type.
type heartbeatInput2 struct {
	ThreadID string `path:"thread_id"`
}

// mapHarnessError turns a harness failure into the documented HTTP status and error code
// (doc/interface.md §20, §8). The code travels in the detail because Huma's problem-details
// model has no code field; the prefix is what a client keys on.
func mapHarnessError(err error) error {
	switch {
	case errors.Is(err, application.ErrHarnessToken):
		return huma.NewError(http.StatusUnauthorized, "HARNESS_TOKEN_INVALID: the run capability is not valid")
	case errors.Is(err, application.ErrToolsETagStale):
		return huma.NewError(http.StatusConflict, "TOOLS_ETAG_STALE: the tool manifest changed; fetch /agent/tools again")
	case errors.Is(err, application.ErrHarnessUnavailable):
		return huma.NewError(http.StatusServiceUnavailable, "HARNESS_UNAVAILABLE: no host can drive this run")
	case errors.Is(err, application.ErrRunNotActive):
		return huma.NewError(http.StatusConflict, "RUN_NOT_ACTIVE: the run already finished")
	case errors.Is(err, application.ErrApprovalTimeout):
		return huma.NewError(http.StatusConflict, "APPROVAL_TIMEOUT: no decision within the wait limit")
	case errors.Is(err, application.ErrValidation):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, application.ErrNotFound), persistence.IsNotFound(err):
		return huma.Error404NotFound("resource not found")
	default:
		return huma.NewError(http.StatusInternalServerError, "the harness request could not be served")
	}
}

// --- conversation threads (doc/interface.md §20.3, doc/chat-features.md §2) ---

// threadBody is one thread on the wire. The field names are snake_case per §1.1; the frontend adapter
// maps them onto assistant-ui's RemoteThreadMetadata (doc/chat-features.md §2.2).
type threadBody struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	GoalID        *string    `json:"goal_id,omitempty"`
	Custom        string     `json:"custom,omitempty"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func threadBodyOf(view application.ThreadView) threadBody {
	return threadBody{
		ID: view.ID, Title: view.Title, Status: view.Status, GoalID: view.GoalID,
		Custom: view.Custom, LastMessageAt: view.LastMessageAt,
		CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
	}
}

// registerThreadRoutes mounts the thread catalogue. Every route takes the user's JWT: a thread is the
// user's property, not a run capability (§20.1).
func (s harnessRoutes) registerThreadRoutes(api huma.API) {
	type listInput struct {
		Status string `query:"status" enum:"regular,archived"`
		Cursor string `query:"cursor"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	}
	register(api, "list-agent-threads", http.MethodGet, "/agent/threads", "List conversation threads", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[threadBody], error) {
		page, err := s.agent.ListAgentThreads(ctx, principal(ctx).UserID, input.Status, input.Cursor, input.Limit)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		out := &listResponse[threadBody]{}
		out.Body.HasMore = page.HasMore
		if page.NextCursor != "" {
			cursor := page.NextCursor
			out.Body.NextCursor = &cursor
		}
		out.Body.Items = make([]threadBody, 0, len(page.Items))
		for _, item := range page.Items {
			out.Body.Items = append(out.Body.Items, threadBodyOf(item))
		}
		return out, nil
	})

	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			GoalID *string `json:"goal_id,omitempty"`
			Title  string  `json:"title,omitempty"`
		}
	}
	register(api, "create-agent-thread", http.MethodPost, "/agent/threads", "Create a conversation thread", userSecurity(), func(ctx context.Context, input *createInput) (*itemResponse[map[string]string], error) {
		view, err := s.agent.CreateAgentThread(ctx, principal(ctx).UserID, input.Body.GoalID, input.Body.Title)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		// remote_id is the name assistant-ui's thread list adapter expects (§2.2).
		return &itemResponse[map[string]string]{Body: map[string]string{"remote_id": view.ID}}, nil
	})

	type threadInput struct {
		ThreadID string `path:"thread_id"`
	}
	register(api, "get-agent-thread", http.MethodGet, "/agent/threads/{thread_id}", "Get a conversation thread", userSecurity(), func(ctx context.Context, input *threadInput) (*itemResponse[threadBody], error) {
		view, err := s.agent.GetAgentThread(ctx, principal(ctx).UserID, input.ThreadID)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[threadBody]{Body: threadBodyOf(view)}, nil
	})

	// The thread's transcript, in the same shape the SSE stream pushes (doc/agent-impl.md §2.7). A client
	// that switches conversation — or reloads, or signs in elsewhere — reads the thread it named instead of
	// whichever one happened to be last (doc/chat-features.md §2).
	register(api, "get-agent-thread-state", http.MethodGet, "/agent/threads/{thread_id}/state", "Read a thread's conversation state", userSecurity(), func(ctx context.Context, input *threadInput) (*itemResponse[map[string]any], error) {
		state, err := s.agent.ThreadState(ctx, principal(ctx).UserID, input.ThreadID)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[map[string]any]{Body: map[string]any{"state": state}}, nil
	})

	type patchInput struct {
		ThreadID string `path:"thread_id"`
		IfMatch  string `header:"If-Match"`
		Body     struct {
			// A missing field means "leave it alone"; an explicit empty string clears it (§1.5).
			Title  *string `json:"title,omitempty"`
			Custom *string `json:"custom,omitempty"`
		}
	}
	register(api, "update-agent-thread", http.MethodPatch, "/agent/threads/{thread_id}", "Rename a thread or update its display data", userSecurity(), func(ctx context.Context, input *patchInput) (*itemResponse[threadBody], error) {
		_ = input.IfMatch
		view, err := s.agent.UpdateAgentThread(ctx, principal(ctx).UserID, input.ThreadID, input.Body.Title, input.Body.Custom)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[threadBody]{Body: threadBodyOf(view)}, nil
	})

	register(api, "archive-agent-thread", http.MethodPost, "/agent/threads/{thread_id}/archive", "Archive a conversation thread", userSecurity(), func(ctx context.Context, input *threadInput) (*itemResponse[threadBody], error) {
		view, err := s.agent.SetAgentThreadStatus(ctx, principal(ctx).UserID, input.ThreadID, persistence.ThreadArchived)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[threadBody]{Body: threadBodyOf(view)}, nil
	})

	register(api, "unarchive-agent-thread", http.MethodPost, "/agent/threads/{thread_id}/unarchive", "Restore an archived conversation thread", userSecurity(), func(ctx context.Context, input *threadInput) (*itemResponse[threadBody], error) {
		view, err := s.agent.SetAgentThreadStatus(ctx, principal(ctx).UserID, input.ThreadID, persistence.ThreadRegular)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[threadBody]{Body: threadBodyOf(view)}, nil
	})

	register(api, "delete-agent-thread", http.MethodDelete, "/agent/threads/{thread_id}", "Delete a conversation thread and everything under it", userSecurity(), func(ctx context.Context, input *threadInput) (*deleteResponse, error) {
		if err := s.agent.DeleteAgentThread(ctx, principal(ctx).UserID, input.ThreadID); err != nil {
			return nil, mapHarnessError(err)
		}
		return &deleteResponse{}, nil
	})

	// A generated title is deterministic (§2.4): the first user message, folded and truncated. It costs no
	// model call, which is the point — a title is not worth a round trip or a quota.
	register(api, "generate-agent-thread-title", http.MethodPost, "/agent/threads/{thread_id}/title", "Generate a thread title", userSecurity(), func(ctx context.Context, input *threadInput) (*itemResponse[map[string]string], error) {
		title, err := s.agent.GenerateThreadTitle(ctx, principal(ctx).UserID, input.ThreadID)
		if err != nil {
			return nil, mapHarnessError(err)
		}
		return &itemResponse[map[string]string]{Body: map[string]string{"title": title}}, nil
	})
}

// deleteResponse is a 204 with no body.
type deleteResponse struct{}

// --- attachments (doc/interface.md §20.4, doc/chat-features.md §4) ---

// attachmentBody is what an upload returns (§4.3). The stored path is never part of it.
type attachmentBody struct {
	AttachmentID string `json:"attachment_id"`
	Mime         string `json:"mime"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Bytes        int64  `json:"bytes"`
}

// attachmentStream writes an image through Huma's raw-body escape hatch. The bytes are not a DTO, but the
// route stays registered so the contract still describes it (§20.5: the bypass list does not grow).
type attachmentStream struct {
	Body func(huma.Context)
}

func (s harnessRoutes) registerAttachmentRoutes(api huma.API) {
	type uploadInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		RawBody        multipart.Form
	}
	register(api, "create-agent-attachment", http.MethodPost, "/agent/attachments", "Upload an image attachment", userSecurity(), func(ctx context.Context, input *uploadInput) (*itemResponse[attachmentBody], error) {
		files := input.RawBody.File["file"]
		if len(files) == 0 {
			return nil, huma.Error400BadRequest("a file part named \"file\" is required")
		}
		if len(files) > 1 {
			return nil, huma.Error400BadRequest("one image per upload")
		}
		handle, err := files[0].Open()
		if err != nil {
			return nil, huma.Error400BadRequest("the upload could not be read")
		}
		defer handle.Close()
		// The declared type and the file name are read for nothing: the server sniffs the bytes (§4.5).
		// The limit is one byte over the cap, so an oversized upload is rejected rather than buffered.
		data, err := io.ReadAll(io.LimitReader(handle, application.AttachmentMaxBytes+1))
		if err != nil {
			return nil, huma.Error400BadRequest("the upload could not be read")
		}
		var threadID *string
		if values := input.RawBody.Value["thread_id"]; len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			scope := strings.TrimSpace(values[0])
			threadID = &scope
		}
		view, err := s.agent.Attachments().Upload(ctx, principal(ctx).UserID, threadID, data)
		if err != nil {
			return nil, mapAttachmentError(err)
		}
		return &itemResponse[attachmentBody]{Body: attachmentBody{
			AttachmentID: view.AttachmentID, Mime: view.Mime, Width: view.Width, Height: view.Height, Bytes: view.Bytes,
		}}, nil
	})

	type attachmentInput struct {
		AttachmentID string `path:"attachment_id"`
	}
	register(api, "get-agent-attachment", http.MethodGet, "/agent/attachments/{attachment_id}", "Read an image attachment", userSecurity(), func(ctx context.Context, input *attachmentInput) (*attachmentStream, error) {
		view, path, err := s.agent.Attachments().Get(ctx, principal(ctx).UserID, input.AttachmentID)
		if err != nil {
			return nil, mapAttachmentError(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, mapAttachmentError(err)
		}
		return &attachmentStream{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", view.Mime)
			hctx.SetHeader("Content-Disposition", `inline; filename="attachment"`)
			// Private: the bytes belong to one user, so a shared cache must not keep them.
			hctx.SetHeader("Cache-Control", "private, max-age=3600")
			hctx.SetStatus(http.StatusOK)
			_, _ = hctx.BodyWriter().Write(data)
		}}, nil
	})

	register(api, "delete-agent-attachment", http.MethodDelete, "/agent/attachments/{attachment_id}", "Delete an image attachment", userSecurity(), func(ctx context.Context, input *attachmentInput) (*deleteResponse, error) {
		if err := s.agent.Attachments().Delete(ctx, principal(ctx).UserID, input.AttachmentID); err != nil {
			return nil, mapAttachmentError(err)
		}
		return &deleteResponse{}, nil
	})
}

// mapAttachmentError turns an upload failure into a 400 with a message the user can act on (§4.5: every
// limit answers with a clear error, not a silent truncation).
func mapAttachmentError(err error) error {
	switch {
	case errors.Is(err, application.ErrAttachmentTooLarge):
		return huma.Error400BadRequest("ATTACHMENT_TOO_LARGE: " + err.Error())
	case errors.Is(err, application.ErrAttachmentQuota):
		return huma.Error400BadRequest("ATTACHMENT_QUOTA: " + err.Error())
	case errors.Is(err, application.ErrAttachmentEmpty):
		return huma.Error400BadRequest("ATTACHMENT_EMPTY: " + err.Error())
	case errors.Is(err, application.ErrUnsupportedImage):
		return huma.Error400BadRequest("ATTACHMENT_UNSUPPORTED: only png, jpeg, webp and gif images are accepted")
	case persistence.IsNotFound(err), errors.Is(err, application.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return huma.Error404NotFound("resource not found")
	default:
		return mapHarnessError(err)
	}
}
