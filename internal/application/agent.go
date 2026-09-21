package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// AgentService owns the agent runtime: it turns transport commands into runs,
// executes the run loop, and persists the authoritative thread state and chunk
// log (doc/agent-impl.md §3, §8). It is an application-layer service (wiring.md
// §4, AgentService / arch.md §7.5) and never exposes *gorm.DB upward.
//
// Phase B implements an ECHO executor: no model, no tools. Its purpose is to
// validate the transport end to end before the tool loop lands in phase C
// (agent-impl.md §11). Everything around the executor — thread/run/message
// persistence, the chunk log, state accumulation, resume — is the real thing.
type AgentService struct {
	store *persistence.Store
	repo  *persistence.AgentRepository
}

// NewAgentService builds the agent runtime over a store.
func NewAgentService(store *persistence.Store) *AgentService {
	return &AgentService{store: store, repo: persistence.NewAgentRepository(store)}
}

// Repository exposes the underlying agent repository for the HTTP layer's
// streaming reads (chunk replay, run lookup). All access stays user-scoped.
func (s *AgentService) Repository() *persistence.AgentRepository { return s.repo }

// --- Transport request contract (doc/agent-impl.md §2.2, §2.3) ---

// CommandsRequest is the body of POST /agent/commands. Per §2.2 the state,
// system and tools fields are UNTRUSTED and ignored: state is at most an
// optimistic hint, and system/tools are discarded so a client cannot inject a
// prompt or declare an unauthorized tool. threadId is validated against the
// authenticated user; user_id always comes from the auth context.
type CommandsRequest struct {
	Commands []Command       `json:"commands"`
	ThreadID *string         `json:"threadId"`
	ParentID *string         `json:"parentId"`
	State    json.RawMessage `json:"state"`
	System   *string         `json:"system"`
	Tools    json.RawMessage `json:"tools"`
}

// Command is one entry of the commands array (§2.3).
type Command struct {
	Type    string          `json:"type"`
	Message *CommandMessage `json:"message,omitempty"`

	ParentID *string `json:"parentId,omitempty"`
	SourceID *string `json:"sourceId,omitempty"`

	// add-tool-result (approval receipt) fields; handled in phase D (§7).
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

// CommandMessage is the message payload of an add-message command (§2.3).
type CommandMessage struct {
	Role  string        `json:"role"`
	Parts []CommandPart `json:"parts"`
}

// CommandPart is one part of an inbound message; phase B understands text.
type CommandPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// SubmitResult is returned by SubmitCommands so the HTTP layer can stream the
// run it just created.
type SubmitResult struct {
	ThreadID  string `json:"threadId"`
	RunID     string `json:"runId"`
	NewThread bool   `json:"newThread"`
}

// ErrEmptyCommand is returned when a commands request carries no usable
// add-message text (phase B does not yet accept tool results).
var ErrEmptyCommand = errors.New("agent command has no message text")

// SubmitCommands performs the transactional half of POST /agent/commands
// (§8): resolve-or-create the thread, enforce one active run per user, persist
// the user message, and create the agent_run + AgentJob(type="agent_run") that
// the Worker will execute. It writes no assistant output; that streams later.
func (s *AgentService) SubmitCommands(ctx context.Context, userID string, req CommandsRequest) (SubmitResult, error) {
	text, err := extractUserText(req.Commands)
	if err != nil {
		return SubmitResult{}, err
	}

	var result SubmitResult
	err = s.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo.WithTx(tx)

		threadID := ""
		if req.ThreadID != nil && strings.TrimSpace(*req.ThreadID) != "" {
			// Cross-user or missing thread is indistinguishable: 404 (§2.2).
			thread, err := repo.GetThread(ctx, userID, strings.TrimSpace(*req.ThreadID))
			if err != nil {
				return err
			}
			threadID = thread.ID
		} else {
			title := text
			if r := []rune(title); len(r) > 24 {
				title = string(r[:24]) + "…"
			}
			thread, err := repo.CreateThread(ctx, userID, nil, title)
			if err != nil {
				return err
			}
			threadID = thread.ID
			result.NewThread = true
		}

		// One active run per user (§9.3). The per-thread DB index is a backstop.
		var active int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentRun{}).
			Where("user_id = ? AND status IN ?", userID, []string{persistence.RunQueued, persistence.RunRunning, persistence.RunAwaitingApproval}).
			Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return persistence.ErrActiveRunExists
		}

		now := persistence.Now()
		run := &persistence.AgentRun{UserID: userID, ThreadID: threadID, Status: persistence.RunQueued, StateJSON: "{}"}
		if err := repo.CreateRun(ctx, run); err != nil {
			if persistence.IsUniqueViolation(err) {
				return persistence.ErrActiveRunExists
			}
			return err
		}

		job := persistence.AgentJob{
			ID: persistence.NewID("job"), UserID: userID, Type: "agent_run", Status: "queued",
			SubjectType: "agent_run", SubjectID: run.ID, BaseRevision: 0, InputJSON: "{}",
			MaxAttempts: 1, RunAfter: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&job).Error; err != nil {
			return err
		}
		if err := repo.UpdateRun(ctx, userID, run.ID, map[string]any{"job_id": job.ID}); err != nil {
			return err
		}

		userMessage := &persistence.AgentMessage{UserID: userID, ThreadID: threadID, RunID: run.ID, Role: "user"}
		if err := repo.CreateMessage(ctx, userMessage); err != nil {
			return err
		}
		if err := repo.CreatePart(ctx, &persistence.AgentMessagePart{
			UserID: userID, MessageID: userMessage.ID, Idx: 0, Type: "text", Text: text, ArgsJSON: "{}",
		}); err != nil {
			return err
		}
		if err := repo.UpdateRun(ctx, userID, run.ID, map[string]any{"parent_message_id": userMessage.ID}); err != nil {
			return err
		}
		if err := repo.TouchThread(ctx, userID, threadID); err != nil {
			return err
		}
		result.ThreadID = threadID
		result.RunID = run.ID
		return nil
	})
	if err != nil {
		return SubmitResult{}, err
	}
	return result, nil
}

