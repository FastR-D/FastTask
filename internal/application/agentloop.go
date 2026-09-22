package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// defaultSystemPrompt is the server-side system prompt (agent-impl.md §6: the
// client-supplied system field is always ignored, §2.2). It encodes the product
// invariants from agent.md §4 and §6 so the model cannot argue its way past the
// deterministic guarantees the program enforces.
const defaultSystemPrompt = `你是 FastTask 的科研推进助手，服务于没有固定课表、需要自行安排一周时间的研究生。

核心产品判断（不可违背）：
1. 你绝不直接修改业务数据。任务树、坐标、每日计划的结构变更只能通过提案工具产出 Proposal，由用户确认后由系统落库。不要声称"已经修改/已经创建"，只能说"已提交待确认的提案"。
2. 每日核心推进项最多三个，且这个上限由程序强制，不由你强制。你可以推荐候选并说明理由，但最终取几个、取哪几个由确定性规则决定。
3. 候选不足时不要凑数。三个是注意力上限，不是必须填满的配额；没有合适的候选就如实说明，不要编造占位任务。
4. "当日满足"不等于"底层任务完成"。完成最小行动/一个步骤/约定专注时间只让当日计划项 satisfied，底层 Task 只有提交最终结果证据后才 completed。
5. 每个推荐的候选都必须带一个 5–15 分钟内就能开始、与目标直接相关的最小行动；缺失则该候选无效。

工作方式：
- 先用只读工具读取上下文（活跃目标、任务树与 revision、当日计划、近期进度证据、周复盘、长期无进展任务、任务坐标），再给建议。不要凭空假设用户的数据。
- 跨用户数据不可达；身份来自认证上下文，绝不在工具参数里出现 user_id 之类的身份字段。
- 用中文、简洁作答。先识别进展或阻碍，再给一个马上能开始的最小行动。

需要结构变更时调用提案工具，并等待用户审批；审批前不要声称变更已生效。`

// sessionStream carries the streaming + persistence state for one run execution:
// the chunk sink, the in-memory protocol.State (saved as the resume snapshot), and
// the assistant message/part cursor — the server is the sole allocator of
// message/part indices (agent-impl.md §2.7.1). Its methods emit chunks and persist
// parts. runSession embeds it and adds the run-lifecycle transitions, keeping each
// type cohesive and within the wiring.md §9 method budget.
type sessionStream struct {
	svc          *AgentService
	ctx          context.Context
	userID       string
	run          *persistence.AgentRun
	sink         *dbSink
	state        protocol.State
	assistantID  string
	assistantIdx int
}

// runSession is one run execution: the embedded sessionStream (emit/state/persist) plus the
// run-lifecycle fields and the terminal transitions (succeed/fail/cancel).
//
// Under the harness a session is a view rather than a loop iteration: it is rebuilt from the
// authoritative rows whenever the proxy or the tool surface has to write something
// (doc/harness.md §4.4). jobID survives for the runs the Worker still executes.
type runSession struct {
	*sessionStream
	jobID    string
	userText string
	turns    int
}

// beginRun sets up the run session: emit isRunning + threadId, replay existing
// thread messages into authoritative state (§2.6), and create the assistant
// message shell that this run streams into (§2.7.1).
func (s *AgentService) beginRun(ctx context.Context, job persistence.AgentJob, run *persistence.AgentRun) (*runSession, error) {
	sink := &dbSink{repo: s.repo, userID: run.UserID, run: run.ID}
	sess := &runSession{
		sessionStream: &sessionStream{svc: s, ctx: ctx, userID: run.UserID, run: run, sink: sink, state: protocol.NewState()},
		jobID:         job.ID,
	}
	sess.state.IsRunning = true
	sess.state.FastTask.ThreadID = run.ThreadID

	if err := sess.emitSet(protocol.IsRunningPath(), true); err != nil {
		return nil, err
	}
	if err := sess.emitSet(protocol.FastTaskThreadIDPath(), run.ThreadID); err != nil {
		return nil, err
	}

	existing, err := s.repo.ListThreadMessages(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return nil, err
	}
	for i, m := range existing {
		pm, err := s.toProtocolMessage(ctx, run.UserID, m, protocol.CompleteStatus(""))
		if err != nil {
			return nil, err
		}
		sess.state.Messages = append(sess.state.Messages, pm)
		if err := sess.emitSet(protocol.MessagePath(i), pm); err != nil {
			return nil, err
		}
	}
	sess.userText = lastUserText(sess.state.Messages)

	assistant := &persistence.AgentMessage{UserID: run.UserID, ThreadID: run.ThreadID, RunID: run.ID, Role: "assistant"}
	if err := s.repo.CreateMessage(ctx, assistant); err != nil {
		return nil, err
	}
	sess.assistantID = assistant.ID
	sess.assistantIdx = assistant.Seq - 1

	shell := protocol.Message{
		ID: assistant.ID, Role: protocol.RoleAssistant, Parts: []protocol.Part{},
		CreatedAt: protocol.RFC3339(assistant.CreatedAt), Status: protocol.RunningStatus(),
	}
	sess.state.Messages = append(sess.state.Messages, shell)
	if err := sess.emitSet(protocol.MessagePath(sess.assistantIdx), shell); err != nil {
		return nil, err
	}
	return sess, nil
}

