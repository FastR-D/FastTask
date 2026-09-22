package protocol

import (
	"encoding/json"
	"strconv"
	"time"
)

// State is the authoritative server-side thread state (doc/agent-impl.md §2.7).
// It is FastTask's own structure, NOT an assistant-ui type: the frontend
// converter maps it to ThreadMessage. The client accumulates update-state
// operations into this shape and renders from it (§2.6), so every field the
// converter reads must round-trip exactly.
type State struct {
	Messages  []Message     `json:"messages"`
	IsRunning bool          `json:"isRunning"`
	FastTask  FastTaskState `json:"fasttask"`
}

// NewState returns an empty, valid state. Messages and the proposal list are
// non-nil empty slices so they serialize as [] rather than null — the client
// decoder expects arrays.
func NewState() State {
	return State{
		Messages:  []Message{},
		IsRunning: false,
		FastTask:  FastTaskState{PendingProposals: []PendingProposal{}},
	}
}

// MarshalJSON guarantees `messages` is always an array, never null.
func (s State) MarshalJSON() ([]byte, error) {
	messages := s.Messages
	if messages == nil {
		messages = []Message{}
	}
	fasttask := s.FastTask
	if fasttask.PendingProposals == nil {
		fasttask.PendingProposals = []PendingProposal{}
	}
	type wire struct {
		Messages  []Message     `json:"messages"`
		IsRunning bool          `json:"isRunning"`
		FastTask  FastTaskState `json:"fasttask"`
	}
	return json.Marshal(wire{Messages: messages, IsRunning: s.IsRunning, FastTask: fasttask})
}

// Role is a message author (§2.7).
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one thread message. Parts is always an array on the wire.
type Message struct {
	ID        string        `json:"id"`
	Role      Role          `json:"role"`
	Parts     []Part        `json:"parts"`
	CreatedAt string        `json:"createdAt"`
	Status    MessageStatus `json:"status"`
}

// MarshalJSON guarantees `parts` is always an array, never null.
func (m Message) MarshalJSON() ([]byte, error) {
	parts := m.Parts
	if parts == nil {
		parts = []Part{}
	}
	type wire struct {
		ID        string        `json:"id"`
		Role      Role          `json:"role"`
		Parts     []Part        `json:"parts"`
		CreatedAt string        `json:"createdAt"`
		Status    MessageStatus `json:"status"`
	}
	return json.Marshal(wire{ID: m.ID, Role: m.Role, Parts: parts, CreatedAt: m.CreatedAt, Status: m.Status})
}

// RFC3339 formats a timestamp for the createdAt field (§2.7).
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// MessageStatus mirrors assistant-ui's MessageStatus so the converter can pass it
// through unchanged (§2.7). Reason is omitted when empty so a running status is
// exactly {"type":"running"}.
type MessageStatus struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

const (
	StatusRunning        = "running"
	StatusComplete       = "complete"
	StatusIncomplete     = "incomplete"
	StatusRequiresAction = "requires-action"

	ReasonStop       = "stop"
	ReasonCancelled  = "cancelled"
	ReasonError      = "error"
	ReasonToolCalls  = "tool-calls"
	ReasonMaxTurns   = "max-turns"
	ReasonRunTimeout = "run-timeout"
)

// RunningStatus is the status of a message still being generated.
func RunningStatus() MessageStatus { return MessageStatus{Type: StatusRunning} }

// CompleteStatus marks a message finished for the given reason (usually "stop").
func CompleteStatus(reason string) MessageStatus {
	return MessageStatus{Type: StatusComplete, Reason: reason}
}

// IncompleteStatus marks a message aborted (cancelled/error/...).
func IncompleteStatus(reason string) MessageStatus {
	return MessageStatus{Type: StatusIncomplete, Reason: reason}
}

// RequiresActionStatus marks a message waiting on a tool approval (§2.7).
func RequiresActionStatus() MessageStatus {
	return MessageStatus{Type: StatusRequiresAction, Reason: ReasonToolCalls}
}

// PartType discriminates the two message part shapes (§2.7).
type PartType string

const (
	PartText     PartType = "text"
	PartToolCall PartType = "tool-call"
	// PartReasoning carries a model's chain of thought (doc/chat-features.md §3.3).
	// It is display-only: nothing may parse or act on its text (agent.md §4
	// invariant 4). ID separates the multiple reasoning segments one message can
	// hold, so they are never concatenated into a single blob.
	PartReasoning PartType = "reasoning"
)

// Part is a message part. It is polymorphic on the wire: a text part is exactly
// {"type":"text","text":"..."} — the text field is ALWAYS present, even when
// empty, because append-text requires an existing string target (§2.6, §2.7.1).
// A tool-call part carries the call, its result and the FastTask-owned approval.
type Part struct {
	Type PartType `json:"type"`

	// Text part.
	Text string `json:"-"`

	// Reasoning part identity (§3.3). Empty for every other part type.
	ID string `json:"-"`

	// Tool-call part.
	ToolCallID string         `json:"-"`
	ToolName   string         `json:"-"`
	Args       map[string]any `json:"-"`
	Result     any            `json:"-"`
	IsError    bool           `json:"-"`
	// Approval is FastTask-owned. The converter must NOT map it to
	// ToolCallMessagePart.approval (ADR-0002 §3.1); it drives makeAssistantToolUI.
	Approval *Approval `json:"-"`
}

// TextPart builds a text part. An empty text is valid and required as the
// append-text target before streaming begins (§2.7.1).
func TextPart(text string) Part { return Part{Type: PartText, Text: text} }

