package application

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// A test host for the harness (doc/harness.md §3, §14).
//
// The point of this file is that tests drive a run the way a real host does — through the
// capability token, the model proxy, the tool execution surface and the completion report —
// instead of calling an in-process loop that no longer exists. The model itself is a scripted
// OpenAI-compatible SSE server, so the proxy's parsing, its credential injection and its
// transcript writing are all exercised for real.

// scriptedTurn is one model call the fake upstream answers with.
type scriptedTurn struct {
	text      string
	reasoning string
	toolCalls []agent.ToolCall
	// status and body make the upstream fail instead of streaming: a 400 whose body mentions
	// unsupported tools is how PROVIDER_NO_TOOL_SUPPORT is detected.
	status int
	body   string
	// drop aborts the connection mid-stream, which is how a transport failure looks to the
	// proxy.
	drop bool
}

// fakeUpstream is a scripted OpenAI-compatible endpoint. It records every request the proxy
// sent, which is what lets a test assert the host's system prompt and tool list were replaced.
type fakeUpstream struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	turns    []scriptedTurn
	calls    int
	chunks   int
	requests []map[string]any
	headers  []http.Header

	// chunkDelay slows the response down, one chunk at a time. It is the only way to observe a
	// cancellation while the model is still producing text (§5.2's second channel): a fixture that writes
	// a whole turn at once has no "mid-stream", and the proxy only re-reads the run row every
	// cancelCheckInterval, so the stream has to outlive that interval to be interruptible at all.
	chunkDelay time.Duration
}

// chunksWritten is how many SSE chunks the upstream has flushed, for a test waiting on a stream to be
// genuinely in flight before it cancels.
func (f *fakeUpstream) chunksWritten() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks
}

func newFakeUpstream(t *testing.T, turns ...scriptedTurn) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{t: t, turns: turns}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// credentials are what the resolver hands the proxy. The API key must never reach a host
// (§4.5), which §14.17 asserts against these values.
func (f *fakeUpstream) credentials() *ModelCredentials {
	return &ModelCredentials{BaseURL: f.server.URL, Model: "qwen3.8-max", APIKey: "upstream-secret-key"}
}

func (f *fakeUpstream) resolver() CredentialsResolver {
	return func(context.Context) (*ModelCredentials, error) { return f.credentials(), nil }
}