// --- session emit + state helpers ---

func (s *sessionStream) emitSet(path []string, value any) error {
	op, err := protocol.Set(path, value)
	if err != nil {
		return err
	}
	return s.sink.Emit(s.ctx, protocol.UpdateState(op))
}

func (s *sessionStream) assistantParts() []protocol.Part {
	return s.state.Messages[s.assistantIdx].Parts
}

// addTextPart appends an empty text part to the assistant message, emitting the
// establishing set required before any append-text (§2.6, §2.7.1).
func (s *sessionStream) addTextPart() (int, error) {
	idx := len(s.assistantParts())
	empty := protocol.TextPart("")
	s.state.Messages[s.assistantIdx].Parts = append(s.assistantParts(), empty)
	if err := s.emitSet(protocol.PartPath(s.assistantIdx, idx), empty); err != nil {
		return 0, err
	}
	return idx, nil
}

// appendTextToPart streams a delta into a text part via append-text and mirrors
// it into the in-memory state.
func (s *sessionStream) appendTextToPart(idx int, delta string) error {
	s.state.Messages[s.assistantIdx].Parts[idx].Text += delta
	op, err := protocol.AppendText(protocol.PartTextPath(s.assistantIdx, idx), delta)
	if err != nil {
		return err
	}
	return s.sink.Emit(s.ctx, protocol.UpdateState(op))
}

// addToolCallPart appends a tool-call part with the model's arguments.
func (s *sessionStream) addToolCallPart(call agent.ToolCall, args map[string]any) (int, error) {
	idx := len(s.assistantParts())
	part := protocol.ToolCallPart(call.ID, call.Name, args)
	s.state.Messages[s.assistantIdx].Parts = append(s.assistantParts(), part)
	if err := s.emitSet(protocol.PartPath(s.assistantIdx, idx), part); err != nil {
		return 0, err
	}
	return idx, nil
}

// setToolCallResult records a tool result on an existing tool-call part.
func (s *sessionStream) setToolCallResult(idx int, result ToolResult) error {
	part := &s.state.Messages[s.assistantIdx].Parts[idx]
	part.IsError = result.IsError
	if result.IsError {
		part.Result = map[string]any{"error": result.Text}
	} else {
		part.Result = result.Result
	}
	return s.emitSet(protocol.PartPath(s.assistantIdx, idx), *part)
}

// persistTextPart writes a finalized text part to the durable message log.
func (s *sessionStream) persistTextPart(idx int, text string) error {
	return s.svc.repo.CreatePart(s.ctx, &persistence.AgentMessagePart{
		UserID: s.userID, MessageID: s.assistantID, Idx: idx, Type: "text", Text: text, ArgsJSON: "{}",
	})
}

// persistToolCallPart writes a finalized tool-call part with its result.
func (s *sessionStream) persistToolCallPart(idx int, call agent.ToolCall, result ToolResult) error {
	resultJSON, isError := resultPayload(result)
	part := &persistence.AgentMessagePart{
		UserID: s.userID, MessageID: s.assistantID, Idx: idx, Type: "tool-call",
		ToolCallID: &call.ID, ToolName: call.Name, ArgsJSON: normalizeArgs(call.Arguments),
		ResultJSON: resultJSON, IsError: isError,
	}
	return s.svc.repo.CreatePart(s.ctx, part)
}

