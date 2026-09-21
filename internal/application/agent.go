package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// AgentService owns the agent runtime: it turns transport commands into runs,
// executes the run loop, and persists the authoritative thread state and chunk
// log (doc/agent-impl.md §3, §8). It is an application-layer service (wiring.md
// §4, AgentService / arch.md §7.5) and never exposes *gorm.DB upward.
//
// When a ChatProvider is resolvable the run executes the multi-turn tool loop
// (§6). When none is configured — no LLM, matching the README's "LLM 未配置时
// Worker 使用确定性本地 Provider" — it falls back to a deterministic reply so the
// product and its tests keep working without a model.
type AgentService struct {
	store        *persistence.Store
	app          *App
	repo         *persistence.AgentRepository
	tools        *ToolRegistry
	chat         ChatResolver
	limits       LoopLimits
	systemPrompt string
}

// ChatResolver resolves the tool-calling model for a run, mirroring the Worker's
// provider resolver. It returns (nil, nil) when no model is configured, which
// selects the deterministic fallback rather than an error.
type ChatResolver func(ctx context.Context) (agent.ChatProvider, error)

// LoopLimits are the conservative bounds from agent-impl.md §6. They are
// overridable for tests and tunable after observing a real model (§11).
type LoopLimits struct {
	MaxTurns        int           // single run's model turns (default 8)
	WallClock       time.Duration // single run wall clock (default 180s)
	ToolTimeout     time.Duration // single tool execution (default 10s)
	MaxOutputTokens int           // per-turn output cap; 0 defers to the provider
}

// DefaultLoopLimits returns the §6 conservative defaults.
func DefaultLoopLimits() LoopLimits {
	return LoopLimits{MaxTurns: 8, WallClock: 180 * time.Second, ToolTimeout: 10 * time.Second}
}

// AgentOption configures an AgentService at construction.
type AgentOption func(*AgentService)

// WithChatResolver installs the model resolver. Without it the service runs the
// deterministic fallback.
func WithChatResolver(resolver ChatResolver) AgentOption {
	return func(s *AgentService) { s.chat = resolver }
}

// WithToolRegistry overrides the default read-only tool set (phase D adds
// proposal tools).
func WithToolRegistry(registry *ToolRegistry) AgentOption {
	return func(s *AgentService) { s.tools = registry }
}

// WithLoopLimits overrides the §6 bounds (used by tests).
func WithLoopLimits(limits LoopLimits) AgentOption {
	return func(s *AgentService) { s.limits = limits }
}

// WithSystemPrompt overrides the server-side system prompt. The client-supplied
// system field is always ignored (§2.2); this is the authoritative one.
func WithSystemPrompt(prompt string) AgentOption {
	return func(s *AgentService) { s.systemPrompt = prompt }
}