// request returns the nth recorded request body.
func (f *fakeUpstream) request(t *testing.T, index int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.requests) {
		t.Fatalf("upstream received %d requests, want at least %d", len(f.requests), index+1)
	}
	return f.requests[index]
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)

	f.mu.Lock()
	index := f.calls
	f.calls++
	f.requests = append(f.requests, payload)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()

	turn := scriptedTurn{}
	if index < len(f.turns) {
		turn = f.turns[index]
	}
	if turn.drop {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			f.t.Error("test server cannot hijack a connection")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			f.t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
		return
	}
	if turn.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.status)
		_, _ = io.WriteString(w, turn.body)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		encoded, _ := json.Marshal(map[string]any{"choices": []any{choice}})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
		f.mu.Lock()
		f.chunks++
		f.mu.Unlock()
		if f.chunkDelay > 0 {
			select {
			case <-time.After(f.chunkDelay):
			case <-r.Context().Done():
				// The proxy cut us off, which is exactly what a cancellation looks like from upstream.
				return
			}
		}
	}
	if turn.reasoning != "" {
		for _, delta := range splitDeltas(turn.reasoning, 6) {
			write(map[string]any{"reasoning_content": delta}, "")
		}
	}
	if turn.text != "" {
		for _, delta := range splitDeltas(turn.text, 8) {
			write(map[string]any{"content": delta}, "")
		}
	}
	// Arguments are split across fragments the way a real provider sends them, and the id and
	// name arrive only on the first fragment of a call.
	for index, call := range turn.toolCalls {
		args := call.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		head, tail := args, ""
		if len(args) > 4 {
			head, tail = args[:len(args)/2], args[len(args)/2:]
		}
		write(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": head},
		}}}, "")
		if tail != "" {
			write(map[string]any{"tool_calls": []any{map[string]any{
				"index":    index,
				"function": map[string]any{"arguments": tail},
			}}}, "")
		}
	}
	finish := "stop"
	if len(turn.toolCalls) > 0 {
		finish = "tool_calls"
	}
	write(map[string]any{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// harnessFixture wires an AgentService whose model is the scripted upstream, so a run can be
// driven as a harness run.
func harnessFixture(t *testing.T, upstream *fakeUpstream, opts ...AgentOption) (fixture, *AgentService) {
	t.Helper()
	f := newFixture(t)
	base := []AgentOption{WithCredentialsResolver(upstream.resolver())}
	svc := NewAgentService(f.app, append(base, opts...)...)
	return f, svc
}

// submitHarnessRun creates a run the way a browser does: POST /agent/commands with
// harness_mode=wasm. It returns the run id; no AgentJob may exist for it (§1.2).
func submitHarnessRun(t *testing.T, svc *AgentService, store *persistence.Store, userID, text string) string {
	t.Helper()
	submitted, err := svc.SubmitCommands(context.Background(), userID, CommandsRequest{
		HarnessMode: HarnessModeWASM,
		Commands: []Command{{Type: "add-message", Message: &CommandMessage{
			Role: "user", Parts: []CommandPart{{Type: "text", Text: text}},
		}}},
	})
	if err != nil {
		t.Fatalf("SubmitCommands: %v", err)
	}
	if submitted.HarnessMode != HarnessModeWASM {
		t.Fatalf("harness mode=%q, want wasm", submitted.HarnessMode)
	}
	var jobs int64
	if err := store.DB.Model(&persistence.AgentJob{}).Where("subject_id = ?", submitted.RunID).Count(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("a wasm run created %d agent jobs; the worker would execute it a second time (§1.2)", jobs)
	}
	return submitted.RunID
}

// callCollector is the sink a test host uses to learn what the model asked for, from the stream
// it was handed rather than from the script — the same information a real host has.
type callCollector struct {
	// forwarded is every byte the proxy handed back, so a test can assert what a host can see
	// (§4.5: no credential may appear in it).
	forwarded string
	calls     []agent.ToolCall
	pending   map[int]*agent.ToolCall
	order     []int
	text      strings.Builder
	reasoning strings.Builder
	finish    string
}

func newCallCollector() *callCollector {
	return &callCollector{pending: map[int]*agent.ToolCall{}}
}

func (c *callCollector) TextDelta(_ context.Context, delta string) error {
	c.text.WriteString(delta)
	return nil
}

func (c *callCollector) ReasoningDelta(_ context.Context, delta string) error {
	c.reasoning.WriteString(delta)
	return nil
}

func (c *callCollector) ToolCallDelta(_ context.Context, delta agent.ToolCallDelta) error {
	slot := c.pending[delta.Index]
	if slot == nil {
		slot = &agent.ToolCall{}
		c.pending[delta.Index] = slot
		c.order = append(c.order, delta.Index)
	}
	if delta.ID != "" {
		slot.ID = delta.ID
	}
	if delta.Name != "" {
		slot.Name = delta.Name
	}
	slot.Arguments += delta.Arguments
	return nil
}

func (c *callCollector) Finish(_ context.Context, reason string) error {
	c.finish = reason
	return nil
}

func (c *callCollector) result() []agent.ToolCall {
	for _, index := range c.order {
		call := c.pending[index]
		if call == nil || call.Name == "" {
			continue
		}
		if strings.TrimSpace(call.Arguments) == "" {
			call.Arguments = "{}"
		}
		c.calls = append(c.calls, *call)
	}
	return c.calls
}

// testHost drives one run the way a libfx host does. It holds a real capability token and uses
// only the exported harness surface, so a test cannot accidentally reach into the server the way
// the old in-process loop tests did.
type testHost struct {
	t         *testing.T
	svc       *AgentService
	upstream  *fakeUpstream
	userID    string
	runID     string
	token     string
	grant     RunGrant
	principal HarnessPrincipal
	// decide answers an approval wait. It defaults to approve, and tests that exercise a
	// rejection replace it.
	decide func(toolCallID, proposalID string) approvalDecision
	// beats counts the heartbeats the host sent, so a test can assert the wait kept beating.
	beats int
}

func newTestHost(t *testing.T, svc *AgentService, upstream *fakeUpstream, userID, runID string) *testHost {
	t.Helper()
	ctx := context.Background()
	grant, err := svc.IssueRunGrant(ctx, userID, runID, "")
	if err != nil {
		t.Fatalf("IssueRunGrant: %v", err)
	}
	principal, err := svc.AuthenticateHarness(ctx, "Bearer "+grant.HarnessToken)
	if err != nil {
		t.Fatalf("AuthenticateHarness: %v", err)
	}
	host := &testHost{
		t: t, svc: svc, upstream: upstream, userID: userID, runID: runID,
		token: grant.HarnessToken, grant: grant, principal: principal,
		decide: func(string, string) approvalDecision { return approvalDecision{Decision: "approve"} },
	}
	return host
}

func (h *testHost) beat() {
	h.t.Helper()
	if _, err := h.svc.HarnessHeartbeat(context.Background(), h.principal); err != nil {
		h.t.Fatalf("heartbeat: %v", err)
	}
	h.beats++
}

// threadID is the thread the run belongs to, read from the run row: the server owns the thread identity,
// and a host learns it from the grant.
func (h *testHost) threadID() string {
	h.t.Helper()
	return h.runStatus().ThreadID
}

func (h *testHost) runStatus() persistence.AgentRun {
	h.t.Helper()
	run, err := h.svc.Repository().GetRun(context.Background(), h.userID, h.runID)
	if err != nil {
		h.t.Fatalf("GetRun: %v", err)
	}
	return *run
}

// hostRequestBody is what a host's shim sends. It deliberately carries a system prompt and a
// tool list of its own, both of which the proxy must discard (§4.3, §14.4).
func hostRequestBody(text string) []byte {
	payload := map[string]any{
		"model":  "host-picked-model",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "HOST INJECTED SYSTEM PROMPT"},
			map[string]any{"role": "user", "content": text},
		},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "delete_everything", "parameters": map[string]any{}},
		}},
		"tool_choice": "required",
		"api_key":     "host-should-not-send-this",
		"base_url":    "https://evil.example/v1",
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// modelCall performs one proxied model call and returns what the stream said.
func (h *testHost) modelCall(text string) (*callCollector, error) {
	return h.modelCallBody(hostRequestBody(text))
}

