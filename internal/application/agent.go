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
	store *persistence.Store
	app   *App
	repo  *persistence.AgentRepository
	tools *ToolRegistry
	// sidecar drives the runs whose host is the Node process instead of the browser
	// (doc/harness.md §8). Nil in the common case: the sidecar is a fallback.
	sidecar      SidecarDriver
	limits       LoopLimits
	systemPrompt string
	// approvalService carries the §7 approval-receipt flow, embedded so
	// s.ResolveApproval resolves by promotion (wiring.md §9: AgentService declares
	// ≤15 methods). Its own app/repo/store fields shadow nothing at depth 0.
	*approvalService
	// threadStateService carries the persisted-state readers, embedded for the
	// same reason: s.toProtocolMessage, s.LatestThreadState and
	// s.ResumeCurrentState all resolve by promotion.
	*threadStateService
	// harness carries the host-facing runtime (doc/harness.md): capability tokens, the tool manifest,
	// the model proxy, the tool execution surface and the run lifecycle. It is embedded so the HTTP
	// layer keeps one service to call.
	*harnessService
	// threads is the user-facing catalogue: list, rename, archive, delete
	// (doc/chat-features.md §2).
	*threadCatalogue
	// attachments owns uploaded images and the bytes behind them (doc/chat-features.md §4). An empty
	// directory means uploads are not configured, which is a 400 rather than a panic.
	attachments *AttachmentStore
	// creds resolves the upstream model the proxy injects. Unlike chat, which drove
	// the in-process loop, it yields raw coordinates that must never leave the
	// process (§4.5).
	creds CredentialsResolver
	// reasoningLevel is the server default the host sets on its model calls
	// (doc/chat-features.md §3.2). The proxy does not override it: a reasoning level
	// guards no invariant.
	reasoningLevel string
	// reasoningPersisted decides whether reasoning deltas are stored. An operator who
	// does not want chains of thought in the database turns it off and the proxy drops
	// them instead of persisting (§3.4).
	reasoningPersisted bool
	// nowFn is the injectable clock the harness lifecycle paths are tested with.
	nowFn func() time.Time
	// approvalSegment and approvalLimit bound the approval long poll (doc/harness.md §7). They
	// are fields rather than constants so the timeout path can be tested without waiting a
	// quarter of an hour.
	approvalSegment time.Duration
	approvalLimit   time.Duration
}

// SidecarDriver drives one run in the Node sidecar (doc/harness.md §8.3). The Worker calls
// it instead of a loop; the sidecar then talks back through the same endpoints a browser host
// uses, which is what makes the two hosts equivalent (§14.3).
type SidecarDriver interface {
	// Drive runs one turn to completion and returns what the host reported.
	Drive(ctx context.Context, run SidecarRun) (SidecarResult, error)
	// Cancel asks the sidecar to stop a run (§5.2).
	Cancel(ctx context.Context, runID string) error
	// Healthy reports whether the sidecar can accept a run right now (§1.2: a sidecar run
	// must not be queued behind a sidecar that is down).
	Healthy(ctx context.Context) bool
}

// SidecarRun is one drive request. The token is a normal harness token, so the sidecar has
// exactly the capability a browser host has and no more (§8.1).
type SidecarRun struct {
	RunID        string
	HarnessToken string
	Prompt       string
	Checkpoint   []byte
	LibfxVersion string
}

// SidecarResult is what the host reported at the end of its turn (§5.1).
type SidecarResult struct {
	StopReason   string
	ErrorMessage string
	Usage        map[string]any
	Checkpoint   []byte
	LibfxVersion string
}

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