func (s *sessionStream) saveState() error {
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	// checkpoint_seq counts the chunks folded into state_json so a resume replays
	// only seq >= checkpoint and never re-applies append-text (§2.8).
	return s.svc.repo.SaveRunState(s.ctx, s.userID, s.run.ID, string(encoded), s.sink.checkpoint())
}

func textOfParts(parts []protocol.Part) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == protocol.PartText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// interruptRun ends a run whose driver disappeared (doc/harness.md §11). It is failRun's shape with
// interrupted's meaning: the partial transcript stays readable and any pending proposal stays
// pending, because the user — not the host — decides what happens next.
func (s *runSession) interruptRun(code, message string) (map[string]any, error) {
	incomplete := protocol.IncompleteStatus(protocol.ReasonError)
	s.state.Messages[s.assistantIdx].Status = incomplete
	_ = s.emitSet(protocol.MessageStatusPath(s.assistantIdx), incomplete)
	s.state.IsRunning = false
	_ = s.emitSet(protocol.IsRunningPath(), false)
	_ = s.saveState()
	_ = s.svc.repo.SetRunStatus(s.ctx, s.userID, s.run.ID, persistence.RunInterrupted, code, message)
	return map[string]any{"run_id": s.run.ID, "status": persistence.RunInterrupted, "chunks": s.sink.emitted}, nil
}

// --- what the harness replaced ---
//
// The multi-turn loop that used to live here (toolLoop, executeToolCall, the turnSink that
// streamed one turn's text, recordProposal and saveAwaitingApproval) is gone, and so is
// resumeRun: doc/harness.md §12 hands the loop to libfx, and a resume is now pure chunk
// replay rather than a rebuilt model context. What survives is everything that writes
// authoritative state — the session, its emit helpers and the terminal transitions — because
// the model proxy (harnessproxy.go) and the tool surface (harnesstools.go) both need it.

// --- terminal transitions ---

func (s *runSession) succeed() (map[string]any, error) {
	complete := protocol.CompleteStatus(protocol.ReasonStop)
	s.state.Messages[s.assistantIdx].Status = complete
	_ = s.emitSet(protocol.MessageStatusPath(s.assistantIdx), complete)
	s.state.IsRunning = false
	_ = s.emitSet(protocol.IsRunningPath(), false)
	if err := s.saveState(); err != nil {
		return nil, err
	}
	if err := s.svc.repo.SetRunStatus(s.ctx, s.userID, s.run.ID, persistence.RunSucceeded, "", ""); err != nil {
		return nil, err
	}
	return map[string]any{"run_id": s.run.ID, "status": persistence.RunSucceeded, "chunks": s.sink.emitted, "turns": s.turns}, nil
}

func (s *runSession) failRun(code string, cause error) (map[string]any, error) {
	if cause == nil {
		cause = errors.New(code)
	}
	incomplete := protocol.IncompleteStatus(protocol.ReasonError)
	s.state.Messages[s.assistantIdx].Status = incomplete
	_ = s.emitSet(protocol.MessageStatusPath(s.assistantIdx), incomplete)
	s.state.IsRunning = false
	_ = s.emitSet(protocol.IsRunningPath(), false)
	_ = s.saveState()
	_ = s.svc.repo.SetRunStatus(s.ctx, s.userID, s.run.ID, persistence.RunFailed, code, cause.Error())
	return nil, cause
}

func (s *runSession) cancelRun() (map[string]any, error) {
	incomplete := protocol.IncompleteStatus(protocol.ReasonCancelled)
	s.state.Messages[s.assistantIdx].Status = incomplete
	_ = s.emitSet(protocol.MessageStatusPath(s.assistantIdx), incomplete)
	s.state.IsRunning = false
	_ = s.emitSet(protocol.IsRunningPath(), false)
	_ = s.saveState()
	_ = s.svc.repo.SetRunStatus(s.ctx, s.userID, s.run.ID, persistence.RunCancelled, "CANCELLED", "cancelled by user")
	return map[string]any{"run_id": s.run.ID, "status": persistence.RunCancelled, "chunks": s.sink.emitted}, nil
}

// --- deterministic fallback (no model configured) ---

