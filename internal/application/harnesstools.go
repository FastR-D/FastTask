package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// The host-facing tool surface (doc/harness.md §5, §7).
//
// A harness host drives the loop, so the server no longer iterates model turns; it
// answers the two kinds of call a host makes — "execute this tool" and "has the user
// decided yet" — and keeps the authoritative transcript while doing so. Everything here
// is scoped by the harness principal: the identity comes from the token, and a body that
// claims one is ignored (agent.md §4 invariant 5).

// ErrToolCallIDCollision means a model reused a tool call id that another run of the same user already
// owns. The id is the key that ties a result to a call (§4.4.1), so the run fails rather than writing into
// somebody else's transcript. Provider-generated ids make this practically unreachable; a host that
// fabricates ids does not.
var ErrToolCallIDCollision = errors.New("tool call id belongs to another run")

// approvalPollInterval is how often a long poll re-reads the decision. It is short
// enough that a cancellation reaches a waiting host well inside the 5 second budget §5.2
// promises, and long enough that a 15 minute wait is not a spin loop.
const approvalPollInterval = 250 * time.Millisecond

// harnessTools is the tool execution surface and the approval long poll. It is its own
// unit so the harness stays inside the wiring.md §9 method budget per struct.
type harnessTools struct{ harnessBase }

// HarnessToolCall is the body of POST /agent/runs/{run_id}/tools/{name} (§5).
//
// tool_call_id ties the execution back to the part the model proxy already wrote, so a result can never
// attach itself to the wrong call (§4.4.1) — but it is OPTIONAL, because a libfx host cannot send one:
// HostTool.execute() receives the input and an abort signal and no call id. With it absent the call is
// matched on (run, tool name) instead, which is unambiguous because a host runs same-name calls one at a
// time. Both fields are marked not-required so the schema says what the server actually accepts; a
// required tool_call_id made every host tool call fail validation before it reached that logic.
type HarnessToolCall struct {
	ToolCallID string         `json:"tool_call_id" required:"false"`
	Input      map[string]any `json:"input" required:"false"`
}

// HarnessToolOutcome is what a host's execute() resolves with. A tool failure is a
// structured error the model can correct itself on, not a failed run (§5 item 6).
type HarnessToolOutcome struct {
	// Status is "ok", "pending" (a proposal awaiting the user) or "error".
	Status     string `json:"status"`
	ProposalID string `json:"proposal_id,omitempty"`
	Result     any    `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ApprovalWait is one segment of the approval long poll (§7). Status "pending" means the
// segment ended without a decision and the host must poll again; the total wait is
// enforced server-side, so a host that keeps polling still ends at "timeout".
type ApprovalWait struct {
	Status     string `json:"status"`
	ProposalID string `json:"proposal_id"`
	Result     any    `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	// WaitedSeconds is the accumulated wait across segments. It is diagnostic: the
	// authoritative exclusion from the wall clock lives on the run row.
	WaitedSeconds int `json:"waited_seconds"`
}

