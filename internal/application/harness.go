package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// The agent harness (doc/harness.md).
//
// The loop that used to run inside the Worker now runs in a host process — the
// browser's WASM instance by default, a Node sidecar when the browser lacks JSPI.
// What stays here is everything that must not be client-side: the model
// credentials, the authoritative transcript, the tool authority and the run
// lifecycle. The host is a driver, never a source of truth (§4.4).
//
// This file holds the capability side: harness tokens, the tool manifest they
// authorize, the heartbeat that proves a host is alive, and the reaper that
// finishes runs whose host is not. The model proxy lives in harnessproxy.go, the
// tool execution surface in harnessrun.go.

// Harness modes (doc/harness.md §1.2). The mode decides who drives a run; it never
// relaxes an invariant, which is why it may come from the client.
const (
	HarnessModeWASM    = "wasm"
	HarnessModeSidecar = "sidecar"
)

// Harness tuning. Every value is the documented default (doc/harness.md §10, §15);
// the host is told the heartbeat interval instead of hardcoding it.
const (
	// harnessTokenTTL is the default life of a run capability (§10.2).
	harnessTokenTTL = 30 * time.Minute
	// harnessRenewBelow triggers a renewal inside a heartbeat response (§10.4).
	harnessRenewBelow = 10 * time.Minute
	// harnessHeartbeatInterval is what a host is asked to beat at (§10.4).
	harnessHeartbeatInterval = 10 * time.Second
	// HarnessHeartbeatLoss is how long a silent host is tolerated before its run is
	// interrupted (§10.4).
	HarnessHeartbeatLoss = 45 * time.Second
	// harnessPickupGrace is how long a run may wait for a host that never asks for a
	// token — a tab closed between POST /agent/commands and POST /agent/runs.
	harnessPickupGrace = 60 * time.Second
	// HarnessCancelGrace is how long a cancelling run is given to confirm before the
	// reaper finishes the transition (§5.2: cancellation is best effort).
	HarnessCancelGrace = 5 * time.Second
	// InstructionsLimit is libfx's cap on the system prompt, inclusive of anything
	// the degraded path appends (§3.5).
	InstructionsLimit = 64 * 1024
	// LibfxVersion is the pinned harness version (§4.6). A checkpoint written by
	// another version takes the degraded path (§6.3).
	LibfxVersion = "0.0.10"
	// toolManifestWarnAt is libfx's 64-tool ceiling minus slack (§3.4).
	toolManifestWarnAt = 60
	// approvalPollSegment bounds one long-poll segment so a proxy in between cannot cut the wait
	// (§7).
	approvalPollSegment = 30 * time.Second
	// ApprovalWaitLimit is the whole approval wait (§7). It is not part of the run wall clock.
	ApprovalWaitLimit = 15 * time.Minute
)

// Harness errors. Each maps to one documented error code (doc/interface.md §20).
var (
	// ErrHarnessUnavailable means no host can drive the run: no model is configured,
	// or sidecar mode was requested without a healthy sidecar (§3.2).
	ErrHarnessUnavailable = errors.New("agent harness is unavailable")
	// ErrToolsETagStale means the tool manifest changed under the host (§10.3).
	ErrToolsETagStale = errors.New("tool manifest changed since the host cached it")
	// ErrHarnessToken covers an unknown, expired, revoked or run-mismatched token
	// (§10.2). It is deliberately one error: the holder learns nothing.
	ErrHarnessToken = errors.New("harness token is not valid for this run")
	// ErrRunNotActive means the run already reached a terminal state.
	ErrRunNotActive = errors.New("agent run is not active")
	// ErrApprovalTimeout is returned to a host whose long poll outlived the wait
	// (§7); the run is failed with the same code.
	ErrApprovalTimeout = errors.New("approval wait timed out")
)

// ModelCredentials are the upstream coordinates the model proxy injects. They never
// leave the Go process (§4.5): a host authenticates with a harness token and sends a
// placeholder key, and the response never echoes the real one.
type ModelCredentials struct {
	BaseURL string
	Model   string
	APIKey  string
}

// CredentialsResolver resolves the model a run may use. It returns (nil, nil) when
// no model is configured, which makes the harness unavailable rather than silently
// degrading to a single-turn reply (§3.2).
type CredentialsResolver func(ctx context.Context) (*ModelCredentials, error)