// WithSidecarDriver installs the Node sidecar (§8). Without it, sidecar mode is refused
// rather than queued behind a host that does not exist (§1.2).
func WithSidecarDriver(driver SidecarDriver) AgentOption {
	return func(s *AgentService) { s.sidecar = driver }
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

// WithCredentialsResolver installs the upstream model resolver the harness proxy
// injects (doc/harness.md §4.3). Without it no host can be granted a run.
func WithCredentialsResolver(resolver CredentialsResolver) AgentOption {
	return func(s *AgentService) { s.creds = resolver }
}

// WithReasoningLevel sets the default reasoning level handed to a host
// (doc/chat-features.md §3.2). An unknown value falls back to provider-default
// rather than being passed through.
func WithReasoningLevel(level string) AgentOption {
	return func(s *AgentService) { s.reasoningLevel = normalizeReasoningLevel(level) }
}

// WithReasoningPersistence turns reasoning-part storage on or off
// (doc/chat-features.md §3.4).
func WithReasoningPersistence(enabled bool) AgentOption {
	return func(s *AgentService) { s.reasoningPersisted = enabled }
}

// WithClock overrides the clock the harness lifecycle uses, so heartbeat loss and
// cancellation grace can be tested without sleeping.
func WithClock(now func() time.Time) AgentOption {
	return func(s *AgentService) { s.nowFn = now }
}

// WithAttachmentDir configures where uploaded images are stored (doc/chat-features.md §4.3). An empty
// directory leaves uploads disabled.
func WithAttachmentDir(dir string) AgentOption {
	return func(s *AgentService) {
		if strings.TrimSpace(dir) != "" {
			s.attachments.dir = dir
		}
	}
}

// Attachments exposes the attachment store for the HTTP layer's upload, read and delete endpoints.
func (s *AgentService) Attachments() *AttachmentStore { return s.attachments }

// WithApprovalWait overrides the approval long poll's segment and total limit
// (doc/harness.md §7, §15).
func WithApprovalWait(segment, limit time.Duration) AgentOption {
	return func(s *AgentService) { s.approvalSegment, s.approvalLimit = segment, limit }
}

// approvalWait returns the configured long-poll segment and total wait limit.
func (s *AgentService) approvalWait() (time.Duration, time.Duration) {
	segment, limit := s.approvalSegment, s.approvalLimit
	if segment <= 0 {
		segment = approvalPollSegment
	}
	if limit <= 0 {
		limit = ApprovalWaitLimit
	}
	return segment, limit
}

// reasoningLevels are the values a host may ask for (doc/chat-features.md §3.2).
var reasoningLevels = map[string]bool{
	"provider-default": true, "none": true, "minimal": true,
	"low": true, "medium": true, "high": true, "xhigh": true,
}

// normalizeReasoningLevel folds anything outside the enum onto provider-default, so
// an illegal value can never reach a provider (§3.2).
func normalizeReasoningLevel(level string) string {
	trimmed := strings.ToLower(strings.TrimSpace(level))
	if !reasoningLevels[trimmed] {
		return "provider-default"
	}
	return trimmed
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
	repo := persistence.NewAgentRepository(app.Store)
	s := &AgentService{
		store:              app.Store,
		app:                app,
		repo:               repo,
		tools:              registry,
		limits:             DefaultLoopLimits(),
		systemPrompt:       defaultSystemPrompt,
		reasoningLevel:     "provider-default",
		reasoningPersisted: true,
		approvalSegment:    approvalPollSegment,
		approvalLimit:      ApprovalWaitLimit,
		// The approval flow shares the same repo/store/app so a receipt resolves
		// against identical state (§7).
		approvalService:    &approvalService{app: app, repo: repo, store: app.Store},
		threadStateService: &threadStateService{repo: repo},
	}
	// The harness reaches its collaborators through the service, so it is built after the
	// fields exist and before options run: an option may replace the clock or the credentials
	// resolver the harness reads at request time.
	s.harnessService = newHarnessService(s)
	s.approvalService.svc = s
	s.threadCatalogue = &threadCatalogue{svc: s}
	s.attachments = NewAttachmentStore("", repo)
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// credentials resolves the upstream model for a harness run. (nil, nil) means no model
// is configured, which makes the harness unavailable rather than degraded
// (doc/harness.md §3.2).
func (s *AgentService) credentials(ctx context.Context) (*ModelCredentials, error) {
	if s.creds == nil {
		return nil, nil
	}
	return s.creds(ctx)
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
	// HarnessMode names the host that will drive the run: "wasm" for the browser,
	// "sidecar" for the Node fallback (doc/harness.md §1.2). It decides WHO drives and
	// nothing else — no invariant depends on it — which is why it may come from the client.
	HarnessMode string `json:"harness_mode"`
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

// CommandPart is one part of an inbound message: text, or an image reference.
//
// An image part carries a URL on our own attachment endpoint and never image bytes
// (doc/chat-features.md §4.2). Inline data is dropped, because every image that reaches a model has to
// pass an ownership check first, and bytes in a command body cannot.
type CommandPart struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Image string `json:"image,omitempty"`
}

// ErrTooManyAttachments is the §4.5 per-message cap. It maps to 400 with a message the user can act on.
var ErrTooManyAttachments = errors.New("a message may carry at most 4 images")

// SubmitResult is returned by SubmitCommands so the HTTP layer can stream the run it just
// created or resumed.
type SubmitResult struct {
	ThreadID  string `json:"threadId"`
	RunID     string `json:"runId"`
	NewThread bool   `json:"newThread"`
	// FromSeq is the chunk-log cursor the client should stream from. A fresh run streams
	// from 0; a run resumed after approval streams from its awaiting checkpoint so
	// already-rendered chunks (and their append-text) are not re-applied (§2.8, §7.2).
	FromSeq int `json:"fromSeq"`
	// HarnessMode is the mode the server ACCEPTED, which is not necessarily the one asked
	// for: with no model configured a run stays server-driven (doc/harness.md §1.2).
	// "wasm" means the caller must hand the run to a browser host, and the response carries
	// X-Harness-Run so a client that can read headers does not have to wait for the stream.
	HarnessMode string `json:"harnessMode,omitempty"`
	// NoStream marks a receipt that must not open a second stream: during a harness run the
	// approval decision is delivered to the host's long poll and the transcript continues on
	// the stream that is already open (§7).
	NoStream bool `json:"-"`
}

// ErrEmptyCommand is returned when a commands request carries no usable
// add-message text and no tool-result receipt.
var ErrEmptyCommand = errors.New("agent command has no message text")

// SubmitCommands performs the transactional half of POST /agent/commands: resolve-or-create
// the thread, enforce one active run per user, persist the user message and create the
// agent_run. It writes no assistant output; that streams later.
//
// Whether an AgentJob is created depends on who drives the run (doc/harness.md §1.2, which
// supersedes agent-impl.md §8):
//
//   - wasm: NO job. The browser host drives the run, so a job would make the Worker execute
//     the same run a second time. The preamble is emitted here instead, and the response
//     carries X-Harness-Run.
//   - sidecar: a job, which the Worker turns into a sidecar call rather than a loop.
//   - "": a job, executed in-process. With no model configured this is the deterministic
//     reply the README promises.
//
// An add-tool-result command is routed to the approval flow instead (§7.2): it resolves the
// pending proposal and continues the SAME run rather than creating one.
func (s *AgentService) SubmitCommands(ctx context.Context, userID string, req CommandsRequest) (SubmitResult, error) {
	if cmd, ok := firstToolResult(req.Commands); ok {
		return s.ResolveApproval(ctx, userID, cmd)
	}
	text, images, err := extractUserMessage(req.Commands)
	if err != nil {
		return SubmitResult{}, err
	}
	if len(images) > AttachmentMaxPerMessage {
		return SubmitResult{}, ErrTooManyAttachments
	}
	mode, err := s.resolveHarnessMode(ctx, req.HarnessMode)
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
		run := &persistence.AgentRun{
			UserID: userID, ThreadID: threadID, Status: persistence.RunQueued,
			StateJSON: "{}", HarnessMode: mode,
		}
		if err := repo.CreateRun(ctx, run); err != nil {
			if persistence.IsUniqueViolation(err) {
				return persistence.ErrActiveRunExists
			}
			return err
		}

		// A browser-driven run gets no job (§1.2): the Worker must not execute what the host
		// is already executing.
		if mode != HarnessModeWASM {
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
		// Attachments are stored as references and bound to this message, so a thread deletion takes
		// them with it and the orphan sweep leaves them alone (doc/chat-features.md §4.5).
		for i, reference := range images {
			attachmentID, ok := AttachmentIDFromRef(reference)
			if !ok {
				// A reference that is not ours is not an attachment; it is dropped rather than stored,
				// because the alternative is persisting a URL the server cannot vouch for.
				continue
			}
			if _, err := repo.GetAttachment(ctx, userID, attachmentID); err != nil {
				continue
			}
			if err := repo.CreatePart(ctx, &persistence.AgentMessagePart{
				UserID: userID, MessageID: userMessage.ID, Idx: i + 1, Type: "image", Text: reference,
				ArgsJSON: "{}",
			}); err != nil {
				return err
			}
			// Through the transaction-bound repository: binding through the store's own handle would
			// open a second writer while this transaction holds the lock, and SQLite would sit on it
			// until the busy timeout expired.
			if err := repo.AttachAttachment(ctx, userID, attachmentID, threadID, userMessage.ID); err != nil {
				return err
			}
		}
		if err := repo.UpdateRun(ctx, userID, run.ID, map[string]any{"parent_message_id": userMessage.ID}); err != nil {
			return err
		}
		if err := repo.TouchThread(ctx, userID, threadID); err != nil {
			return err
		}
		result.ThreadID = threadID
		result.RunID = run.ID
		result.HarnessMode = mode
		return nil
	})
	if err != nil {
		return SubmitResult{}, err
	}
	if mode == HarnessModeWASM {
		// The preamble has to exist before the client's stream starts polling, or the host
		// has nothing to attach to and the UI renders an empty thread (§1.2, §2.7.1).
		if err := s.BeginHarnessRun(ctx, userID, result.RunID); err != nil {
			return SubmitResult{}, err
		}
	}
	return result, nil
}

// resolveHarnessMode decides who drives a run (doc/harness.md §1.2). The client's request is
// a preference, not a decision: with no model configured there is nothing for a host to call,
// so the run stays server-driven and keeps the deterministic reply; a sidecar request without
// a healthy sidecar is refused instead of being queued behind a host that will never pick it
// up (§1.2: HARNESS_UNAVAILABLE rather than a job nobody runs).
func (s *AgentService) resolveHarnessMode(ctx context.Context, requested string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(requested))
	switch mode {
	case "":
		return "", nil
	case HarnessModeWASM, HarnessModeSidecar:
	default:
		return "", fmt.Errorf("%w: harness_mode must be wasm or sidecar", ErrValidation)
	}
	creds, err := s.credentials(ctx)
	if err != nil {
		return "", err
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" || creds.Model == "" {
		return "", nil
	}
	if mode == HarnessModeSidecar && (s.sidecar == nil || !s.sidecar.Healthy(ctx)) {
		return "", ErrHarnessUnavailable
	}
	return mode, nil
}