// ExecuteHarnessTool runs one tool call for a host. It mirrors what the in-process loop
// did per call — validate, execute outside any business transaction, record the result on
// the authoritative part — with the two differences the harness forces:
//
//   - the tool-call part already exists, written by the model proxy when the model named
//     the call (§4.4.1). This method writes only the RESULT onto it, so a call is never
//     recorded twice. A missing part is created rather than rejected: a host can reach
//     this endpoint before the proxy finished reading the stream.
//   - a proposal tool does not end the run. It parks the run at awaiting_approval and the
//     host waits on the long poll, so the same turn continues after the decision (§7)
//     instead of a new run being created.
func (s *harnessTools) ExecuteHarnessTool(ctx context.Context, p HarnessPrincipal, name string, call HarnessToolCall) (HarnessToolOutcome, error) {
	run, err := s.lifecycle().activeRun(ctx, p)
	if err != nil {
		return HarnessToolOutcome{}, err
	}
	if run.CancelRequested {
		// §5.2: cancellation guarantees no NEW tool call happens. The host is on its way
		// out; say so instead of doing work it will discard.
		return HarnessToolOutcome{Status: "error", IsError: true, Error: "run cancelled"}, nil
	}
	toolCallID := strings.TrimSpace(call.ToolCallID)
	args := call.Input
	if args == nil {
		args = map[string]any{}
	}

	sess, err := s.lifecycle().loadRunSession(ctx, run)
	if err != nil {
		return HarnessToolOutcome{}, err
	}
	if sess == nil {
		return HarnessToolOutcome{}, errors.New("run has no assistant message to record the tool call on")
	}

	tool, ok := s.svc.tools.Get(name)
	if !ok {
		return s.recordToolFailure(ctx, sess, run, toolCallID, name, args, toolError("unknown tool %q", name)), nil
	}
	var correlated *persistence.AgentMessagePart
	if toolCallID == "" {
		// A libfx HostTool is called with its input and an abort signal, and no call id, so a host has
		// nothing to echo back (§4.4.1). The call is matched to the part the model proxy already wrote
		// for it; refusing here instead would fail every tool call a browser or sidecar host makes.
		part, err := s.correlateToolCall(ctx, run, sess, name)
		if err != nil {
			return HarnessToolOutcome{}, err
		}
		if part == nil {
			// Nothing is waiting for a result under that name. Saying so reaches the model as a tool
			// error, which is a recoverable turn; inventing a part would put a call in the transcript
			// that no model ever made.
			return s.recordToolFailure(ctx, sess, run, "", name, args,
				toolError("no pending %q call in this run", name)), nil
		}
		correlated = part
		toolCallID = derefString(part.ToolCallID)
	}
	if problems := s.svc.tools.ValidateArgs(name, args); len(problems) > 0 {
		return s.recordToolFailure(ctx, sess, run, toolCallID, name, args,
			ToolResult{IsError: true, Text: "invalid arguments: " + strings.Join(problems, "; ")}), nil
	}

	thread, err := s.repo().GetThread(ctx, p.UserID, p.ThreadID)
	if err != nil {
		return HarnessToolOutcome{}, err
	}
	tc := ToolContext{UserID: p.UserID, ThreadID: p.ThreadID, RunID: p.RunID, ActiveGoalID: derefString(thread.GoalID)}

	part := correlated
	if part == nil {
		part, err = s.ensureToolCallPart(ctx, sess, run, toolCallID, name, args)
		if err != nil {
			return HarnessToolOutcome{}, err
		}
	}

	// Both levels execute OUTSIDE any business transaction with the §6 per-call timeout.
	// A proposal tool writes only the staging record, never a business table (agent.md §4
	// invariant 1). Waiting for the decision happens after this returns and is not bounded
	// by the tool timeout (§7).
	toolCtx, cancel := context.WithTimeout(ctx, s.toolTimeout())
	defer cancel()
	result, execErr := tool.Execute(toolCtx, tc, args)
	if execErr != nil {
		if toolCtx.Err() == context.DeadlineExceeded {
			result = toolError("tool %q timed out after %s", name, s.toolTimeout())
		} else {
			result = ToolResult{IsError: true, Text: fmt.Sprintf("tool %q failed: %v", name, execErr)}
		}
	}

	if result.Proposal != nil && !result.IsError {
		return s.parkForApproval(ctx, sess, run, part, result)
	}
	return s.recordToolResult(ctx, sess, part, result), nil
}

// toolTimeout is the per-call budget (§6), defaulted when unset so a service built without
// options still behaves.
func (s *harnessTools) toolTimeout() time.Duration {
	if s.svc.limits.ToolTimeout > 0 {
		return s.svc.limits.ToolTimeout
	}
	return DefaultLoopLimits().ToolTimeout
}

