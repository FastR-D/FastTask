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
// tool_call_id is the actual id observed by the proxy and host shim. It remains
// optional in the wire schema for compatibility, but an unidentified execution
// is refused without changing the transcript. libfx does not supply it to
// execute(); the shim must carry its verified execution envelope instead.
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
//     recorded twice. The host must carry the actual call id; a missing or
//     ambiguous identity is refused without inventing transcript records.
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

	if toolCallID == "" {
		return identityFailure("TOOL_CALL_ID_REQUIRED: refresh the FastTask host; no tool was executed"), nil
	}
	part, err := s.verifiedToolCall(ctx, run, sess, toolCallID, name, args)
	if err != nil {
		return identityFailure(err.Error()), nil
	}
	if part.ResultJSON != "" {
		return replayToolOutcome(part), nil
	}
	// Claim only a finalized call from the current model response. Retrying the
	// same id cannot consume another call, including an identical-argument call.
	claimed, err := s.repo().ClaimToolCall(ctx, run.UserID, run.ID, sess.assistantID, part.ID, part.ToolModelTurn)
	if err != nil {
		return HarnessToolOutcome{}, err
	}
	if !claimed {
		latest, readErr := s.repo().GetPart(ctx, run.UserID, part.ID)
		if readErr == nil && latest.ResultJSON != "" {
			return replayToolOutcome(latest), nil
		}
		return identityFailure("TOOL_EXECUTION_UNCONFIRMED: this call is already executing or its model turn is no longer current; reconcile its result before retrying"), nil
	}
	s.syncPartIntoState(sess, part)
	tool, ok := s.svc.tools.Get(name)
	if !ok {
		return s.recordToolResult(ctx, sess, part, toolError("unknown tool %q", name)), nil
	}
	if problems := s.svc.tools.ValidateArgs(name, args); len(problems) > 0 {
		return s.recordToolResult(ctx, sess, part, ToolResult{IsError: true, Text: "invalid arguments: " + strings.Join(problems, "; ")}), nil
	}
	thread, err := s.repo().GetThread(ctx, p.UserID, p.ThreadID)
	if err != nil {
		return HarnessToolOutcome{}, err
	}
	tc := ToolContext{UserID: p.UserID, ThreadID: p.ThreadID, RunID: p.RunID, ActiveGoalID: derefString(thread.GoalID)}

	// Both levels execute OUTSIDE any business transaction with the §6 per-call timeout.
	// A proposal tool writes only the staging record, never a business table (agent.md §4
	// invariant 1). Waiting for the decision happens after this returns and is not bounded
	// by the tool timeout (§7).
	budget := s.executionTimeout(tool)
	toolCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	result, execErr := tool.Execute(toolCtx, tc, args)
	if execErr != nil {
		if toolCtx.Err() == context.DeadlineExceeded {
			result = toolError("tool %q timed out after %s", name, budget)
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

func (s *harnessTools) executionTimeout(tool Tool) time.Duration {
	budget := s.toolTimeout()
	// Explicit operator/test bounds win. Only locally registered tools can
	// request a longer bounded budget; model input cannot increase it.
	if declared, ok := tool.(ToolExecutionBudget); ok && !s.svc.toolTimeoutConfigured {
		if requested := declared.ExecutionBudget(); requested > budget && requested <= 45*time.Second {
			return requested
		}
	}
	return budget
}

func identityFailure(message string) HarnessToolOutcome {
	return HarnessToolOutcome{Status: "error", IsError: true, Error: message}
}

func sameToolArgs(raw string, args map[string]any) bool {
	var decoded map[string]any
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return false
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	a, errA := json.Marshal(decoded)
	b, errB := json.Marshal(args)
	return errA == nil && errB == nil && string(a) == string(b)
}

// A bounded wait handles the proxy finishing persistence after the last streamed
// byte. No path invents a call or chooses one by name/argument similarity.
func (s *harnessTools) verifiedToolCall(ctx context.Context, run *persistence.AgentRun, sess *runSession, id, name string, args map[string]any) (*persistence.AgentMessagePart, error) {
	for attempt := 0; attempt < 10; attempt++ {
		part, err := s.repo().GetPartByToolCallID(ctx, run.UserID, id)
		if err != nil && !persistence.IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			if part.MessageID != sess.assistantID || part.Type != "tool-call" || part.ToolName != name {
				return nil, fmt.Errorf("TOOL_CALL_IDENTITY_MISMATCH: call does not belong to this run and tool")
			}
			if part.ToolArgsReady || part.ResultJSON != "" {
				if !sameToolArgs(part.ArgsJSON, args) {
					return nil, fmt.Errorf("TOOL_CALL_ARGUMENT_MISMATCH: full input differs from the model call")
				}
				return part, nil
			}
		}
		if attempt < 9 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("TOOL_CALL_NOT_READY: no finalized %q call with this identity; no tool was executed", name)
}

func replayToolOutcome(part *persistence.AgentMessagePart) HarnessToolOutcome {
	var value any
	if json.Unmarshal([]byte(part.ResultJSON), &value) != nil {
		return identityFailure("TOOL_RESULT_UNREADABLE: execution will not be repeated")
	}
	if part.IsError {
		message := part.ResultJSON
		if result, ok := value.(map[string]any); ok {
			if detail, ok := result["error"].(string); ok {
				message = detail
			}
		}
		return identityFailure(message)
	}
	if part.ProposalID != nil && part.ApprovalStatus == protocol.ApprovalPending {
		return HarnessToolOutcome{Status: "pending", ProposalID: *part.ProposalID, Result: value}
	}
	return HarnessToolOutcome{Status: "ok", Result: value}
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
	if err := s.commitPendingApproval(ctx, sess, run, part, result); err != nil {
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
	message, err := s.repo().GetMessage(ctx, p.UserID, part.MessageID)
	if err != nil || message.RunID != p.RunID {
		return ApprovalWait{}, ErrHarnessToken
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
			total := waited
			if current.ApprovalStartedAt != nil {
				total = s.clock().Sub(*current.ApprovalStartedAt)
			}
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
	// Waiting belongs to the run state interval, not to overlapping HTTP polls.
	return 0
}