// HarnessPrincipal is what a harness token resolves to. It is the identity for every
// harness endpoint: UserID comes from here and never from a request body
// (agent.md §4 invariant 5).
type HarnessPrincipal struct {
	TokenID  string
	UserID   string
	ThreadID string
	RunID    string
	Mode     string
}

// RunGrant is the response of POST /agent/runs (§10.2). Model and instructions come
// from the server; a host must not carry defaults of its own (§3.3).
type RunGrant struct {
	HarnessToken       string    `json:"harness_token"`
	Model              string    `json:"model"`
	Instructions       string    `json:"instructions"`
	ToolsETag          string    `json:"tools_etag"`
	ExpiresAt          time.Time `json:"expires_at"`
	HeartbeatIntervalS int       `json:"heartbeat_interval_s"`
	LibfxVersion       string    `json:"libfx_version"`
	Reasoning          string    `json:"reasoning"`
	// ContextDegraded reports that the thread had no usable checkpoint, so the
	// instructions carry a rebuilt summary instead (§6.3). The UI owes the user a
	// one-time notice; this is the only signal it gets.
	ContextDegraded bool `json:"context_degraded"`
}

// HeartbeatReply is the response of POST /agent/runs/{id}/heartbeat (§10.4). The
// cancellation flag rides along because a heartbeat is the one channel that reaches
// an idle host (§5.2).
type HeartbeatReply struct {
	CancelRequested bool      `json:"cancel_requested"`
	RunStatus       string    `json:"run_status"`
	ExpiresAt       time.Time `json:"expires_at"`
	// HarnessToken is present only when the current one is close to expiring; the
	// host replaces its copy atomically (§10.4).
	HarnessToken string `json:"harness_token,omitempty"`
}

// ToolDescriptor is one entry of GET /agent/tools (§3.4). The registry is the single
// source of truth: a host declares no tool of its own.
type ToolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// harnessService is the container AgentService embeds. It declares no methods of its
// own: the harness surface is split into three cohesive units so none of them crosses
// the wiring.md §9 budget of 15 methods per struct, and AgentService still resolves
// every entry point by promotion.
type harnessService struct {
	*harnessTokens
	*harnessManifest
	*harnessLifecycle
	*harnessTools
	*harnessProxy
}

// newHarnessService builds the three units over one AgentService. They share the same
// repository, clock and configuration because they are one subsystem seen from three
// angles: capability, advertisement and lifecycle.
func newHarnessService(svc *AgentService) *harnessService {
	base := harnessBase{svc: svc}
	return &harnessService{
		harnessTokens:    &harnessTokens{harnessBase: base},
		harnessManifest:  &harnessManifest{harnessBase: base},
		harnessLifecycle: &harnessLifecycle{harnessBase: base},
		harnessTools:     &harnessTools{harnessBase: base},
		harnessProxy:     &harnessProxy{harnessBase: base},
	}
}

// harnessBase carries what every unit needs. The collaborators are reached through the
// owning AgentService rather than copied, because options install the system prompt,
// the loop limits and the credentials resolver after construction.
type harnessBase struct {
	svc *AgentService
}

// repo is the user-scoped agent repository.
func (b harnessBase) repo() *persistence.AgentRepository { return b.svc.repo }

// clock is the injectable current time, so the heartbeat-loss and cancellation-grace
// paths can be tested without sleeping.
func (b harnessBase) clock() time.Time {
	if b.svc.nowFn != nil {
		return b.svc.nowFn()
	}
	return persistence.Now()
}

// lifecycle reaches the unit that owns run teardown. The tool surface needs it to fail a
// run whose approval wait expired, and going through the container keeps the dependency
// explicit instead of relying on promotion.
func (b harnessBase) lifecycle() *harnessLifecycle { return b.svc.harnessService.harnessLifecycle }

// harnessTokens owns the run capability: issuance, validation, heartbeat and rotation
// (doc/harness.md §10).
type harnessTokens struct{ harnessBase }

// harnessManifest owns the tool projection a host is allowed to call (§3.4, §10.3).
type harnessManifest struct{ harnessBase }

// harnessLifecycle owns what happens to a run once a host is driving it: cancellation,
// completion, checkpoint storage and the reaper (§5.1, §5.2, §6, §11).
type harnessLifecycle struct{ harnessBase }