// correlateToolCall finds the part an id-less tool call belongs to (§4.4.1), or nil when there is none.
//
// The name is enough because a host runs same-name calls sequentially (web/src/harness/tools.ts), so at
// most one part of that name is waiting for a result at a time. The wait is short and bounded: the proxy
// writes the part while it reads the model's stream, and a host cannot know about the call until that
// stream reached it, so a miss means the model never made one rather than that the row is still on its way.
func (s *harnessTools) correlateToolCall(ctx context.Context, run *persistence.AgentRun, sess *runSession, name string) (*persistence.AgentMessagePart, error) {
	for attempt := 0; attempt < 5; attempt++ {
		part, err := s.repo().LatestUnfinishedToolPart(ctx, run.UserID, sess.assistantID, name)
		if err == nil {
			return part, nil
		}
		if !persistence.IsNotFound(err) {
			return nil, err
		}
		if attempt == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, nil
}

// ensureToolCallPart returns the part carrying this tool call, creating it when the model
// proxy has not written it yet. Creation claims the next part index inside a transaction
// so the proxy and this path cannot both take the same one (§2.7.1: the server is the sole
// index owner).
func (s *harnessTools) ensureToolCallPart(ctx context.Context, sess *runSession, run *persistence.AgentRun, toolCallID, name string, args map[string]any) (*persistence.AgentMessagePart, error) {
	if toolCallID != "" {
		existing, err := s.repo().GetPartByToolCallID(ctx, run.UserID, toolCallID)
		if err == nil {
			if existing.MessageID != sess.assistantID {
				// Another run's part carries this id. Writing a result onto it would put this run's tool
				// output in somebody else's transcript, and a second row is blocked by the unique index.
				return nil, fmt.Errorf("%w: %s", ErrToolCallIDCollision, toolCallID)
			}
			s.syncPartIntoState(sess, existing)
			return existing, nil
		}
		if !persistence.IsNotFound(err) {
			return nil, err
		}
	}
	part := persistence.AgentMessagePart{
		UserID: run.UserID, MessageID: sess.assistantID, Type: "tool-call", ToolName: name,
	}
	if toolCallID != "" {
		callID := toolCallID
		part.ToolCallID = &callID
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	part.ArgsJSON = string(encoded)

	err = s.svc.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo().WithTx(tx)
		idx, err := repo.NextPartIdx(ctx, run.UserID, sess.assistantID)
		if err != nil {
			return err
		}
		part.Idx = idx
		if err := repo.CreatePartAtIdx(ctx, &part); err != nil {
			if persistence.IsUniqueViolation(err) && toolCallID != "" {
				// The proxy won the race for this run's part; use its row rather than duplicating the call.
				found, lookupErr := repo.GetPartByToolCallID(ctx, run.UserID, toolCallID)
				if lookupErr != nil {
					return lookupErr
				}
				if found.MessageID != sess.assistantID {
					return fmt.Errorf("%w: %s", ErrToolCallIDCollision, toolCallID)
				}
				part = *found
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.syncPartIntoState(sess, &part)
	return &part, nil
}

// syncPartIntoState makes the in-memory session state agree with a part that came from the
// database, so the chunk emitted next carries the whole part instead of a stale copy.
func (s *harnessTools) syncPartIntoState(sess *runSession, part *persistence.AgentMessagePart) {
	if sess.assistantID == "" || part.MessageID != sess.assistantID {
		return
	}
	parts := &sess.state.Messages[sess.assistantIdx].Parts
	for len(*parts) <= part.Idx {
		*parts = append(*parts, protocol.TextPart(""))
	}
	(*parts)[part.Idx] = partFromRow(part)
}

// partFromRow renders a persisted part as its wire shape.
func partFromRow(part *persistence.AgentMessagePart) protocol.Part {
	switch part.Type {
	case "tool-call":
		wire := protocol.ToolCallPart(derefString(part.ToolCallID), part.ToolName, decodeArgsMap(part.ArgsJSON))
		if strings.TrimSpace(part.ResultJSON) != "" {
			var result any
			if err := json.Unmarshal([]byte(part.ResultJSON), &result); err == nil {
				wire.Result = result
			}
		}
		wire.IsError = part.IsError
		wire.Approval = approvalFromStatus(part.ApprovalStatus)
		return wire
	case "reasoning":
		return protocol.ReasoningPart(part.ID, part.Text)
	case "image":
		return protocol.ImagePart(part.Text)
	default:
		return protocol.TextPart(part.Text)
	}
}

// recordToolResult writes a finished tool result onto its part and pushes the update to the
// client (§4.4.1: the execution surface owns the result, never the call).
func (s *harnessTools) recordToolResult(ctx context.Context, sess *runSession, part *persistence.AgentMessagePart, result ToolResult) HarnessToolOutcome {
	resultJSON, isError := resultPayload(result)
	if err := s.repo().UpdatePartResult(ctx, part.UserID, part.ID, resultJSON, isError); err != nil {
		return HarnessToolOutcome{Status: "error", IsError: true, Error: "failed to record the tool result"}
	}
	part.ResultJSON, part.IsError = resultJSON, isError
	s.emitPartResult(sess, part, result, "")
	if isError {
		message := strings.TrimSpace(result.Text)
		if message == "" {
			message = resultContent(result)
		}
		return HarnessToolOutcome{Status: "error", IsError: true, Error: message}
	}
	return HarnessToolOutcome{Status: "ok", Result: result.Result}
}

// recordToolFailure is the structured-error path: the model gets something it can correct
// itself on and the run keeps going (§5 item 6).
func (s *harnessTools) recordToolFailure(ctx context.Context, sess *runSession, run *persistence.AgentRun, toolCallID, name string, args map[string]any, result ToolResult) HarnessToolOutcome {
	part, err := s.ensureToolCallPart(ctx, sess, run, toolCallID, name, args)
	if err != nil {
		return HarnessToolOutcome{Status: "error", IsError: true, Error: result.Text}
	}
	return s.recordToolResult(ctx, sess, part, result)
}

// emitPartResult mirrors a recorded result into the session state and emits the part, so a
// tool output or an approval card appears without a reload.
func (s *harnessTools) emitPartResult(sess *runSession, part *persistence.AgentMessagePart, result ToolResult, approval string) {
	idx := part.Idx
	parts := sess.assistantParts()
	if idx < 0 || idx >= len(parts) {
		return
	}
	updated := partFromRow(part)
	if approval != "" {
		updated.Approval = &protocol.Approval{Status: approval}
	}
	sess.state.Messages[sess.assistantIdx].Parts[idx] = updated
	_ = sess.emitSet(protocol.PartPath(sess.assistantIdx, idx), updated)
	if approval == protocol.ApprovalPending && result.Proposal != nil {
		sess.state.FastTask.PendingProposals = append(sess.state.FastTask.PendingProposals, protocol.PendingProposal{
			ID: result.Proposal.ProposalID, GoalID: result.Proposal.GoalID,
			BaseRevision: result.Proposal.BaseRevision, Summary: result.Proposal.Summary,
		})
		_ = sess.emitSet(protocol.FastTaskPath(), sess.state.FastTask)
	}
}

// parkForApproval is the §7 half of a proposal tool: the Proposal is already staged and no
// business table was touched, so all that remains is to show the user the decision, park the
// run, and hand the host a "pending" outcome that keeps its execute() open.
func (s *harnessTools) parkForApproval(ctx context.Context, sess *runSession, run *persistence.AgentRun, part *persistence.AgentMessagePart, result ToolResult) (HarnessToolOutcome, error) {
	proposalID := result.Proposal.ProposalID
	resultJSON, isError := resultPayload(result)
	if err := s.repo().UpdatePartResult(ctx, part.UserID, part.ID, resultJSON, isError); err != nil {
		return HarnessToolOutcome{}, err
	}
	if err := s.repo().UpdatePartApproval(ctx, part.UserID, part.ID, protocol.ApprovalPending, &proposalID); err != nil {
		return HarnessToolOutcome{}, err
	}
	part.ResultJSON, part.IsError, part.ApprovalStatus = resultJSON, isError, protocol.ApprovalPending
	part.ProposalID = &proposalID
	s.emitPartResult(sess, part, result, protocol.ApprovalPending)

	// The message needs a user decision before it can continue (§2.7).
	requires := protocol.RequiresActionStatus()
	sess.state.Messages[sess.assistantIdx].Status = requires
	_ = sess.emitSet(protocol.MessageStatusPath(sess.assistantIdx), requires)
	if err := sess.saveState(); err != nil {
		return HarnessToolOutcome{}, err
	}
	if err := s.repo().SetRunStatus(ctx, run.UserID, run.ID, persistence.RunAwaitingApproval, "", ""); err != nil {
		return HarnessToolOutcome{}, err
	}
	// The run is parked and the host is about to block on a long poll. A user who closed
	// the tab has no other way to learn a decision is waiting (§7, doc/notification.md §10).
	notifyProposalPending(ctx, s.svc.app, run, result.Proposal)
	return HarnessToolOutcome{Status: "pending", ProposalID: proposalID, Result: result.Result}, nil
}

// --- approval long poll (§7) ---

// WaitForApproval holds a host's tool call open until the user decides. It is a long poll
// rather than a page-local event because two kinds of host share the code and a sidecar has
// no page (§7).
//
// Four things end a wait: a decision, a cancellation (§5.2 channel 2), a run that is no
// longer drivable, and the segment timer — which returns "pending" so an intermediate proxy
// cannot cut a 15 minute wait in half. The total wait is measured across segments on the run
// row; exceeding it fails the run with APPROVAL_TIMEOUT (§7).
func (s *harnessTools) WaitForApproval(ctx context.Context, p HarnessPrincipal, proposalID string, segment time.Duration) (ApprovalWait, error) {
	maxSegment, waitLimit := s.svc.approvalWait()
	if segment <= 0 || segment > maxSegment {
		segment = maxSegment
	}
	if _, err := s.lifecycle().activeRun(ctx, p); err != nil {
		return ApprovalWait{}, err
	}
	part, err := s.repo().GetPartByProposalID(ctx, p.UserID, proposalID)
	if err != nil {
		return ApprovalWait{}, err
	}
	deadline := s.clock().Add(segment)
	started := s.clock()
	for {
		if decision, done, err := s.readApprovalDecision(ctx, p, part.ID, proposalID); err != nil {
			return ApprovalWait{}, err
		} else if done {
			s.recordApprovalWait(ctx, p, started)
			return decision, nil
		}

		current, err := s.repo().GetRun(ctx, p.UserID, p.RunID)
		if err != nil {
			return ApprovalWait{}, err
		}
		if current.CancelRequested || current.Status == persistence.RunCancelling {
			s.recordApprovalWait(ctx, p, started)
			return ApprovalWait{Status: "cancelled", ProposalID: proposalID}, nil
		}
		if IsTerminalRunStatus(current.Status) {
			s.recordApprovalWait(ctx, p, started)
			return ApprovalWait{Status: current.Status, ProposalID: proposalID}, nil
		}

		now := s.clock()
		if !now.Before(deadline) {
			waited := s.recordApprovalWait(ctx, p, started)
			total := time.Duration(current.ApprovalWaitMs)*time.Millisecond + waited
			if total >= waitLimit {
				// The user never decided. The run cannot stay parked forever (§7): it fails
				// with a code the model is not blamed for, and the proposal stays pending so
				// the user can still see what was asked.
				if err := s.timeoutApproval(ctx, current, part, proposalID); err != nil {
					return ApprovalWait{}, err
				}
				return ApprovalWait{
					Status: "timeout", ProposalID: proposalID, IsError: true,
					Result:        map[string]any{"error": "APPROVAL_TIMEOUT: no decision within the wait limit"},
					WaitedSeconds: int(total.Seconds()),
				}, nil
			}
			return ApprovalWait{
				Status: "pending", ProposalID: proposalID,
				WaitedSeconds: int(total.Seconds()),
			}, nil
		}
		select {
		case <-ctx.Done():
			// The host went away mid-wait. The proposal stays pending: the run is left to the
			// heartbeat reaper and the user can still decide later (§7, §11).
			s.recordApprovalWait(ctx, p, started)
			return ApprovalWait{}, ctx.Err()
		case <-time.After(approvalPollInterval):
		}
	}
}

// timeoutApproval fails a run whose approval wait outlived the limit (§7).
func (s *harnessTools) timeoutApproval(ctx context.Context, run *persistence.AgentRun, part *persistence.AgentMessagePart, proposalID string) error {
	payload, _ := json.Marshal(map[string]any{"error": "APPROVAL_TIMEOUT", "proposal_id": proposalID})
	if err := s.repo().UpdatePartResult(ctx, part.UserID, part.ID, string(payload), true); err != nil {
		return err
	}
	if err := s.repo().UpdatePartApproval(ctx, part.UserID, part.ID, "timeout", nil); err != nil {
		return err
	}
	return s.lifecycle().finishRun(ctx, run, persistence.RunFailed, "APPROVAL_TIMEOUT", "approval wait exceeded the limit", protocol.ReasonError)
}

// readApprovalDecision reports whether the user has decided. A conflict counts as a
// decision: the model must learn the tree moved instead of waiting for one that will never
// come (§7.4).
func (s *harnessTools) readApprovalDecision(ctx context.Context, p HarnessPrincipal, partID, proposalID string) (ApprovalWait, bool, error) {
	part, err := s.repo().GetPart(ctx, p.UserID, partID)
	if err != nil {
		return ApprovalWait{}, false, err
	}
	outcome := ApprovalWait{ProposalID: proposalID}
	var result any
	if strings.TrimSpace(part.ResultJSON) != "" {
		_ = json.Unmarshal([]byte(part.ResultJSON), &result)
	}
	switch part.ApprovalStatus {
	case protocol.ApprovalApproved:
		outcome.Status, outcome.Result = "approved", result
		return outcome, true, nil
	case protocol.ApprovalRejected:
		outcome.Status, outcome.Result = "rejected", result
		return outcome, true, nil
	case "conflict", "timeout":
		outcome.Status, outcome.Result, outcome.IsError = part.ApprovalStatus, result, true
		return outcome, true, nil
	}
	// The part is still pending, but the proposal itself may have been marked conflict by a
	// failed apply (§7.4). That is a decision the host must see.
	var proposal persistence.Proposal
	err = s.svc.store.DB.WithContext(ctx).
		Where("id = ? AND user_id = ?", proposalID, p.UserID).First(&proposal).Error
	if err != nil && !persistence.IsNotFound(err) {
		return ApprovalWait{}, false, err
	}
	if err == nil && proposal.Status == "conflict" {
		outcome.Status, outcome.IsError = "conflict", true
		outcome.Result = map[string]any{"error": "the task tree changed before this approval; the proposal was marked conflict"}
		return outcome, true, nil
	}
	return outcome, false, nil
}

// recordApprovalWait adds the time just spent waiting to the run's excluded total, so the
// wall clock the model proxy enforces does not include time spent waiting for a human (§7).
// It returns the wait it recorded.
func (s *harnessTools) recordApprovalWait(ctx context.Context, p HarnessPrincipal, started time.Time) time.Duration {
	waited := s.clock().Sub(started)
	if waited <= 0 {
		return 0
	}
	if err := s.repo().AddApprovalWait(ctx, p.UserID, p.RunID, waited); err != nil {
		return waited
	}
	return waited
}