// extractUserMessage reads the text and the attachment references out of the first user add-message
// command. A tool result is not a message and is refused here; the approval flow owns those.
//
// A message of only images is accepted: "look at this plot" with no words is a complete request.
func extractUserMessage(commands []Command) (string, []string, error) {
	var builder strings.Builder
	var images []string
	sawMessage := false
	for _, command := range commands {
		switch command.Type {
		case "add-message":
			if command.Message == nil {
				continue
			}
			for _, part := range command.Message.Parts {
				switch {
				case part.Type == "text" || part.Type == "":
					builder.WriteString(part.Text)
					sawMessage = true
				case part.Type == "image":
					if reference := strings.TrimSpace(part.Image); reference != "" {
						images = append(images, reference)
						sawMessage = true
					}
				}
			}
		case "add-tool-result":
			return "", nil, fmt.Errorf("%w: tool results are handled by the approval flow", ErrEmptyCommand)
		}
	}
	text := strings.TrimSpace(builder.String())
	if !sawMessage || (text == "" && len(images) == 0) {
		return "", nil, ErrEmptyCommand
	}
	return text, images, nil
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

// ExecuteRun runs one agent_run job. It is called by the Worker, so it runs independently of
// any HTTP request: a client disconnect does not cancel it (agent-impl.md §4.3).
//
// Under the harness only two kinds of run arrive here (doc/harness.md §1.2):
//
//   - a sidecar-driven run, which the Worker hands to the Node host instead of looping;
//   - a run with no model configured, which keeps the deterministic reply the README promises
//     ("LLM 未配置时使用确定性本地 Provider").
//
// A WASM-driven run never becomes a job at all. That is the whole point of §1.2: a job would
// have the Worker execute a run the browser is already driving.
func (s *AgentService) ExecuteRun(ctx context.Context, job persistence.AgentJob) (map[string]any, error) {
	userID, runID := job.UserID, job.SubjectID

	run, err := s.repo.GetRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}
	// Idempotent re-entry: a prior attempt already finished this run, so its output must not
	// be duplicated.
	if IsTerminalRunStatus(run.Status) {
		return map[string]any{"run_id": runID, "status": run.Status, "replayed": true}, nil
	}
	if run.HarnessMode == HarnessModeSidecar {
		return s.driveViaSidecar(ctx, job, run)
	}
	if run.HarnessMode == HarnessModeWASM {
		// Unreachable through SubmitCommands, which creates no job for a WASM run. Fail loudly
		// instead of driving a run a browser may already be driving.
		return nil, s.repo.SetRunStatus(ctx, userID, runID, persistence.RunFailed,
			"HARNESS_CONFLICT", "a browser-driven run must not be executed by the worker")
	}

	if err := s.repo.SetRunStatus(ctx, userID, runID, persistence.RunRunning, "", ""); err != nil {
		return nil, err
	}
	sess, err := s.beginRun(ctx, job, run)
	if err != nil {
		_ = s.repo.SetRunStatus(ctx, userID, runID, persistence.RunFailed, "STATE_ERROR", err.Error())
		return nil, err
	}
	// No model is configured for this run — with one, SubmitCommands would have made it a
	// harness run. The deterministic reply is labelled so it is never mistaken for a model
	// answer.
	return s.deterministicReply(sess)
}