// --- tool manifest (§3.4, §10.3) ---

// ToolManifest projects the registry onto the wire. The order is the registry's, so
// a host sees exactly what the server will validate against.
func (s *harnessManifest) ToolManifest() []ToolDescriptor {
	defs := s.svc.tools.Definitions(ToolReadonly, ToolProposal)
	out := make([]ToolDescriptor, 0, len(defs))
	for _, def := range defs {
		schema := def.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, ToolDescriptor{Name: def.Name, Description: def.Description, InputSchema: schema})
	}
	return out
}

// ToolsETag is the stable hash of the manifest: names, descriptions and schemas in
// registry order (§10.3). It changes exactly when a host's cached manifest stops
// describing what the server will accept.
func (s *harnessManifest) ToolsETag() string {
	digest := sha256.New()
	for _, tool := range s.ToolManifest() {
		encoded, err := json.Marshal(tool)
		if err != nil {
			// A manifest that cannot be encoded cannot be advertised either; hash the
			// name so the etag still moves when the set changes.
			encoded = []byte(tool.Name)
		}
		digest.Write(encoded)
		digest.Write([]byte{'\n'})
	}
	return `"` + hex.EncodeToString(digest.Sum(nil)) + `"`
}

// ToolManifestWarning reports a manifest that is close to libfx's 64-tool ceiling
// (§3.4). It is a log line, not an error: the limit is the harness's, and crossing
// it must not break the API.
func (s *harnessManifest) ToolManifestWarning() string {
	count := len(s.svc.tools.Names())
	if count <= toolManifestWarnAt {
		return ""
	}
	return fmt.Sprintf("agent tool manifest holds %d tools; libfx accepts at most 64", count)
}

// IssueRunGrant mints the capability that lets a host drive one run. It fails closed:
// a run that is terminal, a run the in-process loop owns, a missing model and a stale
// tool manifest each refuse the grant rather than issuing something unusable.
func (s *harnessTokens) IssueRunGrant(ctx context.Context, userID, runID, toolsETag string) (RunGrant, error) {
	run, err := s.repo().GetRun(ctx, userID, runID)
	if err != nil {
		return RunGrant{}, err
	}
	if IsTerminalRunStatus(run.Status) {
		return RunGrant{}, ErrRunNotActive
	}
	if run.HarnessMode != HarnessModeWASM && run.HarnessMode != HarnessModeSidecar {
		// Nothing to drive: this run belongs to the Worker's in-process path.
		return RunGrant{}, ErrHarnessUnavailable
	}
	creds, err := s.svc.credentials(ctx)
	if err != nil {
		return RunGrant{}, err
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" || creds.Model == "" {
		return RunGrant{}, ErrHarnessUnavailable
	}
	current := s.svc.ToolsETag()
	if trimmed := strings.TrimSpace(toolsETag); trimmed != "" && trimmed != current {
		return RunGrant{}, ErrToolsETagStale
	}

	// One authorized host per run: a re-issue (a reload, a second tab) revokes the
	// previous token so a lost one stops working.
	if _, err := s.repo().RevokeHarnessTokensForRun(ctx, run.ID); err != nil {
		return RunGrant{}, err
	}
	plaintext, err := newHarnessToken()
	if err != nil {
		return RunGrant{}, err
	}
	now := s.clock()
	expires := now.Add(harnessTokenTTL)
	row := &persistence.AgentHarnessToken{
		TokenHash: persistence.Hash(plaintext), UserID: userID, ThreadID: run.ThreadID,
		RunID: run.ID, Mode: run.HarnessMode, ToolsETag: current, ExpiresAt: expires, CreatedAt: now,
	}
	if err := s.repo().CreateHarnessToken(ctx, row); err != nil {
		return RunGrant{}, err
	}
	// A host has arrived: the run is being driven now.
	if run.Status == persistence.RunQueued {
		if err := s.repo().SetRunStatus(ctx, userID, run.ID, persistence.RunRunning, "", ""); err != nil {
			return RunGrant{}, err
		}
	}

	instructions, degraded, err := s.instructionsFor(ctx, run)
	if err != nil {
		return RunGrant{}, err
	}
	return RunGrant{
		HarnessToken: plaintext, Model: creds.Model, Instructions: instructions,
		ToolsETag: current, ExpiresAt: expires,
		HeartbeatIntervalS: int(harnessHeartbeatInterval / time.Second),
		LibfxVersion:       LibfxVersion,
		Reasoning:          s.svc.reasoningLevel,
		ContextDegraded:    degraded,
	}, nil
}