// NewAgentService builds the agent runtime over an App. It registers the default
// read-only tools (§5.1); a chat resolver supplied via options enables the real
// loop, otherwise runs use the deterministic fallback.
func NewAgentService(app *App, opts ...AgentOption) *AgentService {
	builtIn := append(NewReadonlyTools(app), NewProposalTools(app)...)
	registry, err := NewToolRegistry(builtIn...)
	if err != nil {
		// Unreachable for the built-in set (unique names, no identity fields).
		// Degrade to "no tools" rather than crash boot.
		registry, _ = NewToolRegistry()
	}
	s := &AgentService{
		store:        app.Store,
		app:          app,
		repo:         persistence.NewAgentRepository(app.Store),
		tools:        registry,
		limits:       DefaultLoopLimits(),
		systemPrompt: defaultSystemPrompt,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Repository exposes the underlying agent repository for the HTTP layer's
// streaming reads (chunk replay, run lookup). All access stays user-scoped.
func (s *AgentService) Repository() *persistence.AgentRepository { return s.repo }

// Tools exposes the tool registry (used by tests and phase D proposal wiring).
func (s *AgentService) Tools() *ToolRegistry { return s.tools }

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
// run it just created or resumed.
type SubmitResult struct {
	ThreadID  string `json:"threadId"`
	RunID     string `json:"runId"`
	NewThread bool   `json:"newThread"`
	// FromSeq is the chunk-log cursor the client should stream from. A fresh run
	// streams from 0; a run resumed after approval streams from its awaiting
	// checkpoint so already-rendered chunks (and their append-text) are not
	// re-applied (§2.8, §7.2).
	FromSeq int `json:"fromSeq"`
}

// ErrEmptyCommand is returned when a commands request carries no usable
// add-message text and no tool-result receipt.
var ErrEmptyCommand = errors.New("agent command has no message text")

// SubmitCommands performs the transactional half of POST /agent/commands
// (§8): resolve-or-create the thread, enforce one active run per user, persist
// the user message, and create the agent_run + AgentJob(type="agent_run") that
// the Worker will execute. It writes no assistant output; that streams later.
//
// An add-tool-result command is routed to the approval flow instead (§7.2): it
// resolves the pending proposal and resumes the SAME run rather than creating one.
func (s *AgentService) SubmitCommands(ctx context.Context, userID string, req CommandsRequest) (SubmitResult, error) {
	if cmd, ok := firstToolResult(req.Commands); ok {
		return s.ResolveApproval(ctx, userID, cmd)
	}
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

// checkpoint is the seq the next chunk will take, i.e. the count of chunks
// committed so far across ALL segments of the run. It is the resume cursor: a
// client caught up to checkpoint needs seq >= checkpoint next (§2.8). Using
// lastSeq+1 (not emitted) keeps it correct when a run resumes after approval and
// the sink for the new segment starts counting from zero.
func (d *dbSink) checkpoint() int {
	if d.emitted == 0 {
		return d.lastSeq
	}
	return d.lastSeq + 1
}

// ExecuteRun runs one agent_run job to completion. It is called by the Worker
// (§8), so it runs independently of any HTTP request: a client disconnect does
// not cancel it (§4.3).
//
// It sets up the run session (isRunning, threadId push, message replay, assistant
// shell), then dispatches to the multi-turn tool loop when a ChatProvider is
// configured (§6), or to a deterministic reply when no model is available (the
// README's "LLM 未配置时使用确定性本地 Provider" behaviour).
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

	// A run that paused at awaiting_approval already has an assistant message; an
	// approval receipt re-queues it to continue the SAME run and message (§7.2).
	// A fresh run has none yet. This distinguishes resume from first execution.
	priorAssistant, err := s.existingAssistantMessage(ctx, userID, runID)
	if err != nil {
		_ = s.repo.SetRunStatus(ctx, userID, runID, persistence.RunFailed, "STATE_ERROR", err.Error())
		return nil, err
	}

	var sess *runSession
	if priorAssistant != nil {
		sess, err = s.resumeRun(ctx, job, run, *priorAssistant)
	} else {
		sess, err = s.beginRun(ctx, job, run)
	}
	if err != nil {
		_ = s.repo.SetRunStatus(ctx, userID, runID, persistence.RunFailed, "STATE_ERROR", err.Error())
		return nil, err
	}

	var provider agent.ChatProvider
	if s.chat != nil {
		provider, err = s.chat(ctx)
		if err != nil {
			return sess.failRun("PROVIDER_ERROR", err)
		}
	}
	if provider == nil {
		// No model: a fresh run gets the deterministic reply. A resumed run has
		// already delivered its proposal outcome, so just close it cleanly.
		if sess.resumed {
			return sess.succeed()
		}
		return s.deterministicReply(sess)
	}
	return s.toolLoop(sess, provider)
}

// existingAssistantMessage returns the run's assistant message if one was already
// created (a resumed run), or nil for a fresh run.
func (s *AgentService) existingAssistantMessage(ctx context.Context, userID, runID string) (*persistence.AgentMessage, error) {
	messages, err := s.repo.ListRunMessages(ctx, userID, runID)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		if messages[i].Role == "assistant" {
			return &messages[i], nil
		}
	}
	return nil, nil
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

// decodeArgsMap parses persisted tool-call args back into a map for the wire part.
func decodeArgsMap(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]any{}
	}
	return args
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