// deterministicReply streams a labelled fallback reply when no ChatProvider is
// configured, preserving the README's "LLM 未配置时使用确定性本地 Provider"
// behaviour. It is clearly marked so it is never mistaken for a model answer.
func (s *AgentService) deterministicReply(sess *runSession) (map[string]any, error) {
	idx, err := sess.addTextPart()
	if err != nil {
		return sess.failRun("STATE_ERROR", err)
	}
	reply := echoReply(sess.userText)
	for _, delta := range splitDeltas(reply, 12) {
		if err := sess.appendTextToPart(idx, delta); err != nil {
			return sess.failRun("STATE_ERROR", err)
		}
	}
	if err := sess.persistTextPart(idx, reply); err != nil {
		return sess.failRun("STATE_ERROR", err)
	}
	return sess.succeed()
}

// pendingProposals lists proposals still awaiting approval across the thread's runs, so a
// session's fasttask state reflects only what is truly pending.
func (s *AgentService) pendingProposals(ctx context.Context, userID, threadID string) []protocol.PendingProposal {
	out := []protocol.PendingProposal{}
	var proposals []persistence.Proposal
	s.store.DB.WithContext(ctx).
		Where("user_id = ? AND status = 'pending'", userID).
		Order("created_at DESC").Limit(20).Find(&proposals)
	for _, p := range proposals {
		out = append(out, protocol.PendingProposal{ID: p.ID, GoalID: p.GoalID, BaseRevision: p.BaseRevision, Summary: p.Instruction})
	}
	_ = threadID
	return out
}

// rebuildModelContext converts the wire messages up to a given index into model turns: text
// parts become content, and each tool-call part with a recorded result becomes an assistant
// tool_call plus a tool message.
//
// It used to rebuild the context of a run resuming after approval. Under the harness the
// checkpoint carries history instead, so this became the DEGRADED path: it runs when a thread
// has no usable checkpoint and its transcript has to be summarized into the instructions
// (doc/harness.md §6.3).
func (s *AgentService) rebuildModelContext(ctx context.Context, userID string, messages []protocol.Message, assistantIdx int) []agent.ChatMessage {
	out := make([]agent.ChatMessage, 0, len(messages)+2)
	for i := 0; i <= assistantIdx && i < len(messages); i++ {
		m := messages[i]
		switch m.Role {
		case protocol.RoleUser:
			if text := strings.TrimSpace(textOfParts(m.Parts)); text != "" {
				out = append(out, agent.ChatMessage{Role: "user", Content: text})
			}
		case protocol.RoleAssistant:
			var content strings.Builder
			var calls []agent.ToolCall
			var results []agent.ChatMessage
			for _, part := range m.Parts {
				switch part.Type {
				case protocol.PartText:
					content.WriteString(part.Text)
				case protocol.PartToolCall:
					argsJSON := "{}"
					if part.Args != nil {
						if encoded, err := json.Marshal(part.Args); err == nil {
							argsJSON = string(encoded)
						}
					}
					calls = append(calls, agent.ToolCall{ID: part.ToolCallID, Name: part.ToolName, Arguments: argsJSON})
					resultContent := "{}"
					if part.Result != nil {
						if encoded, err := json.Marshal(part.Result); err == nil {
							resultContent = string(encoded)
						}
					}
					results = append(results, agent.ChatMessage{Role: "tool", ToolCallID: part.ToolCallID, Name: part.ToolName, Content: resultContent})
				}
			}
			if content.Len() > 0 || len(calls) > 0 {
				out = append(out, agent.ChatMessage{Role: "assistant", Content: content.String(), ToolCalls: calls})
				out = append(out, results...)
			}
		}
	}
	_ = ctx
	_ = userID
	return out
}

// --- small helpers ---

func parseArgs(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		return nil
	}
	return args
}

func normalizeArgs(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

// resultPayload renders a tool result for durable storage, mirroring the wire
// part shape: an error becomes {"error": ...} with isError true.
func resultPayload(result ToolResult) (string, bool) {
	if result.IsError {
		encoded, _ := json.Marshal(map[string]any{"error": result.Text})
		return string(encoded), true
	}
	encoded, err := json.Marshal(result.Result)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{"error": "unserializable result"})
		return string(fallback), true
	}
	return string(encoded), false
}

// resultContent is the string fed back to the model as the tool message content.
func resultContent(result ToolResult) string {
	if result.IsError {
		encoded, _ := json.Marshal(map[string]any{"error": result.Text})
		return string(encoded)
	}
	encoded, err := json.Marshal(result.Result)
	if err != nil {
		return `{"error":"unserializable result"}`
	}
	return string(encoded)
}
