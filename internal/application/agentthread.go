package application

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// threadStateService turns persisted agent rows back into wire state
// (doc/agent-impl.md §2.7): rebuilding one message from its parts, restoring a
// user's latest conversation for a chat view that was unmounted, and resolving
// the in-flight run behind a client that can only present assistant-ui's
// temporary thread id. It is split out of AgentService and embedded back, so the
// promoted methods keep their existing call sites (wiring.md §9: no struct in
// application declares more than 15 methods).
type threadStateService struct {
	repo *persistence.AgentRepository
	// svc reaches the pieces a rebuilt state needs beyond the rows: the pending-proposal list that the
	// approval cards render from.
	svc *AgentService
}

// toProtocolMessage rebuilds a wire message from persisted rows.
func (s *threadStateService) toProtocolMessage(ctx context.Context, userID string, m persistence.AgentMessage, status protocol.MessageStatus) (protocol.Message, error) {
	parts, err := s.repo.ListMessageParts(ctx, userID, m.ID)
	if err != nil {
		return protocol.Message{}, err
	}
	wire := protocol.Message{
		ID: m.ID, Role: protocol.Role(m.Role), Parts: []protocol.Part{},
		CreatedAt: protocol.RFC3339(m.CreatedAt), Status: status,
	}
	for _, p := range parts {
		switch p.Type {
		case "reasoning":
			wire.Parts = append(wire.Parts, protocol.ReasoningPart(p.ID, p.Text))
		case "image":
			// A reference, never bytes: the client renders it through the owner-scoped attachment
			// endpoint (doc/chat-features.md §4.4).
			wire.Parts = append(wire.Parts, protocol.ImagePart(p.Text))
		case "tool-call":
			tp := protocol.ToolCallPart(derefString(p.ToolCallID), p.ToolName, decodeArgsMap(p.ArgsJSON))
			if p.ResultJSON != "" {
				var result any
				if err := json.Unmarshal([]byte(p.ResultJSON), &result); err == nil {
					tp.Result = result
				}
			}
			tp.IsError = p.IsError
			tp.Approval = approvalFromStatus(p.ApprovalStatus)
			wire.Parts = append(wire.Parts, tp)
		default:
			wire.Parts = append(wire.Parts, protocol.TextPart(p.Text))
		}
	}
	return wire, nil
}

// LatestThreadState loads the most recently active agent conversation for a
// user. The browser can mount the chat again without treating an empty local
// runtime as a new conversation. A running run may not have saved a snapshot
// yet, so its persisted messages are the fallback.
func (s *threadStateService) LatestThreadState(ctx context.Context, userID string) (protocol.State, bool, error) {
	run, err := s.repo.LatestRun(ctx, userID)
	if err != nil {
		if persistence.IsNotFound(err) {
			return protocol.State{}, false, nil
		}
		return protocol.State{}, false, err
	}

	// The retained snapshot is authoritative when it holds messages; a run that
	// has not checkpointed yet (or one written before this field existed) falls
	// back to the persisted thread, which is the same source §2.6 renders from.
	state := protocol.NewState()
	if err := json.Unmarshal([]byte(run.StateJSON), &state); err != nil || len(state.Messages) == 0 {
		state = protocol.NewState()
		messages, err := s.repo.ListThreadMessages(ctx, userID, run.ThreadID)
		if err != nil {
			return protocol.State{}, false, err
		}
		for _, message := range messages {
			status := protocol.CompleteStatus("")
			if message.Role == string(protocol.RoleAssistant) && message.RunID == run.ID {
				switch run.Status {
				case persistence.RunQueued, persistence.RunRunning:
					status = protocol.RunningStatus()
				case persistence.RunAwaitingApproval:
					status = protocol.RequiresActionStatus()
				case persistence.RunFailed, persistence.RunCancelled, persistence.RunInterrupted:
					status = protocol.IncompleteStatus(protocol.ReasonError)
				}
			}
			wire, err := s.toProtocolMessage(ctx, userID, message, status)
			if err != nil {
				return protocol.State{}, false, err
			}
			state.Messages = append(state.Messages, wire)
		}
	}
	// threadId always comes from the row, not from the snapshot: it is what lets
	// the client's next command continue this thread instead of opening one (§2.2).
	state.FastTask.ThreadID = run.ThreadID
	state.IsRunning = run.Status == persistence.RunQueued || run.Status == persistence.RunRunning
	return state, true, nil
}