// extractUserText concatenates the text parts of the first user add-message
// command. Tool results are not accepted in phase B (§7 lands in phase D).
func extractUserText(commands []Command) (string, error) {
	var builder strings.Builder
	sawMessage := false
	for _, command := range commands {
		switch command.Type {
		case "add-message":
			if command.Message == nil {
				continue
			}
			for _, part := range command.Message.Parts {
				if part.Type == "text" || part.Type == "" {
					builder.WriteString(part.Text)
					sawMessage = true
				}
			}
		case "add-tool-result":
			return "", fmt.Errorf("%w: tool results are handled by the approval flow", ErrEmptyCommand)
		}
	}
	text := strings.TrimSpace(builder.String())
	if !sawMessage || text == "" {
		return "", ErrEmptyCommand
	}
	return text, nil
}

// --- Run execution (Worker-driven; doc/agent-impl.md §6, §8) ---

// ChunkSink receives protocol chunks as a run executes. Phase B persists each
// chunk to the run's log; phase F adds an in-memory hub for low-latency fanout
// (§8: "SQLite 是持久日志，hub 只是实时通道").
type ChunkSink interface {
	Emit(ctx context.Context, chunk protocol.Chunk) error
}

// dbSink persists chunks to agent_run_chunks, assigning monotonic seq.
type dbSink struct {
	repo        *persistence.AgentRepository
	userID, run string
	lastSeq     int
	emitted     int
}

func (d *dbSink) Emit(ctx context.Context, chunk protocol.Chunk) error {
	if err := chunk.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("encode chunk: %w", err)
	}
	seq, err := d.repo.AppendChunk(ctx, d.userID, d.run, string(encoded))
	if err != nil {
		return err
	}
	d.lastSeq = seq
	d.emitted++
	return nil
}

