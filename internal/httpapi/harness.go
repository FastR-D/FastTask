package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