// driveViaSidecar hands a run to the Node host (§8.3). The Worker's job here is bookkeeping:
// emit the preamble the UI renders from, mint the capability the sidecar drives with, wait,
// and record what came back. The sidecar writes no transcript of its own — the model proxy
// already did (§4.4), which is why the two hosts are equivalent (§14.3).
func (s *AgentService) driveViaSidecar(ctx context.Context, job persistence.AgentJob, run *persistence.AgentRun) (map[string]any, error) {
	if s.sidecar == nil || !s.sidecar.Healthy(ctx) {
		return nil, s.failHarnessRun(ctx, run, "HARNESS_UNAVAILABLE", "the sidecar host is not available")
	}
	prompt, err := s.runPromptText(ctx, run)
	if err != nil {
		return nil, s.failHarnessRun(ctx, run, "STATE_ERROR", err.Error())
	}
	if err := s.BeginHarnessRun(ctx, run.UserID, run.ID); err != nil {
		return nil, s.failHarnessRun(ctx, run, "STATE_ERROR", err.Error())
	}
	grant, err := s.IssueRunGrant(ctx, run.UserID, run.ID, "")
	if err != nil {
		return nil, s.failHarnessRun(ctx, run, "HARNESS_UNAVAILABLE", err.Error())
	}
	thread, err := s.repo.GetThread(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return nil, s.failHarnessRun(ctx, run, "STATE_ERROR", err.Error())
	}
	result, err := s.sidecar.Drive(ctx, SidecarRun{
		RunID: run.ID, HarnessToken: grant.HarnessToken, Prompt: prompt,
		Checkpoint: thread.Checkpoint, LibfxVersion: thread.LibfxVersion,
	})
	if err != nil {
		return nil, s.failHarnessRun(ctx, run, "PROVIDER_ERROR", err.Error())
	}
	principal := HarnessPrincipal{UserID: run.UserID, ThreadID: run.ThreadID, RunID: run.ID, Mode: HarnessModeSidecar}
	completion := Completion{
		StopReason: result.StopReason, ErrorMessage: result.ErrorMessage, Usage: result.Usage,
		Checkpoint: result.Checkpoint, LibfxVersion: result.LibfxVersion,
	}
	if err := s.CompleteRun(ctx, principal, completion); err != nil {
		return nil, err
	}
	_ = job.ID
	return map[string]any{"run_id": run.ID, "status": "succeeded", "stop_reason": result.StopReason}, nil
}

// failHarnessRun records a run that never got as far as a host, and returns the error so the
// Worker's own bookkeeping sees it too.
func (s *AgentService) failHarnessRun(ctx context.Context, run *persistence.AgentRun, code, message string) error {
	_ = s.repo.SetRunStatus(ctx, run.UserID, run.ID, persistence.RunFailed, code, message)
	return fmt.Errorf("%s: %s", code, message)
}

// runPromptText is the user message a run answers (§5.1: one user message, one run, one
// prompt). It is read from the authoritative parts, never from a request body.
func (s *AgentService) runPromptText(ctx context.Context, run *persistence.AgentRun) (string, error) {
	messageID := derefString(run.ParentMessageID)
	if messageID == "" {
		return "", errors.New("run has no user message")
	}
	parts, err := s.repo.ListMessageParts(ctx, run.UserID, messageID)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(builder.String()), nil
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

// echoReply is the deterministic fallback used when no model is configured.
// It is intentionally labelled so no one mistakes it for a model reply.
func echoReply(userText string) string {
	if userText == "" {
		userText = "(空消息)"
	}
	return "【未配置模型】我已收到：" + userText +
		"。请配置支持工具调用的模型，以获得完整的 Agent 建议。" +
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