// ThreadState loads one thread's authoritative state — the same shape §2.6 renders from — for a client
// that named the thread it wants (doc/chat-features.md §2).
//
// It is rebuilt from the persisted parts rather than read from a run snapshot, because a thread outlives
// any run: switching conversations, reloading, or signing in on a second device all land here with no run
// in flight. When a run IS in flight, its status decides how the last assistant message is marked, so a
// client that switches into a live thread sees the same thing a client that never left would.
func (s *threadStateService) ThreadState(ctx context.Context, userID, threadID string) (protocol.State, error) {
	thread, err := s.repo.GetThread(ctx, userID, threadID)
	if err != nil {
		return protocol.State{}, err
	}
	state := protocol.NewState()
	// threadId comes from the row, not from a snapshot: it is what lets the client's next command continue
	// this thread instead of opening another one (§2.2).
	state.FastTask.ThreadID = thread.ID
	state.FastTask.ActiveGoalID = derefString(thread.GoalID)
	state.FastTask.PendingProposals = s.svc.pendingProposals(ctx, userID, threadID)

	run, runErr := s.repo.GetActiveRunByThread(ctx, userID, threadID)
	if runErr != nil && !persistence.IsNotFound(runErr) {
		return protocol.State{}, runErr
	}
	if runErr == nil {
		state.FastTask.RunID = run.ID
		state.IsRunning = run.Status == persistence.RunQueued || run.Status == persistence.RunRunning ||
			run.Status == persistence.RunCancelling
		if strings.TrimSpace(run.StateJSON) != "" && run.StateJSON != "{}" {
			// A run that has already checkpointed its state carries the exact snapshot its client saw,
			// including the part indices an append-text depends on.
			var snapshot protocol.State
			if err := json.Unmarshal([]byte(run.StateJSON), &snapshot); err == nil && len(snapshot.Messages) > 0 {
				snapshot.FastTask = state.FastTask
				snapshot.IsRunning = state.IsRunning
				return snapshot, nil
			}
		}
	}

	messages, err := s.repo.ListThreadMessages(ctx, userID, threadID)
	if err != nil {
		return protocol.State{}, err
	}
	for _, message := range messages {
		status := protocol.CompleteStatus("")
		if runErr == nil && message.Role == string(protocol.RoleAssistant) && message.RunID == run.ID {
			switch run.Status {
			case persistence.RunQueued, persistence.RunRunning, persistence.RunCancelling:
				status = protocol.RunningStatus()
			case persistence.RunAwaitingApproval:
				status = protocol.RequiresActionStatus()
			}
		}
		wire, err := s.toProtocolMessage(ctx, userID, message, status)
		if err != nil {
			return protocol.State{}, err
		}
		state.Messages = append(state.Messages, wire)
	}
	return state, nil
}

// ResumeCurrentState resolves a browser's temporary local thread ID to the
// authenticated user's single active run. This is only used for the library's
// __LOCALID_... identity; normal thread IDs still receive ownership checks.
func (s *threadStateService) ResumeCurrentState(ctx context.Context, userID string) (ResumeStateResult, bool, error) {
	run, err := s.repo.GetActiveRunForUser(ctx, userID)
	if err != nil {
		if persistence.IsNotFound(err) {
			return ResumeStateResult{}, false, nil
		}
		return ResumeStateResult{}, false, err
	}
	stateJSON := run.StateJSON
	if strings.TrimSpace(stateJSON) == "" {
		stateJSON = "{}"
	}
	return ResumeStateResult{State: json.RawMessage(stateJSON), RunID: run.ID}, true, nil
}
