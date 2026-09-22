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

// runSession is one run execution: the embedded sessionStream (emit/state/persist)
// plus the run-lifecycle fields (job id, turn count, resume context) and the
// terminal transitions (succeed/fail/cancel/awaiting-approval).
type runSession struct {
	*sessionStream
	jobID    string
	userText string
	turns    int
	// resumed marks a run continuing after an approval receipt (§7.2); it reuses
	// the existing assistant message instead of creating one. resumeContext is the
	// model conversation reconstructed from the persisted parts of that message.
	resumed       bool
	resumeContext []agent.ChatMessage
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

func (s *runSession) jobCancelRequested() (bool, error) {
	if s.jobID == "" {
		return false, nil
	}
	var job persistence.AgentJob
	err := s.svc.store.DB.WithContext(s.ctx).Select("cancel_requested").Where("id = ?", s.jobID).First(&job).Error
	if err != nil {
		if persistence.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return job.CancelRequested, nil
}

// initialMessages builds the model conversation: the server system prompt plus
// the replayed thread history (which ends with the current user message).
func (s *runSession) initialMessages(systemPrompt string) []agent.ChatMessage {
	messages := []agent.ChatMessage{{Role: "system", Content: systemPrompt}}
	for i := 0; i < s.assistantIdx; i++ {
		m := s.state.Messages[i]
		role := string(m.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(textOfParts(m.Parts))
		if text == "" {
			continue
		}
		messages = append(messages, agent.ChatMessage{Role: role, Content: text})
	}
	return messages
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

// --- the tool-calling loop (agent-impl.md §6) ---

// turnSink streams one model turn's text into a lazily-created text part.
type turnSink struct {
	sess    *runSession
	partIdx int
	text    strings.Builder
	err     error
}

func (t *turnSink) TextDelta(_ context.Context, delta string) error {
	if delta == "" {
		return nil
	}
	if t.partIdx < 0 {
		idx, err := t.sess.addTextPart()
		if err != nil {
			t.err = err
			return err
		}
		t.partIdx = idx
	}
	if err := t.sess.appendTextToPart(t.partIdx, delta); err != nil {
		t.err = err
		return err
	}
	t.text.WriteString(delta)
	return nil
}

// toolLoop drives the multi-turn tool-calling loop (§6). Readonly tool calls are
// executed and fed back; a turn with no tool calls ends the run. A proposal tool
// call pauses the run at awaiting_approval (§7); a resumed run continues from
// the reconstructed context with the approval outcome already fed back.
func (s *AgentService) toolLoop(sess *runSession, provider agent.ChatProvider) (map[string]any, error) {
	limits := s.limits
	if limits.MaxTurns <= 0 {
		limits = DefaultLoopLimits()
	}
	deadline := time.Now().Add(limits.WallClock)
	// A fresh run starts from the system prompt + thread history; a resumed run
	// starts from the reconstructed context that already includes the resolved
	// proposal tool call and its result (§7.2).
	messages := sess.initialMessages(s.systemPrompt)
	if sess.resumed {
		messages = append([]agent.ChatMessage{{Role: "system", Content: s.systemPrompt}}, sess.resumeContext...)
	}
	tools := s.tools.Definitions(ToolReadonly, ToolProposal)
	tc := ToolContext{UserID: sess.userID, ThreadID: sess.run.ThreadID, RunID: sess.run.ID}

	for turn := 0; turn < limits.MaxTurns; turn++ {
		sess.turns = turn + 1

		if time.Now().After(deadline) {
			return sess.failRun("RUN_TIMEOUT", errors.New("run wall clock exceeded"))
		}
		if cancelled, err := sess.jobCancelRequested(); err == nil && cancelled {
			return sess.cancelRun()
		}

		turnCtx, cancelTurn := context.WithTimeout(sess.ctx, time.Until(deadline))
		sink := &turnSink{sess: sess, partIdx: -1}
		result, err := provider.Chat(turnCtx, agent.ChatRequest{Messages: messages, Tools: tools, MaxTokens: limits.MaxOutputTokens}, sink)
		cancelTurn()

		if err != nil {
			switch {
			case errors.Is(err, agent.ErrNoToolSupport):
				return sess.failRun("PROVIDER_NO_TOOL_SUPPORT", err)
			case time.Now().After(deadline):
				return sess.failRun("RUN_TIMEOUT", err)
			default:
				return sess.failRun("PROVIDER_ERROR", err)
			}
		}
		if sink.err != nil {
			return sess.failRun("STATE_ERROR", sink.err)
		}
		if sink.partIdx >= 0 {
			if err := sess.persistTextPart(sink.partIdx, sink.text.String()); err != nil {
				return sess.failRun("STATE_ERROR", err)
			}
		}

		if len(result.ToolCalls) == 0 {
			return sess.succeed()
		}

		messages = append(messages, agent.ChatMessage{Role: "assistant", Content: result.Text, ToolCalls: result.ToolCalls})

		for i, call := range result.ToolCalls {
			if strings.TrimSpace(call.ID) == "" {
				call.ID = fmt.Sprintf("call_%s_t%d_%d", sess.run.ID, turn, i)
			}
			toolResult, awaiting := s.executeToolCall(sess, tc, call, limits.ToolTimeout)
			if awaiting {
				// Phase D: a proposal tool moved the run to awaiting_approval.
				return sess.saveAwaitingApproval()
			}
			content := resultContent(toolResult)
			if err := sess.persistToolCallPart(partIndexOfCall(sess, call.ID), call, toolResult); err != nil {
				return sess.failRun("STATE_ERROR", err)
			}
			messages = append(messages, agent.ChatMessage{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: content})
		}
	}

	// Turn budget exhausted: end the run and tell the user (§6), not an error.
	idx, err := sess.addTextPart()
	if err != nil {
		return sess.failRun("STATE_ERROR", err)
	}
	notice := "（已达到单次运行的工具调用轮次上限，先在此收尾。如需继续，请再发一条消息。）"
	if err := sess.appendTextToPart(idx, notice); err != nil {
		return sess.failRun("STATE_ERROR", err)
	}
	if err := sess.persistTextPart(idx, notice); err != nil {
		return sess.failRun("STATE_ERROR", err)
	}
	return sess.succeed()
}

// executeToolCall validates and runs one tool call, streaming its tool-call part
// and result into the run state. It returns the result to feed back to the model
// and whether the call moved the run to awaiting_approval (phase D proposals).
func (s *AgentService) executeToolCall(sess *runSession, tc ToolContext, call agent.ToolCall, timeout time.Duration) (ToolResult, bool) {
	args := parseArgs(call.Arguments)
	partIdx, err := sess.addToolCallPart(call, args)
	if err != nil {
		return ToolResult{IsError: true, Text: "failed to record tool call"}, false
	}
	_ = partIdx

	tool, ok := s.tools.Get(call.Name)
	if !ok {
		return s.finishToolCall(sess, call, partIdx, toolError("unknown tool %q", call.Name)), false
	}
	if args == nil {
		return s.finishToolCall(sess, call, partIdx, toolError("arguments are not a valid JSON object")), false
	}
	if problems := s.tools.ValidateArgs(call.Name, args); len(problems) > 0 {
		return s.finishToolCall(sess, call, partIdx, ToolResult{IsError: true, Text: "invalid arguments: " + strings.Join(problems, "; ")}), false
	}

	// Both readonly and proposal tools execute OUTSIDE any business transaction
	// (§5.2) with a per-call timeout (§6). A proposal tool writes only the
	// Proposal staging record, never a business table (invariant 1).
	toolCtx, cancel := context.WithTimeout(sess.ctx, timeout)
	defer cancel()
	result, execErr := tool.Execute(toolCtx, tc, args)
	if execErr != nil {
		if toolCtx.Err() == context.DeadlineExceeded {
			result = toolError("tool %q timed out after %s", call.Name, timeout)
		} else {
			result = ToolResult{IsError: true, Text: fmt.Sprintf("tool %q failed: %v", call.Name, execErr)}
		}
	}

	// A proposal tool that created a pending Proposal pauses the run for user
	// approval (§6, §7): emit the approval part, surface it in fasttask state,
	// and signal awaiting so the loop transitions to awaiting_approval.
	if result.Proposal != nil && !result.IsError {
		if err := sess.recordProposal(partIdx, call, result); err != nil {
			return s.finishToolCall(sess, call, partIdx, toolError("failed to record proposal: %v", err)), false
		}
		return result, true
	}
	return s.finishToolCall(sess, call, partIdx, result), false
}

// finishToolCall records the result on the tool-call part and returns it.
func (s *AgentService) finishToolCall(sess *runSession, call agent.ToolCall, partIdx int, result ToolResult) ToolResult {
	_ = sess.setToolCallResult(partIdx, result)
	return result
}

// recordProposal marks a tool-call part as awaiting approval, adds the proposal
// to the fasttask business state (§2.7), and persists the part with its proposal
// link so the approval receipt can find it (§7).
func (s *runSession) recordProposal(partIdx int, call agent.ToolCall, result ToolResult) error {
	ref := result.Proposal
	part := &s.state.Messages[s.assistantIdx].Parts[partIdx]
	part.Result = result.Result
	part.Approval = &protocol.Approval{Status: protocol.ApprovalPending}
	if err := s.emitSet(protocol.PartPath(s.assistantIdx, partIdx), *part); err != nil {
		return err
	}
	s.state.FastTask.PendingProposals = append(s.state.FastTask.PendingProposals, protocol.PendingProposal{
		ID: ref.ProposalID, GoalID: ref.GoalID, BaseRevision: ref.BaseRevision, Summary: ref.Summary,
	})
	if err := s.emitSet(protocol.FastTaskPath(), s.state.FastTask); err != nil {
		return err
	}
	resultJSON, _ := json.Marshal(result.Result)
	toolCallID := call.ID
	proposalID := ref.ProposalID
	return s.svc.repo.CreatePart(s.ctx, &persistence.AgentMessagePart{
		UserID: s.userID, MessageID: s.assistantID, Idx: partIdx, Type: "tool-call",
		ToolCallID: &toolCallID, ToolName: call.Name, ArgsJSON: normalizeArgs(call.Arguments),
		ResultJSON: string(resultJSON), ApprovalStatus: protocol.ApprovalPending, ProposalID: &proposalID,
	})
}

// saveAwaitingApproval persists state and flips the run to awaiting_approval
// (phase D). The HTTP stream ends with [DONE]; the run holds no lease (§4.0).
func (s *runSession) saveAwaitingApproval() (map[string]any, error) {
	requires := protocol.RequiresActionStatus()
	s.state.Messages[s.assistantIdx].Status = requires
	_ = s.emitSet(protocol.MessageStatusPath(s.assistantIdx), requires)
	s.state.IsRunning = false
	_ = s.emitSet(protocol.IsRunningPath(), false)
	if err := s.saveState(); err != nil {
		return nil, err
	}
	if err := s.svc.repo.SetRunStatus(s.ctx, s.userID, s.run.ID, persistence.RunAwaitingApproval, "", ""); err != nil {
		return nil, err
	}
	return map[string]any{"run_id": s.run.ID, "status": persistence.RunAwaitingApproval, "chunks": s.sink.emitted}, nil
}

// resumeRun rebuilds a session for a run continuing after an approval receipt
// (§7.2): it reuses the SAME assistant message and message index, replays the
// thread into authoritative state, and reconstructs the model context from the
// persisted parts so the loop can carry on from the resolved tool call.
func (s *AgentService) resumeRun(ctx context.Context, job persistence.AgentJob, run *persistence.AgentRun, assistant persistence.AgentMessage) (*runSession, error) {
	sink := &dbSink{repo: s.repo, userID: run.UserID, run: run.ID}
	sess := &runSession{
		sessionStream: &sessionStream{svc: s, ctx: ctx, userID: run.UserID, run: run, sink: sink, state: protocol.NewState()},
		jobID:         job.ID,
		resumed:       true,
	}
	sess.state.IsRunning = true
	sess.state.FastTask.ThreadID = run.ThreadID

	if err := sess.emitSet(protocol.IsRunningPath(), true); err != nil {
		return nil, err
	}

	existing, err := s.repo.ListThreadMessages(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return nil, err
	}
	for i, m := range existing {
		status := protocol.CompleteStatus("")
		if m.ID == assistant.ID {
			// The reused assistant message goes back to running while it continues.
			status = protocol.RunningStatus()
			sess.assistantID = m.ID
			sess.assistantIdx = i
		}
		pm, err := s.toProtocolMessage(ctx, run.UserID, m, status)
		if err != nil {
			return nil, err
		}
		sess.state.Messages = append(sess.state.Messages, pm)
		if err := sess.emitSet(protocol.MessagePath(i), pm); err != nil {
			return nil, err
		}
	}
	if sess.assistantID == "" {
		return nil, errors.New("resume could not locate the assistant message")
	}
	// Re-emit the (now updated) fasttask state so resolved proposals drop off the
	// pending list on the client.
	sess.state.FastTask.PendingProposals = s.pendingProposals(ctx, run.UserID, run.ThreadID)
	if err := sess.emitSet(protocol.FastTaskPath(), sess.state.FastTask); err != nil {
		return nil, err
	}
	sess.userText = lastUserText(sess.state.Messages)

	// Reconstruct the model conversation from persisted parts: prior turns become
	// assistant/tool messages so the model sees the approval outcome and continues.
	sess.resumeContext = s.rebuildModelContext(ctx, run.UserID, sess.state.Messages, sess.assistantIdx)
	return sess, nil
}

// pendingProposals lists proposals still awaiting approval across the thread's
// runs, so a resumed run's fasttask state reflects only what is truly pending.
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

// rebuildModelContext converts the wire messages before and including the reused
// assistant message into model turns. Text parts become assistant content; each
// tool-call part with a recorded result becomes an assistant tool_call plus a
// tool message, so the model resumes with the approval outcome in context (§7).
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

// partIndexOfCall finds the assistant-message part index for a tool call id.
func partIndexOfCall(sess *runSession, callID string) int {
	for i, part := range sess.assistantParts() {
		if part.Type == protocol.PartToolCall && part.ToolCallID == callID {
			return i
		}
	}
	return len(sess.assistantParts()) - 1
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