// ExecuteRun runs one agent_run job to completion. It is called by the Worker
// (§8), so it runs independently of any HTTP request: a client disconnect does
// not cancel it (§4.3). Phase B is an echo; phase C replaces the body with the
// tool-calling loop while keeping this contract.
func (s *AgentService) ExecuteRun(ctx context.Context, job persistence.AgentJob) (map[string]any, error) {
	userID := job.UserID
	runID := job.SubjectID

	run, err := s.repo.GetRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}
	// Idempotent re-entry: if a prior attempt already finished the run, do not
	// duplicate its output (§7.4 spirit; full resume semantics land in phase F).
	switch run.Status {
	case persistence.RunSucceeded, persistence.RunFailed, persistence.RunCancelled, persistence.RunInterrupted:
		return map[string]any{"run_id": runID, "status": run.Status, "replayed": true}, nil
	}

	if err := s.repo.SetRunStatus(ctx, userID, runID, persistence.RunRunning, "", ""); err != nil {
		return nil, err
	}

	sink := &dbSink{repo: s.repo, userID: userID, run: runID}
	state := protocol.NewState()
	state.IsRunning = true
	state.FastTask.ThreadID = run.ThreadID

	fail := func(code string, err error) (map[string]any, error) {
		_ = s.repo.SetRunStatus(ctx, userID, runID, persistence.RunFailed, code, err.Error())
		return nil, err
	}

	emitState := func(ops ...protocol.Operation) error {
		return sink.Emit(ctx, protocol.UpdateState(ops...))
	}
	mustSet := func(path []string, value any) error {
		op, err := protocol.Set(path, value)
		if err != nil {
			return err
		}
		return emitState(op)
	}

	// isRunning=true and the threadId push (§2.2) come first so the client can
	// attach a brand-new thread before any message renders.
	if err := mustSet(protocol.IsRunningPath(), true); err != nil {
		return fail("STATE_ERROR", err)
	}
	if err := mustSet(protocol.FastTaskThreadIDPath(), run.ThreadID); err != nil {
		return fail("STATE_ERROR", err)
	}

	// Replay existing thread messages into authoritative state (§2.6). The user
	// message persisted at submit time is included here.
	existing, err := s.repo.ListThreadMessages(ctx, userID, run.ThreadID)
	if err != nil {
		return fail("STATE_ERROR", err)
	}
	for i, m := range existing {
		pm, err := s.toProtocolMessage(ctx, userID, m, protocol.CompleteStatus(""))
		if err != nil {
			return fail("STATE_ERROR", err)
		}
		state.Messages = append(state.Messages, pm)
		if err := mustSet(protocol.MessagePath(i), pm); err != nil {
			return fail("STATE_ERROR", err)
		}
	}

	userText := lastUserText(state.Messages)

	// Create the assistant message and stream an echo into it (§2.7.1):
	// set shell -> set empty text part -> append-text deltas -> set status.
	assistant := &persistence.AgentMessage{UserID: userID, ThreadID: run.ThreadID, RunID: runID, Role: "assistant"}
	if err := s.repo.CreateMessage(ctx, assistant); err != nil {
		return fail("STATE_ERROR", err)
	}
	assistantIdx := assistant.Seq - 1

	shell := protocol.Message{
		ID: assistant.ID, Role: protocol.RoleAssistant, Parts: []protocol.Part{},
		CreatedAt: protocol.RFC3339(assistant.CreatedAt), Status: protocol.RunningStatus(),
	}
	state.Messages = append(state.Messages, shell)
	if err := mustSet(protocol.MessagePath(assistantIdx), shell); err != nil {
		return fail("STATE_ERROR", err)
	}
	emptyText := protocol.TextPart("")
	if err := mustSet(protocol.PartPath(assistantIdx, 0), emptyText); err != nil {
		return fail("STATE_ERROR", err)
	}
	state.Messages[assistantIdx].Parts = []protocol.Part{emptyText}

	echo := echoReply(userText)
	for _, delta := range splitDeltas(echo, 12) {
		appendOp, err := protocol.AppendText(protocol.PartTextPath(assistantIdx, 0), delta)
		if err != nil {
			return fail("STATE_ERROR", err)
		}
		if err := emitState(appendOp); err != nil {
			return fail("STATE_ERROR", err)
		}
		state.Messages[assistantIdx].Parts[0].Text += delta
	}

	// Persist the assistant part with its full text for history/state rebuild.
	if err := s.repo.CreatePart(ctx, &persistence.AgentMessagePart{
		UserID: userID, MessageID: assistant.ID, Idx: 0, Type: "text", Text: echo, ArgsJSON: "{}",
	}); err != nil {
		return fail("STATE_ERROR", err)
	}

	// Finish: assistant status complete, isRunning false (§2.7.1).
	complete := protocol.CompleteStatus(protocol.ReasonStop)
	state.Messages[assistantIdx].Status = complete
	if err := mustSet(protocol.MessageStatusPath(assistantIdx), complete); err != nil {
		return fail("STATE_ERROR", err)
	}
	state.IsRunning = false
	if err := mustSet(protocol.IsRunningPath(), false); err != nil {
		return fail("STATE_ERROR", err)
	}

	encodedState, err := json.Marshal(state)
	if err != nil {
		return fail("STATE_ERROR", err)
	}
	// checkpoint_seq is the number of chunks reflected in state_json, so a resume
	// replays only chunks with seq >= checkpoint and never re-applies append-text
	// already folded into the returned state (§2.8).
	if err := s.repo.SaveRunState(ctx, userID, runID, string(encodedState), sink.emitted); err != nil {
		return fail("STATE_ERROR", err)
	}
	if err := s.repo.SetRunStatus(ctx, userID, runID, persistence.RunSucceeded, "", ""); err != nil {
		return fail("STATE_ERROR", err)
	}
	return map[string]any{"run_id": runID, "status": persistence.RunSucceeded, "chunks": sink.emitted, "echo": true}, nil
}