// ReasoningPart builds a reasoning part. An empty text is valid: like a text part
// it is the append-text target established before the deltas arrive.
func ReasoningPart(id, text string) Part {
	return Part{Type: PartReasoning, ID: id, Text: text}
}

// ToolCallPart builds a tool-call part with the given arguments.
func ToolCallPart(toolCallID, toolName string, args map[string]any) Part {
	if args == nil {
		args = map[string]any{}
	}
	return Part{Type: PartToolCall, ToolCallID: toolCallID, ToolName: toolName, Args: args}
}

type toolCallWire struct {
	Type       PartType       `json:"type"`
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Args       map[string]any `json:"args"`
	Result     any            `json:"result"`
	IsError    bool           `json:"isError"`
	Approval   *Approval      `json:"approval,omitempty"`
}

// MarshalJSON emits only the fields relevant to the part type. A text part
// always includes "text"; a tool-call part always includes args/result/isError.
func (p Part) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case PartText:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			Text string   `json:"text"`
		}{Type: p.Type, Text: p.Text})
	case PartReasoning:
		return json.Marshal(struct {
			Type PartType `json:"type"`
			ID   string   `json:"id,omitempty"`
			Text string   `json:"text"`
		}{Type: p.Type, ID: p.ID, Text: p.Text})
	case PartToolCall:
		args := p.Args
		if args == nil {
			args = map[string]any{}
		}
		return json.Marshal(toolCallWire{
			Type: p.Type, ToolCallID: p.ToolCallID, ToolName: p.ToolName,
			Args: args, Result: p.Result, IsError: p.IsError, Approval: p.Approval,
		})
	default:
		return json.Marshal(struct {
			Type PartType `json:"type"`
		}{Type: p.Type})
	}
}

// UnmarshalJSON parses either part shape back, so persisted state_json
// round-trips (doc/agent-impl.md §3.1).
func (p *Part) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type PartType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	p.Type = probe.Type
	switch probe.Type {
	case PartToolCall:
		var wire toolCallWire
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		p.ToolCallID, p.ToolName, p.Args = wire.ToolCallID, wire.ToolName, wire.Args
		p.Result, p.IsError, p.Approval = wire.Result, wire.IsError, wire.Approval
	case PartReasoning:
		var wire struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		p.ID, p.Text = wire.ID, wire.Text
	default:
		var wire struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		p.Text = wire.Text
	}
	return nil
}

// Approval is the FastTask-owned approval block on a proposal tool-call part
// (§2.7). Status is pending|approved|rejected.
type Approval struct {
	Status  string `json:"status"`
	Options []any  `json:"options,omitempty"`
}

const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// FastTaskState carries business state alongside the conversation so the two
// share one stream instead of a separate poll (§2.7). ThreadID is pushed on the
// first run of a new thread so the client can attach it to its thread list
// (§2.2).
type FastTaskState struct {
	ThreadID     string `json:"threadId,omitempty"`
	ActiveGoalID string `json:"activeGoalId,omitempty"`
	// RunID names the run this state belongs to. A harness host needs it to ask
	// for a run capability token (doc/harness.md §10.2): assistant-ui owns the
	// fetch that creates the run, so the browser learns the id from the stream
	// rather than from a response header it cannot read.
	RunID            string            `json:"runId,omitempty"`
	PendingProposals []PendingProposal `json:"pendingProposals"`
}

// PendingProposal summarizes an awaiting-approval proposal for the diff card.
type PendingProposal struct {
	ID           string `json:"id"`
	GoalID       string `json:"goalId"`
	BaseRevision int    `json:"baseRevision"`
	Summary      string `json:"summary"`
}

// --- String-path builders (§2.6, §2.7.1) ---
//
// update-state operation paths are STRING arrays; array indices are integer-form
// strings. These helpers are the single source of truth for the path shapes so
// the server is the sole owner of message indexing (§2.7.1).

// MessagePath addresses the whole message at index n.
func MessagePath(n int) []string { return []string{"messages", strconv.Itoa(n)} }

// MessagePartsPath addresses the parts array of message n.
func MessagePartsPath(n int) []string { return []string{"messages", strconv.Itoa(n), "parts"} }

// PartPath addresses part idx of message n.
func PartPath(n, idx int) []string {
	return []string{"messages", strconv.Itoa(n), "parts", strconv.Itoa(idx)}
}

// PartTextPath addresses the text string of part idx of message n — the target
// of append-text during streaming (§2.7.1).
func PartTextPath(n, idx int) []string {
	return []string{"messages", strconv.Itoa(n), "parts", strconv.Itoa(idx), "text"}
}

// MessageStatusPath addresses the status object of message n.
func MessageStatusPath(n int) []string { return []string{"messages", strconv.Itoa(n), "status"} }

// IsRunningPath addresses the top-level isRunning flag.
func IsRunningPath() []string { return []string{"isRunning"} }

// FastTaskPath addresses the fasttask business-state namespace.
func FastTaskPath() []string { return []string{"fasttask"} }

// FastTaskThreadIDPath addresses the threadId pushed back to the client on the
// first run of a new thread (§2.2): the server creates the conversation and
// sets ["fasttask","threadId"] so the converter can attach it to the thread list.
func FastTaskThreadIDPath() []string { return []string{"fasttask", "threadId"} }

// FastTaskRunIDPath addresses the runId a harness host needs in order to request
// its capability token (doc/harness.md §10.2).
func FastTaskRunIDPath() []string { return []string{"fasttask", "runId"} }