// newHarnessToken returns an opaque 256-bit token. It shares no code with the
// run_token of doc/interface.md §13, which is a Job lease (§10.1).
func newHarnessToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "fth_" + hex.EncodeToString(buf), nil
}

// instructionsFor returns the system prompt a host must pass to libfx, plus whether
// the degraded path was taken. libfx adds no hidden base prompt (§3.5), so this text
// is the whole system context the model sees.
//
// A thread whose checkpoint is missing or was written by another libfx version cannot
// restore its history, so the authoritative parts are summarized into the prompt
// instead (§6.3). That is lossy and the caller must tell the user.
func (s *harnessTokens) instructionsFor(ctx context.Context, run *persistence.AgentRun) (string, bool, error) {
	base := s.svc.systemPrompt
	thread, err := s.repo().GetThread(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return "", false, err
	}
	if len(thread.Checkpoint) > 0 && thread.LibfxVersion == LibfxVersion {
		return truncateInstructions(base), false, nil
	}
	summary := s.rebuildSummary(ctx, run)
	if strings.TrimSpace(summary) == "" {
		return truncateInstructions(base), len(thread.Checkpoint) > 0, nil
	}
	// CHECKPOINT_VERSION_SKEW is the observable marker for this path (§6.3).
	return truncateInstructions(base + "\n\n" + summary), true, nil
}

// rebuildSummary renders the thread's authoritative transcript into the compact
// history the degraded path appends to the instructions (§6.3). It reuses
// rebuildModelContext's trimming rather than re-deriving one, and keeps the most
// recent turns plus whatever is still awaiting approval.
func (s *harnessTokens) rebuildSummary(ctx context.Context, run *persistence.AgentRun) string {
	messages, err := s.repo().ListThreadMessages(ctx, run.UserID, run.ThreadID)
	if err != nil || len(messages) == 0 {
		return ""
	}
	wire := make([]protocol.Message, 0, len(messages))
	assistantIdx := -1
	for i, m := range messages {
		pm, err := s.svc.toProtocolMessage(ctx, run.UserID, m, protocol.CompleteStatus(""))
		if err != nil {
			return ""
		}
		if m.ID == derefString(run.ParentMessageID) {
			// The run's own user message is the last turn; everything up to it is history.
			assistantIdx = i
		}
		wire = append(wire, pm)
	}
	if assistantIdx < 0 {
		assistantIdx = len(wire) - 1
	}
	history := s.svc.rebuildModelContext(ctx, run.UserID, wire, assistantIdx)
	// Keep the tail: the oldest turns are the ones a summary may lose.
	if len(history) > 12 {
		history = history[len(history)-12:]
	}
	var builder strings.Builder
	builder.WriteString("【历史上下文摘要（本线程的 libfx checkpoint 不可用，早期对话已压缩）】")
	for _, m := range history {
		switch m.Role {
		case "user":
			builder.WriteString("\n用户：" + m.Content)
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				builder.WriteString("\n助手：" + m.Content)
			}
			for _, call := range m.ToolCalls {
				builder.WriteString(fmt.Sprintf("\n助手调用工具 %s(%s)", call.Name, call.Arguments))
			}
		case "tool":
			builder.WriteString(fmt.Sprintf("\n工具 %s 返回：%s", m.Name, m.ToolCallID))
		}
	}
	if pending := s.svc.pendingProposals(ctx, run.UserID, run.ThreadID); len(pending) > 0 {
		builder.WriteString("\n待用户确认的提案：")
		for _, p := range pending {
			builder.WriteString(fmt.Sprintf("\n- %s（%s）", p.Summary, p.ID))
		}
	}
	return builder.String()
}

// truncateInstructions enforces libfx's 64 KiB prompt cap on a rune boundary (§3.5).
// The check is server-side on purpose: createFxAgent must never be the one to throw.
func truncateInstructions(text string) string {
	if len(text) <= InstructionsLimit {
		return text
	}
	runes := []rune(text)
	for len(string(runes)) > InstructionsLimit {
		runes = runes[:len(runes)-128]
	}
	return string(runes) + "\n【系统提示已按 64 KiB 上限截断】"
}