// modelCallBody performs one proxied model call with an explicit request body, and returns what
// the stream said. Reading the whole body is what writes the transcript, so a host that
// discarded it would lose the run's record (§4.4).
func (h *testHost) modelCallBody(body []byte) (*callCollector, error) {
	stream, err := h.svc.OpenModelProxy(context.Background(), h.principal, body)
	if err != nil {
		return nil, err
	}
	collector := newCallCollector()
	parser := agent.NewStreamParser(collector)
	buffer := make([]byte, 4096)
	var forwarded bytes.Buffer
	for {
		n, readErr := stream.Body.Read(buffer)
		if n > 0 {
			forwarded.Write(buffer[:n])
			if feedErr := parser.Feed(context.Background(), buffer[:n]); feedErr != nil {
				stream.Body.Close()
				collector.forwarded = forwarded.String()
				return collector, feedErr
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				stream.Body.Close()
				collector.forwarded = forwarded.String()
				return collector, readErr
			}
			break
		}
	}
	collector.forwarded = forwarded.String()
	if err := parser.Close(context.Background()); err != nil {
		stream.Body.Close()
		return collector, err
	}
	collector.result()
	if err := stream.Body.Close(); err != nil {
		return collector, err
	}
	return collector, nil
}

// drive runs turns until the model stops asking for tools, executing each call through the tool
// surface and waiting out any approval, then reports the turn as finished. It is the whole host
// loop, and it is what the ported loop tests now exercise.
func (h *testHost) drive(maxTurns int) {
	h.t.Helper()
	for turn := 0; turn < maxTurns; turn++ {
		if !h.driveOneTurn() {
			break
		}
	}
	h.complete("stop")
}