// toProtocolMessage rebuilds a wire message from persisted rows.
func (s *AgentService) toProtocolMessage(ctx context.Context, userID string, m persistence.AgentMessage, status protocol.MessageStatus) (protocol.Message, error) {
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
		case "tool-call":
			tp := protocol.ToolCallPart(derefString(p.ToolCallID), p.ToolName, nil)
			tp.Approval = approvalFromStatus(p.ApprovalStatus)
			wire.Parts = append(wire.Parts, tp)
		default:
			wire.Parts = append(wire.Parts, protocol.TextPart(p.Text))
		}
	}
	return wire, nil
}

func approvalFromStatus(status string) *protocol.Approval {
	if status == "" {
		return nil
	}
	return &protocol.Approval{Status: status}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// lastUserText returns the concatenated text parts of the most recent user
// message, used by the phase B echo. It reads from the already-built wire
// messages so it never depends on message ordering in the persistence layer.
func lastUserText(messages []protocol.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != protocol.RoleUser {
			continue
		}
		var builder strings.Builder
		for _, part := range messages[i].Parts {
			if part.Type == protocol.PartText {
				builder.WriteString(part.Text)
			}
		}
		return strings.TrimSpace(builder.String())
	}
	return ""
}

// echoReply is the phase B fake assistant message (agent-impl.md §11: "先用回声
// 实现验证协议理解是否正确"). It is intentionally labelled so no one mistakes it
// for a real model reply.
func echoReply(userText string) string {
	if userText == "" {
		userText = "(空消息)"
	}
	return "【阶段 B 回声】我已收到：" + userText +
		"。Agent 运行时已连通，但尚未接入模型与工具（阶段 C 起接入真实循环）。" +
		"现在先挑一个 5–15 分钟能开始的最小行动推进它。"
}

// splitDeltas breaks text into rune-safe chunks of at most size runes so the
// stream exercises append-text incrementally rather than in one shot.
func splitDeltas(text string, size int) []string {
	if size <= 0 {
		size = 1
	}
	var deltas []string
	runes := []rune(text)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		deltas = append(deltas, string(runes[i:end]))
	}
	if len(deltas) == 0 {
		deltas = []string{""}
	}
	return deltas
}

// --- Resume (doc/agent-impl.md §2.8) ---

// ResumeStateResult is the 200 body of POST /agent/resume-state.
type ResumeStateResult struct {
	State json.RawMessage `json:"state"`
	RunID string          `json:"runId"`
}

// ResumeState returns the retained state and runId for a thread's active run, or
// found=false when there is none (the handler answers 204). threadId ownership
// is enforced; a cross-user thread yields not-found (§2.8, §10).
func (s *AgentService) ResumeState(ctx context.Context, userID, threadID string) (ResumeStateResult, bool, error) {
	// A cross-user or missing thread is indistinguishable from not-found (§2.2).
	if _, err := s.repo.GetThread(ctx, userID, threadID); err != nil {
		return ResumeStateResult{}, false, err
	}
	run, err := s.repo.GetActiveRunByThread(ctx, userID, threadID)
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