// --- token validation (§10.2) ---

// AuthenticateHarness resolves an Authorization header to a principal. Every harness
// endpoint calls it and nothing else: a user JWT must not work here and a harness
// token must not work anywhere else (§10.2, §14.5, §14.16).
func (s *harnessTokens) AuthenticateHarness(ctx context.Context, authorization string) (HarnessPrincipal, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(strings.TrimSpace(authorization), prefix) {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	plaintext := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(authorization), prefix))
	if plaintext == "" {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	token, err := s.repo().GetHarnessTokenByHash(ctx, persistence.Hash(plaintext))
	if err != nil {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	if token.RevokedAt != nil || !token.ExpiresAt.After(s.clock()) {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	// A token dies with its run (§10.2). Checking the run rather than trusting the
	// row is what makes a terminal run's token useless even before revocation lands.
	run, err := s.repo().GetRun(ctx, token.UserID, token.RunID)
	if err != nil {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	if IsTerminalRunStatus(run.Status) {
		return HarnessPrincipal{}, ErrHarnessToken
	}
	return HarnessPrincipal{
		TokenID: token.ID, UserID: token.UserID, ThreadID: token.ThreadID,
		RunID: token.RunID, Mode: token.Mode,
	}, nil
}

// IsTerminalRunStatus reports whether a run status ends the run (§4). awaiting_approval
// and cancelling are intermediate: the run still owns the thread.
func IsTerminalRunStatus(status string) bool {
	switch status {
	case persistence.RunSucceeded, persistence.RunFailed, persistence.RunCancelled, persistence.RunInterrupted:
		return true
	default:
		return false
	}
}

// --- heartbeat (§10.4) ---

// HarnessHeartbeat records that a host is alive and hands back the cancellation flag.
// A token close to expiring is replaced in the same response, so a long approval wait
// never outlives its credential.
func (s *harnessTokens) HarnessHeartbeat(ctx context.Context, p HarnessPrincipal) (HeartbeatReply, error) {
	now := s.clock()
	run, err := s.repo().GetRun(ctx, p.UserID, p.RunID)
	if err != nil {
		return HeartbeatReply{}, err
	}
	ok, err := s.repo().BeatHarnessToken(ctx, p.TokenID, now)
	if err != nil {
		return HeartbeatReply{}, err
	}
	if !ok {
		// The token died while the host was away. Tell it so through the same error the
		// other endpoints use; the run itself is left to the reaper.
		return HeartbeatReply{}, ErrHarnessToken
	}
	token, err := s.repo().GetHarnessToken(ctx, p.TokenID)
	if err != nil {
		return HeartbeatReply{}, err
	}
	reply := HeartbeatReply{CancelRequested: run.CancelRequested, RunStatus: run.Status, ExpiresAt: token.ExpiresAt}
	// Rotation, not extension (§10.4): a token inside its last ten minutes is replaced
	// so a long approval wait never outlives its credential, and a leaked token keeps
	// dying on the schedule it was issued with.
	if token.ExpiresAt.Sub(now) < harnessRenewBelow {
		renewed, expires, err := s.renewToken(ctx, p, now)
		if err != nil {
			return HeartbeatReply{}, err
		}
		if renewed != "" {
			reply.HarnessToken, reply.ExpiresAt = renewed, expires
		}
	}
	return reply, nil
}

// renewToken replaces a token that is inside its renewal window (§10.4). The old one
// is revoked in the same breath, so a run has at most one usable token at any instant.
func (s *harnessTokens) renewToken(ctx context.Context, p HarnessPrincipal, now time.Time) (string, time.Time, error) {
	plaintext, err := newHarnessToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(harnessTokenTTL)
	row := &persistence.AgentHarnessToken{
		TokenHash: persistence.Hash(plaintext), UserID: p.UserID, ThreadID: p.ThreadID,
		RunID: p.RunID, Mode: p.Mode, ToolsETag: s.svc.ToolsETag(), ExpiresAt: expires, CreatedAt: now,
	}
	err = s.svc.store.Transaction(ctx, func(tx *gorm.DB) error {
		repo := s.repo().WithTx(tx)
		if err := repo.CreateHarnessToken(ctx, row); err != nil {
			return err
		}
		return repo.RevokeHarnessToken(ctx, p.TokenID, now)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return plaintext, expires, nil
}