// driveOneTurn performs one model call, executes the tools it asked for and waits out any
// approval. It reports whether the model asked for tools at all, which is what tells a host its
// turn is over.
func (h *testHost) driveOneTurn() bool {
	h.t.Helper()
	outcomes := h.modelTurn()
	for _, outcome := range outcomes {
		if outcome.Status != "pending" {
			continue
		}
		// A proposal parks the run and the host waits for the user (§7). The decision is
		// delivered the way the UI delivers it, then read back off the long poll.
		h.resolveApproval(outcome.toolCallID, outcome.ProposalID)
		wait, err := h.svc.WaitForApproval(context.Background(), h.principal, outcome.ProposalID, 5*time.Second)
		if err != nil {
			h.t.Fatalf("WaitForApproval: %v", err)
		}
		if wait.Status != "approved" && wait.Status != "rejected" && wait.Status != "conflict" {
			h.t.Fatalf("approval wait ended as %q, want a decision", wait.Status)
		}
	}
	return len(outcomes) > 0
}

// modelTurn performs one model call and executes the tools it asked for, WITHOUT resolving any
// approval. Tests that assert on a parked run use this and decide it themselves.
func (h *testHost) modelTurn() []toolOutcome {
	h.t.Helper()
	h.beat()
	collector, err := h.modelCall("推进一下")
	if err != nil {
		var proxyErr *ProxyError
		if asProxy(err, &proxyErr) {
			h.t.Fatalf("model call refused: %s (%s)", proxyErr.Code, proxyErr.Detail)
		}
		h.t.Fatalf("model call: %v", err)
	}
	if len(collector.calls) == 0 {
		return nil
	}
	return h.executeCalls(collector.calls)
}

// toolOutcome pairs an execution result with the call it answered.
type toolOutcome struct {
	HarnessToolOutcome
	toolCallID string
	toolName   string
}

// executeCalls runs every call the model asked for through the tool surface.
func (h *testHost) executeCalls(calls []agent.ToolCall) []toolOutcome {
	h.t.Helper()
	outcomes := make([]toolOutcome, 0, len(calls))
	for _, call := range calls {
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			args = map[string]any{}
		}
		outcome, err := h.svc.ExecuteHarnessTool(context.Background(), h.principal, call.Name,
			HarnessToolCall{ToolCallID: call.ID, Input: args})
		if err != nil {
			h.t.Fatalf("ExecuteHarnessTool(%s): %v", call.Name, err)
		}
		outcomes = append(outcomes, toolOutcome{HarnessToolOutcome: outcome, toolCallID: call.ID, toolName: call.Name})
	}
	return outcomes
}

// resolveApproval delivers a user's decision the way the UI does: an add-tool-result command
// (doc/agent-impl.md §7.1). It must not create a run or a job (§7).
func (h *testHost) resolveApproval(toolCallID, proposalID string) {
	h.t.Helper()
	decision := h.decide(toolCallID, proposalID)
	encoded, _ := json.Marshal(decision)
	result, err := h.svc.ResolveApproval(context.Background(), h.userID, Command{
		Type: "add-tool-result", ToolCallID: toolCallID, ToolName: "proposal",
		Result: encoded,
	})
	if err != nil {
		h.t.Fatalf("ResolveApproval: %v", err)
	}
	if !result.NoStream {
		h.t.Fatal("a harness approval receipt opened a second stream; the turn continues on the open one (§7)")
	}
	if result.RunID != h.runID {
		h.t.Fatalf("approval resolved run %q, want the same run %q", result.RunID, h.runID)
	}
}

// complete reports the end of the turn, storing the checkpoint (§5.1, §6.1).
func (h *testHost) complete(stopReason string) {
	h.t.Helper()
	err := h.svc.CompleteRun(context.Background(), h.principal, Completion{
		StopReason: stopReason, Checkpoint: []byte("checkpoint-bytes"), LibfxVersion: LibfxVersion,
	})
	if err != nil {
		h.t.Fatalf("CompleteRun: %v", err)
	}
}

// asProxy is errors.As for *ProxyError, kept short for test readability.
func asProxy(err error, target **ProxyError) bool {
	proxyErr, ok := err.(*ProxyError)
	if ok {
		*target = proxyErr
	}
	return ok
}

// slowTool blocks longer than the configured tool timeout.
type slowTool struct{ delay time.Duration }

func (slowTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "slow_tool", Description: "slow", Parameters: objectSchema(nil)}
}
func (slowTool) Level() ToolLevel { return ToolReadonly }
func (s slowTool) Execute(ctx context.Context, _ ToolContext, _ map[string]any) (ToolResult, error) {
	select {
	case <-time.After(s.delay):
		return ToolResult{Result: map[string]any{"ok": true}}, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

// Two model calls in one run, each with a chain of thought.
//
// A reasoning part's id is its primary key, and it used to be numbered only by sequence within one call,
// so the second call of a run claimed the first call's key. The insert failed, the transcribing copy
// aborted mid-stream, and the host saw a response that stopped early — which libfx retries. The run then
// burned its whole turn budget and "succeeded" with the budget notice instead of the answer the model had
// already given, with nothing in any log to explain it (§4.4.1).
func TestReasoningAcrossModelCallsKeepsItsOwnParts(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{
			reasoning: "先查活跃目标",
			toolCalls: []agent.ToolCall{{ID: "call_reason_1", Name: "list_goals", Arguments: `{"status":"active"}`}},
		},
		scriptedTurn{reasoning: "再报标题", text: "你有一个活跃目标。"},
	)
	f := newFixture(t)
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()))
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "我有哪些目标")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)
	host.drive(4)

	status := runStatus(t, svc, f.user.ID, runID)
	if status.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q (%s/%s), want succeeded", status.Status, status.ErrorCode, status.ErrorMessage)
	}
	if status.ModelCalls != 2 {
		t.Fatalf("model calls=%d, want 2 — a call that had to be retried means the transcript broke", status.ModelCalls)
	}

	parts := assistantParts(t, svc, f.user.ID, runID)
	seen := map[string]bool{}
	var reasoning []string
	for _, part := range parts {
		if part.Type != "reasoning" {
			continue
		}
		if seen[part.ID] {
			t.Fatalf("reasoning part %q was written twice", part.ID)
		}
		seen[part.ID] = true
		reasoning = append(reasoning, strings.TrimSpace(part.Text))
	}
	if len(reasoning) != 2 {
		t.Fatalf("reasoning parts=%d %v, want one per model call", len(reasoning), reasoning)
	}
	if reasoning[0] != "先查活跃目标" || reasoning[1] != "再报标题" {
		t.Fatalf("reasoning=%v, want each call's own chain of thought in order", reasoning)
	}
	if got := strings.TrimSpace(concatText(parts)); got != "你有一个活跃目标。" {
		t.Fatalf("assistant text=%q, want the second call's answer", got)
	}
	var toolResult string
	for _, part := range parts {
		if part.Type == "tool-call" {
			toolResult = part.ResultJSON
		}
	}
	if strings.TrimSpace(toolResult) == "" {
		t.Fatalf("the tool call has no recorded result; parts=%+v", parts)
	}
}
